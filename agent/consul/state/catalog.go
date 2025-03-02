package state

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/go-uuid"
	"github.com/mitchellh/copystructure"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/connect"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/consul/types"
)

// indexServiceExtinction keeps track of the last raft index when the last instance
// of any service was unregistered. This is used by blocking queries on missing services.
const indexServiceExtinction = "service_last_extinction"

const (
	// minUUIDLookupLen is used as a minimum length of a node name required before
	// we test to see if the name is actually a UUID and perform an ID-based node
	// lookup.
	minUUIDLookupLen = 2
)

// Query is type used to query any single value index that may include an
// enterprise identifier.
type Query struct {
	Value string
	structs.EnterpriseMeta
}

func resizeNodeLookupKey(s string) string {
	l := len(s)

	if l%2 != 0 {
		return s[0 : l-1]
	}

	return s
}

// Nodes is used to pull the full list of nodes for use during snapshots.
func (s *Snapshot) Nodes() (memdb.ResultIterator, error) {
	iter, err := s.tx.Get(tableNodes, indexID)
	if err != nil {
		return nil, err
	}
	return iter, nil
}

// Services is used to pull the full list of services for a given node for use
// during snapshots.
func (s *Snapshot) Services(node string) (memdb.ResultIterator, error) {
	iter, err := catalogServiceListByNode(s.tx, node, structs.WildcardEnterpriseMeta(), true)
	if err != nil {
		return nil, err
	}
	return iter, nil
}

// Checks is used to pull the full list of checks for a given node for use
// during snapshots.
func (s *Snapshot) Checks(node string) (memdb.ResultIterator, error) {
	iter, err := catalogListChecksByNode(s.tx, node, structs.WildcardEnterpriseMeta())
	if err != nil {
		return nil, err
	}
	return iter, nil
}

// Registration is used to make sure a node, service, and check registration is
// performed within a single transaction to avoid race conditions on state
// updates.
func (s *Restore) Registration(idx uint64, req *structs.RegisterRequest) error {
	return s.store.ensureRegistrationTxn(s.tx, idx, true, req, true)
}

// EnsureRegistration is used to make sure a node, service, and check
// registration is performed within a single transaction to avoid race
// conditions on state updates.
func (s *Store) EnsureRegistration(idx uint64, req *structs.RegisterRequest) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	if err := s.ensureRegistrationTxn(tx, idx, false, req, false); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) ensureCheckIfNodeMatches(tx WriteTxn, idx uint64, preserveIndexes bool, node string, check *structs.HealthCheck) error {
	if check.Node != node {
		return fmt.Errorf("check node %q does not match node %q",
			check.Node, node)
	}
	if err := s.ensureCheckTxn(tx, idx, preserveIndexes, check); err != nil {
		return fmt.Errorf("failed inserting check: %s on node %q", err, check.Node)
	}
	return nil
}

// ensureRegistrationTxn is used to make sure a node, service, and check
// registration is performed within a single transaction to avoid race
// conditions on state updates.
func (s *Store) ensureRegistrationTxn(tx WriteTxn, idx uint64, preserveIndexes bool, req *structs.RegisterRequest, restore bool) error {
	if _, err := validateRegisterRequestTxn(tx, req, restore); err != nil {
		return err
	}

	// Create a node structure.
	node := &structs.Node{
		ID:              req.ID,
		Node:            req.Node,
		Address:         req.Address,
		Datacenter:      req.Datacenter,
		TaggedAddresses: req.TaggedAddresses,
		Meta:            req.NodeMeta,
	}
	if preserveIndexes {
		node.CreateIndex = req.CreateIndex
		node.ModifyIndex = req.ModifyIndex
	}

	// Since this gets called for all node operations (service and check
	// updates) and churn on the node itself is basically none after the
	// node updates itself the first time, it's worth seeing if we need to
	// modify the node at all so we prevent watch churn and useless writes
	// and modify index bumps on the node.
	{
		existing, err := tx.First(tableNodes, indexID, Query{Value: node.Node})
		if err != nil {
			return fmt.Errorf("node lookup failed: %s", err)
		}
		if existing == nil || req.ChangesNode(existing.(*structs.Node)) {
			if err := s.ensureNodeTxn(tx, idx, preserveIndexes, node); err != nil {
				return fmt.Errorf("failed inserting node: %s", err)
			}
		}
	}

	// Add the service, if any. We perform a similar check as we do for the
	// node info above to make sure we actually need to update the service
	// definition in order to prevent useless churn if nothing has changed.
	if req.Service != nil {
		_, existing, err := firstWatchCompoundWithTxn(tx, "services", "id", &req.Service.EnterpriseMeta, req.Node, req.Service.ID)
		if err != nil {
			return fmt.Errorf("failed service lookup: %s", err)
		}
		if existing == nil || !(existing.(*structs.ServiceNode).ToNodeService()).IsSame(req.Service) {
			if err := ensureServiceTxn(tx, idx, req.Node, preserveIndexes, req.Service); err != nil {
				return fmt.Errorf("failed inserting service: %s", err)

			}
		}
	}

	// Add the checks, if any.
	if req.Check != nil {
		if err := s.ensureCheckIfNodeMatches(tx, idx, preserveIndexes, req.Node, req.Check); err != nil {
			return err
		}
	}
	for _, check := range req.Checks {
		if err := s.ensureCheckIfNodeMatches(tx, idx, preserveIndexes, req.Node, check); err != nil {
			return err
		}
	}

	return nil
}

// EnsureNode is used to upsert node registration or modification.
func (s *Store) EnsureNode(idx uint64, node *structs.Node) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	// Call the node upsert
	if err := s.ensureNodeTxn(tx, idx, false, node); err != nil {
		return err
	}

	return tx.Commit()
}

// ensureNoNodeWithSimilarNameTxn checks that no other node has conflict in its name
// If allowClashWithoutID then, getting a conflict on another node without ID will be allowed
func ensureNoNodeWithSimilarNameTxn(tx ReadTxn, node *structs.Node, allowClashWithoutID bool) error {
	// Retrieve all of the nodes
	enodes, err := tx.Get(tableNodes, indexID)
	if err != nil {
		return fmt.Errorf("Cannot lookup all nodes: %s", err)
	}
	for nodeIt := enodes.Next(); nodeIt != nil; nodeIt = enodes.Next() {
		enode := nodeIt.(*structs.Node)
		if strings.EqualFold(node.Node, enode.Node) && node.ID != enode.ID {
			// Look up the existing node's Serf health check to see if it's failed.
			// If it is, the node can be renamed.
			_, enodeCheck, err := firstWatchCompoundWithTxn(tx, "checks", "id", structs.DefaultEnterpriseMeta(), enode.Node, string(structs.SerfCheckID))
			if err != nil {
				return fmt.Errorf("Cannot get status of node %s: %s", enode.Node, err)
			}

			// Get the node health. If there's no Serf health check, we consider it safe to rename
			// the node as it's likely an external node registration not managed by Consul.
			var nodeHealthy bool
			if enodeCheck != nil {
				enodeSerfCheck, ok := enodeCheck.(*structs.HealthCheck)
				if ok {
					nodeHealthy = enodeSerfCheck.Status != api.HealthCritical
				}
			}

			if !(enode.ID == "" && allowClashWithoutID) && nodeHealthy {
				return fmt.Errorf("Node name %s is reserved by node %s with name %s (%s)", node.Node, enode.ID, enode.Node, enode.Address)
			}
		}
	}
	return nil
}

// ensureNodeCASTxn updates a node only if the existing index matches the given index.
// Returns a bool indicating if a write happened and any error.
func (s *Store) ensureNodeCASTxn(tx WriteTxn, idx uint64, node *structs.Node) (bool, error) {
	// Retrieve the existing entry.
	existing, err := getNodeTxn(tx, node.Node)
	if err != nil {
		return false, err
	}

	// Check if the we should do the set. A ModifyIndex of 0 means that
	// we are doing a set-if-not-exists.
	if node.ModifyIndex == 0 && existing != nil {
		return false, nil
	}
	if node.ModifyIndex != 0 && existing == nil {
		return false, nil
	}
	if existing != nil && node.ModifyIndex != 0 && node.ModifyIndex != existing.ModifyIndex {
		return false, nil
	}

	// Perform the update.
	if err := s.ensureNodeTxn(tx, idx, false, node); err != nil {
		return false, err
	}

	return true, nil
}

// ensureNodeTxn is the inner function called to actually create a node
// registration or modify an existing one in the state store. It allows
// passing in a memdb transaction so it may be part of a larger txn.
func (s *Store) ensureNodeTxn(tx WriteTxn, idx uint64, preserveIndexes bool, node *structs.Node) error {
	// See if there's an existing node with this UUID, and make sure the
	// name is the same.
	var n *structs.Node
	if node.ID != "" {
		existing, err := getNodeIDTxn(tx, node.ID)
		if err != nil {
			return fmt.Errorf("node lookup failed: %s", err)
		}
		if existing != nil {
			n = existing
			if n.Node != node.Node {
				// Lets first get all nodes and check whether name do match, we do not allow clash on nodes without ID
				dupNameError := ensureNoNodeWithSimilarNameTxn(tx, node, false)
				if dupNameError != nil {
					return fmt.Errorf("Error while renaming Node ID: %q (%s): %s", node.ID, node.Address, dupNameError)
				}
				// We are actually renaming a node, remove its reference first
				err := s.deleteNodeTxn(tx, idx, n.Node)
				if err != nil {
					return fmt.Errorf("Error while renaming Node ID: %q (%s) from %s to %s",
						node.ID, node.Address, n.Node, node.Node)
				}
			}
		} else {
			// We allow to "steal" another node name that would have no ID
			// It basically means that we allow upgrading a node without ID and add the ID
			dupNameError := ensureNoNodeWithSimilarNameTxn(tx, node, true)
			if dupNameError != nil {
				return fmt.Errorf("Error while renaming Node ID: %q: %s", node.ID, dupNameError)
			}
		}
	}
	// TODO: else Node.ID == "" should be forbidden in future Consul releases
	// See https://github.com/hashicorp/consul/pull/3983 for context

	// Check for an existing node by name to support nodes with no IDs.
	if n == nil {
		existing, err := tx.First(tableNodes, indexID, Query{Value: node.Node})
		if err != nil {
			return fmt.Errorf("node name lookup failed: %s", err)
		}

		if existing != nil {
			n = existing.(*structs.Node)
		}
		// WARNING, for compatibility reasons with tests, we do not check
		// for case insensitive matches, which may lead to DB corruption
		// See https://github.com/hashicorp/consul/pull/3983 for context
	}

	// Get the indexes.
	if n != nil {
		node.CreateIndex = n.CreateIndex
		node.ModifyIndex = n.ModifyIndex
		// We do not need to update anything
		if node.IsSame(n) {
			return nil
		}
		node.ModifyIndex = idx
	} else if !preserveIndexes || node.CreateIndex == 0 {
		// If this isn't a snapshot or there were no saved indexes, set CreateIndex
		// and ModifyIndex from the given index. Prior to 1.9.0/1.8.3/1.7.7, nodes
		// were not saved with an index, so this is to avoid ending up with a 0 index
		// when loading a snapshot from an older version.
		node.CreateIndex = idx
		node.ModifyIndex = idx
	}

	// Insert the node and update the index.
	if err := tx.Insert("nodes", node); err != nil {
		return fmt.Errorf("failed inserting node: %s", err)
	}
	if err := tx.Insert(tableIndex, &IndexEntry{"nodes", idx}); err != nil {
		return fmt.Errorf("failed updating index: %s", err)
	}
	// Update the node's service indexes as the node information is included
	// in health queries and we would otherwise miss node updates in some cases
	// for those queries.
	if err := updateAllServiceIndexesOfNode(tx, idx, node.Node); err != nil {
		return fmt.Errorf("failed updating index: %s", err)
	}

	return nil
}

// GetNode is used to retrieve a node registration by node name ID.
func (s *Store) GetNode(id string) (uint64, *structs.Node, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := maxIndexTxn(tx, "nodes")

	// Retrieve the node from the state store
	node, err := getNodeTxn(tx, id)
	if err != nil {
		return 0, nil, fmt.Errorf("node lookup failed: %s", err)
	}
	return idx, node, nil
}

func getNodeTxn(tx ReadTxn, nodeName string) (*structs.Node, error) {
	node, err := tx.First(tableNodes, indexID, Query{Value: nodeName})
	if err != nil {
		return nil, fmt.Errorf("node lookup failed: %s", err)
	}
	if node != nil {
		return node.(*structs.Node), nil
	}
	return nil, nil
}

func getNodeIDTxn(tx ReadTxn, id types.NodeID) (*structs.Node, error) {
	strnode := string(id)
	uuidValue, err := uuid.ParseUUID(strnode)
	if err != nil {
		return nil, fmt.Errorf("node lookup by ID failed, wrong UUID: %v for '%s'", err, strnode)
	}

	node, err := tx.First("nodes", "uuid", uuidValue)
	if err != nil {
		return nil, fmt.Errorf("node lookup by ID failed: %s", err)
	}
	if node != nil {
		return node.(*structs.Node), nil
	}
	return nil, nil
}

// GetNodeID is used to retrieve a node registration by node ID.
func (s *Store) GetNodeID(id types.NodeID) (uint64, *structs.Node, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := maxIndexTxn(tx, "nodes")

	// Retrieve the node from the state store
	node, err := getNodeIDTxn(tx, id)
	return idx, node, err
}

// Nodes is used to return all of the known nodes.
func (s *Store) Nodes(ws memdb.WatchSet) (uint64, structs.Nodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := maxIndexTxn(tx, "nodes")

	// Retrieve all of the nodes
	nodes, err := tx.Get(tableNodes, indexID)
	if err != nil {
		return 0, nil, fmt.Errorf("failed nodes lookup: %s", err)
	}
	ws.Add(nodes.WatchCh())

	// Create and return the nodes list.
	var results structs.Nodes
	for node := nodes.Next(); node != nil; node = nodes.Next() {
		results = append(results, node.(*structs.Node))
	}
	return idx, results, nil
}

// NodesByMeta is used to return all nodes with the given metadata key/value pairs.
func (s *Store) NodesByMeta(ws memdb.WatchSet, filters map[string]string) (uint64, structs.Nodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := maxIndexTxn(tx, "nodes")

	// Retrieve all of the nodes
	var args []interface{}
	for key, value := range filters {
		args = append(args, key, value)
		break
	}
	nodes, err := tx.Get("nodes", "meta", args...)
	if err != nil {
		return 0, nil, fmt.Errorf("failed nodes lookup: %s", err)
	}
	ws.Add(nodes.WatchCh())

	// Create and return the nodes list.
	var results structs.Nodes
	for node := nodes.Next(); node != nil; node = nodes.Next() {
		n := node.(*structs.Node)
		if len(filters) <= 1 || structs.SatisfiesMetaFilters(n.Meta, filters) {
			results = append(results, n)
		}
	}
	return idx, results, nil
}

// DeleteNode is used to delete a given node by its ID.
func (s *Store) DeleteNode(idx uint64, nodeName string) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	// Call the node deletion.
	if err := s.deleteNodeTxn(tx, idx, nodeName); err != nil {
		return err
	}

	return tx.Commit()
}

// deleteNodeCASTxn is used to try doing a node delete operation with a given
// raft index. If the CAS index specified is not equal to the last observed index for
// the given check, then the call is a noop, otherwise a normal check delete is invoked.
func (s *Store) deleteNodeCASTxn(tx WriteTxn, idx, cidx uint64, nodeName string) (bool, error) {
	// Look up the node.
	node, err := getNodeTxn(tx, nodeName)
	if err != nil {
		return false, err
	}
	if node == nil {
		return false, nil
	}

	// If the existing index does not match the provided CAS
	// index arg, then we shouldn't update anything and can safely
	// return early here.
	if node.ModifyIndex != cidx {
		return false, nil
	}

	// Call the actual deletion if the above passed.
	if err := s.deleteNodeTxn(tx, idx, nodeName); err != nil {
		return false, err
	}

	return true, nil
}

// deleteNodeTxn is the inner method used for removing a node from
// the store within a given transaction.
func (s *Store) deleteNodeTxn(tx WriteTxn, idx uint64, nodeName string) error {
	// Look up the node.
	node, err := tx.First(tableNodes, indexID, Query{Value: nodeName})
	if err != nil {
		return fmt.Errorf("node lookup failed: %s", err)
	}
	if node == nil {
		return nil
	}

	// Delete all services associated with the node and update the service index.
	services, err := tx.Get("services", "node", nodeName)
	if err != nil {
		return fmt.Errorf("failed service lookup: %s", err)
	}
	var deleteServices []*structs.ServiceNode
	for service := services.Next(); service != nil; service = services.Next() {
		svc := service.(*structs.ServiceNode)
		deleteServices = append(deleteServices, svc)

		if err := catalogUpdateServiceIndexes(tx, svc.ServiceName, idx, &svc.EnterpriseMeta); err != nil {
			return err
		}
		if err := catalogUpdateServiceKindIndexes(tx, svc.ServiceKind, idx, &svc.EnterpriseMeta); err != nil {
			return err
		}
	}

	// Do the delete in a separate loop so we don't trash the iterator.
	for _, svc := range deleteServices {
		if err := s.deleteServiceTxn(tx, idx, nodeName, svc.ServiceID, &svc.EnterpriseMeta); err != nil {
			return err
		}
	}

	// Delete all checks associated with the node. This will invalidate
	// sessions as necessary.
	checks, err := tx.Get("checks", "node", nodeName)
	if err != nil {
		return fmt.Errorf("failed check lookup: %s", err)
	}
	var deleteChecks []*structs.HealthCheck
	for check := checks.Next(); check != nil; check = checks.Next() {
		deleteChecks = append(deleteChecks, check.(*structs.HealthCheck))
	}

	// Do the delete in a separate loop so we don't trash the iterator.
	for _, chk := range deleteChecks {
		if err := s.deleteCheckTxn(tx, idx, nodeName, chk.CheckID, &chk.EnterpriseMeta); err != nil {
			return err
		}
	}

	// Delete any coordinates associated with this node.
	coords, err := tx.Get("coordinates", "node", nodeName)
	if err != nil {
		return fmt.Errorf("failed coordinate lookup: %s", err)
	}
	var coordsToDelete []interface{}
	for coord := coords.Next(); coord != nil; coord = coords.Next() {
		coordsToDelete = append(coordsToDelete, coord)
	}
	for _, coord := range coordsToDelete {
		if err := tx.Delete("coordinates", coord); err != nil {
			return fmt.Errorf("failed deleting coordinate: %s", err)
		}
		if err := tx.Insert(tableIndex, &IndexEntry{"coordinates", idx}); err != nil {
			return fmt.Errorf("failed updating index: %s", err)
		}
	}

	// Delete the node and update the index.
	if err := tx.Delete("nodes", node); err != nil {
		return fmt.Errorf("failed deleting node: %s", err)
	}
	if err := tx.Insert(tableIndex, &IndexEntry{"nodes", idx}); err != nil {
		return fmt.Errorf("failed updating index: %s", err)
	}

	// Invalidate any sessions for this node.
	toDelete, err := allNodeSessionsTxn(tx, nodeName)
	if err != nil {
		return err
	}

	for _, session := range toDelete {
		if err := s.deleteSessionTxn(tx, idx, session.ID, &session.EnterpriseMeta); err != nil {
			return fmt.Errorf("failed to delete session '%s': %v", session.ID, err)
		}
	}

	return nil
}

// EnsureService is called to upsert creation of a given NodeService.
func (s *Store) EnsureService(idx uint64, node string, svc *structs.NodeService) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	// Call the service registration upsert
	if err := ensureServiceTxn(tx, idx, node, false, svc); err != nil {
		return err
	}

	return tx.Commit()
}

var errCASCompareFailed = errors.New("compare-and-set: comparison failed")

// ensureServiceCASTxn updates a service only if the existing index matches the given index.
// Returns an error if the write didn't happen and nil if write was successful.
func ensureServiceCASTxn(tx WriteTxn, idx uint64, node string, svc *structs.NodeService) error {
	// Retrieve the existing service.
	_, existing, err := firstWatchCompoundWithTxn(tx, "services", "id", &svc.EnterpriseMeta, node, svc.ID)
	if err != nil {
		return fmt.Errorf("failed service lookup: %s", err)
	}

	// Check if the we should do the set. A ModifyIndex of 0 means that
	// we are doing a set-if-not-exists.
	if svc.ModifyIndex == 0 && existing != nil {
		return errCASCompareFailed
	}
	if svc.ModifyIndex != 0 && existing == nil {
		return errCASCompareFailed
	}
	e, ok := existing.(*structs.ServiceNode)
	if ok && svc.ModifyIndex != 0 && svc.ModifyIndex != e.ModifyIndex {
		return errCASCompareFailed
	}

	return ensureServiceTxn(tx, idx, node, false, svc)
}

// ensureServiceTxn is used to upsert a service registration within an
// existing memdb transaction.
func ensureServiceTxn(tx WriteTxn, idx uint64, node string, preserveIndexes bool, svc *structs.NodeService) error {
	// Check for existing service
	_, existing, err := firstWatchCompoundWithTxn(tx, "services", "id", &svc.EnterpriseMeta, node, svc.ID)
	if err != nil {
		return fmt.Errorf("failed service lookup: %s", err)
	}

	if err = structs.ValidateServiceMetadata(svc.Kind, svc.Meta, false); err != nil {
		return fmt.Errorf("Invalid Service Meta for node %s and serviceID %s: %v", node, svc.ID, err)
	}

	// Check if this service is covered by a gateway's wildcard specifier
	if err = checkGatewayWildcardsAndUpdate(tx, idx, svc); err != nil {
		return fmt.Errorf("failed updating gateway mapping: %s", err)
	}
	// Update upstream/downstream mappings if it's a connect service
	if svc.Kind == structs.ServiceKindConnectProxy {
		if err = updateMeshTopology(tx, idx, node, svc, existing); err != nil {
			return fmt.Errorf("failed updating upstream/downstream association")
		}
	}

	// Create the service node entry and populate the indexes. Note that
	// conversion doesn't populate any of the node-specific information.
	// That's always populated when we read from the state store.
	entry := svc.ToServiceNode(node)
	// Get the node
	n, err := tx.First(tableNodes, indexID, Query{Value: node})
	if err != nil {
		return fmt.Errorf("failed node lookup: %s", err)
	}
	if n == nil {
		return ErrMissingNode
	}
	if existing != nil {
		serviceNode := existing.(*structs.ServiceNode)
		entry.CreateIndex = serviceNode.CreateIndex
		entry.ModifyIndex = serviceNode.ModifyIndex
		// We cannot return here because: we want to keep existing behavior (ex: failed node lookup -> ErrMissingNode)
		// It might be modified in future, but it requires changing many unit tests
		// Enforcing saving the entry also ensures that if we add default values in .ToServiceNode()
		// those values will be saved even if node is not really modified for a while.
		if entry.IsSameService(serviceNode) {
			return nil
		}
	}
	if !preserveIndexes {
		entry.ModifyIndex = idx
		if existing == nil {
			entry.CreateIndex = idx
		}
	}

	// Insert the service and update the index
	return catalogInsertService(tx, entry)
}

// Services returns all services along with a list of associated tags.
func (s *Store) Services(ws memdb.WatchSet, entMeta *structs.EnterpriseMeta) (uint64, structs.Services, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogServicesMaxIndex(tx, entMeta)

	// List all the services.
	services, err := catalogServiceList(tx, entMeta, false)
	if err != nil {
		return 0, nil, fmt.Errorf("failed querying services: %s", err)
	}
	ws.Add(services.WatchCh())

	// Rip through the services and enumerate them and their unique set of
	// tags.
	unique := make(map[string]map[string]struct{})
	for service := services.Next(); service != nil; service = services.Next() {
		svc := service.(*structs.ServiceNode)
		tags, ok := unique[svc.ServiceName]
		if !ok {
			unique[svc.ServiceName] = make(map[string]struct{})
			tags = unique[svc.ServiceName]
		}
		for _, tag := range svc.ServiceTags {
			tags[tag] = struct{}{}
		}
	}

	// Generate the output structure.
	var results = make(structs.Services)
	for service, tags := range unique {
		results[service] = make([]string, 0, len(tags))
		for tag := range tags {
			results[service] = append(results[service], tag)
		}
	}
	return idx, results, nil
}

func (s *Store) ServiceList(ws memdb.WatchSet, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceList, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	return serviceListTxn(tx, ws, entMeta)
}

func serviceListTxn(tx ReadTxn, ws memdb.WatchSet, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceList, error) {
	idx := catalogServicesMaxIndex(tx, entMeta)

	services, err := catalogServiceList(tx, entMeta, true)
	if err != nil {
		return 0, nil, fmt.Errorf("failed querying services: %s", err)
	}
	ws.Add(services.WatchCh())

	unique := make(map[structs.ServiceName]struct{})
	for service := services.Next(); service != nil; service = services.Next() {
		svc := service.(*structs.ServiceNode)
		unique[svc.CompoundServiceName()] = struct{}{}
	}

	results := make(structs.ServiceList, 0, len(unique))
	for sn := range unique {
		results = append(results, structs.ServiceName{Name: sn.Name, EnterpriseMeta: sn.EnterpriseMeta})
	}

	return idx, results, nil
}

// ServicesByNodeMeta returns all services, filtered by the given node metadata.
func (s *Store) ServicesByNodeMeta(ws memdb.WatchSet, filters map[string]string, entMeta *structs.EnterpriseMeta) (uint64, structs.Services, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogServicesMaxIndex(tx, entMeta)
	if nodeIdx := maxIndexTxn(tx, "nodes"); nodeIdx > idx {
		idx = nodeIdx
	}

	// Retrieve all of the nodes with the meta k/v pair
	var args []interface{}
	for key, value := range filters {
		args = append(args, key, value)
		break
	}
	nodes, err := tx.Get("nodes", "meta", args...)
	if err != nil {
		return 0, nil, fmt.Errorf("failed nodes lookup: %s", err)
	}
	ws.Add(nodes.WatchCh())

	// We don't want to track an unlimited number of services, so we pull a
	// top-level watch to use as a fallback.
	allServices, err := catalogServiceList(tx, entMeta, false)
	if err != nil {
		return 0, nil, fmt.Errorf("failed services lookup: %s", err)
	}
	allServicesCh := allServices.WatchCh()

	// Populate the services map
	unique := make(map[string]map[string]struct{})
	for node := nodes.Next(); node != nil; node = nodes.Next() {
		n := node.(*structs.Node)
		if len(filters) > 1 && !structs.SatisfiesMetaFilters(n.Meta, filters) {
			continue
		}

		// List all the services on the node
		services, err := catalogServiceListByNode(tx, n.Node, entMeta, false)
		if err != nil {
			return 0, nil, fmt.Errorf("failed querying services: %s", err)
		}
		ws.AddWithLimit(watchLimit, services.WatchCh(), allServicesCh)

		// Rip through the services and enumerate them and their unique set of
		// tags.
		for service := services.Next(); service != nil; service = services.Next() {
			svc := service.(*structs.ServiceNode)
			tags, ok := unique[svc.ServiceName]
			if !ok {
				unique[svc.ServiceName] = make(map[string]struct{})
				tags = unique[svc.ServiceName]
			}
			for _, tag := range svc.ServiceTags {
				tags[tag] = struct{}{}
			}
		}
	}

	// Generate the output structure.
	var results = make(structs.Services)
	for service, tags := range unique {
		results[service] = make([]string, 0, len(tags))
		for tag := range tags {
			results[service] = append(results[service], tag)
		}
	}
	return idx, results, nil
}

// maxIndexForService return the maximum Raft Index for a service
// If the index is not set for the service, it will return the missing
// service index.
// The service_last_extinction is set to the last raft index when a service
// was unregistered (or 0 if no services were ever unregistered). This
// allows blocking queries to
//   * return when the last instance of a service is removed
//   * block until an instance for this service is available, or another
//     service is unregistered.
func maxIndexForService(tx ReadTxn, serviceName string, serviceExists, checks bool, entMeta *structs.EnterpriseMeta) uint64 {
	idx, _ := maxIndexAndWatchChForService(tx, serviceName, serviceExists, checks, entMeta)
	return idx
}

// maxIndexAndWatchChForService return the maximum Raft Index for a service. If
// the index is not set for the service, it will return the missing service
// index. The service_last_extinction is set to the last raft index when a
// service was unregistered (or 0 if no services were ever unregistered). This
// allows blocking queries to
//   * return when the last instance of a service is removed
//   * block until an instance for this service is available, or another
//     service is unregistered.
//
// It also _may_ return a watch chan to add to a WatchSet. It will only return
// one if the service exists, and has a service index. If it doesn't then nil is
// returned for the chan. This allows for blocking watchers to _only_ watch this
// one chan in the common case, falling back to watching all touched MemDB
// indexes in more complicated cases.
func maxIndexAndWatchChForService(tx ReadTxn, serviceName string, serviceExists, checks bool, entMeta *structs.EnterpriseMeta) (uint64, <-chan struct{}) {
	if !serviceExists {
		res, err := catalogServiceLastExtinctionIndex(tx, entMeta)
		if missingIdx, ok := res.(*IndexEntry); ok && err == nil {
			// Note safe to only watch the extinction index as it's not updated when new instances come along so return nil watchCh
			return missingIdx.Value, nil
		}
	}

	ch, res, err := catalogServiceMaxIndex(tx, serviceName, entMeta)
	if idx, ok := res.(*IndexEntry); ok && err == nil {
		return idx.Value, ch
	}
	return catalogMaxIndex(tx, entMeta, checks), nil
}

// Wrapper for maxIndexAndWatchChForService that operates on a list of ServiceNodes
func maxIndexAndWatchChsForServiceNodes(tx ReadTxn,
	nodes structs.ServiceNodes, watchChecks bool) (uint64, []<-chan struct{}) {

	var watchChans []<-chan struct{}
	var maxIdx uint64

	seen := make(map[structs.ServiceName]bool)
	for i := 0; i < len(nodes); i++ {
		sn := structs.NewServiceName(nodes[i].ServiceName, &nodes[i].EnterpriseMeta)
		if ok := seen[sn]; !ok {
			idx, svcCh := maxIndexAndWatchChForService(tx, sn.Name, true, watchChecks, &sn.EnterpriseMeta)
			if idx > maxIdx {
				maxIdx = idx
			}
			if svcCh != nil {
				watchChans = append(watchChans, svcCh)
			}
			seen[sn] = true
		}
	}

	return maxIdx, watchChans
}

// ConnectServiceNodes returns the nodes associated with a Connect
// compatible destination for the given service name. This will include
// both proxies and native integrations.
func (s *Store) ConnectServiceNodes(ws memdb.WatchSet, serviceName string, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceNodes, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()
	return serviceNodesTxn(tx, ws, serviceName, true, entMeta)
}

// ServiceNodes returns the nodes associated with a given service name.
func (s *Store) ServiceNodes(ws memdb.WatchSet, serviceName string, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceNodes, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()
	return serviceNodesTxn(tx, ws, serviceName, false, entMeta)
}

func serviceNodesTxn(tx ReadTxn, ws memdb.WatchSet, serviceName string, connect bool, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceNodes, error) {
	// Function for lookup
	index := "service"
	if connect {
		index = "connect"
	}

	services, err := catalogServiceNodeList(tx, serviceName, index, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed service lookup: %s", err)
	}
	ws.Add(services.WatchCh())

	var results structs.ServiceNodes
	for service := services.Next(); service != nil; service = services.Next() {
		results = append(results, service.(*structs.ServiceNode))
	}

	// If we are querying for Connect nodes, the associated proxy might be a gateway.
	// Gateways are tracked in a separate table, and we append them to the result set.
	// We append rather than replace since it allows users to migrate a service
	// to the mesh with a mix of sidecars and gateways until all its instances have a sidecar.
	var idx uint64
	if connect {
		// Look up gateway nodes associated with the service
		gwIdx, nodes, err := serviceGatewayNodes(tx, ws, serviceName, structs.ServiceKindTerminatingGateway, entMeta)
		if err != nil {
			return 0, nil, fmt.Errorf("failed gateway nodes lookup: %v", err)
		}
		if idx < gwIdx {
			idx = gwIdx
		}

		// Watch for index changes to the gateway nodes
		svcIdx, chans := maxIndexAndWatchChsForServiceNodes(tx, nodes, false)
		if svcIdx > idx {
			idx = svcIdx
		}
		for _, ch := range chans {
			ws.Add(ch)
		}

		for i := 0; i < len(nodes); i++ {
			results = append(results, nodes[i])
		}
	}

	// Fill in the node details.
	results, err = parseServiceNodes(tx, ws, results)
	if err != nil {
		return 0, nil, fmt.Errorf("failed parsing service nodes: %s", err)
	}

	// Get the table index.
	// TODO (gateways) (freddy) Why do we always consider the main service index here?
	//      This doesn't seem to make sense for Connect when there's more than 1 result
	svcIdx := maxIndexForService(tx, serviceName, len(results) > 0, false, entMeta)
	if idx < svcIdx {
		idx = svcIdx
	}

	return idx, results, nil
}

// ServiceTagNodes returns the nodes associated with a given service, filtering
// out services that don't contain the given tags.
func (s *Store) ServiceTagNodes(ws memdb.WatchSet, service string, tags []string, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceNodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// List all the services.
	services, err := catalogServiceNodeList(tx, service, "service", entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed service lookup: %s", err)
	}
	ws.Add(services.WatchCh())

	// Gather all the services and apply the tag filter.
	serviceExists := false
	var results structs.ServiceNodes
	for service := services.Next(); service != nil; service = services.Next() {
		svc := service.(*structs.ServiceNode)
		serviceExists = true
		if !serviceTagsFilter(svc, tags) {
			results = append(results, svc)
		}
	}

	// Fill in the node details.
	results, err = parseServiceNodes(tx, ws, results)
	if err != nil {
		return 0, nil, fmt.Errorf("failed parsing service nodes: %s", err)
	}
	// Get the table index.
	idx := maxIndexForService(tx, service, serviceExists, false, entMeta)

	return idx, results, nil
}

// serviceTagFilter returns true (should filter) if the given service node
// doesn't contain the given tag.
func serviceTagFilter(sn *structs.ServiceNode, tag string) bool {
	tag = strings.ToLower(tag)

	// Look for the lower cased version of the tag.
	for _, t := range sn.ServiceTags {
		if strings.ToLower(t) == tag {
			return false
		}
	}

	// If we didn't hit the tag above then we should filter.
	return true
}

// serviceTagsFilter returns true (should filter) if the given service node
// doesn't contain the given set of tags.
func serviceTagsFilter(sn *structs.ServiceNode, tags []string) bool {
	for _, tag := range tags {
		if serviceTagFilter(sn, tag) {
			// If any one of the expected tags was not found, filter the service
			return true
		}
	}

	// If all tags were found, don't filter the service
	return false
}

// ServiceAddressNodes returns the nodes associated with a given service, filtering
// out services that don't match the given serviceAddress
func (s *Store) ServiceAddressNodes(ws memdb.WatchSet, address string, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceNodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// List all the services.
	services, err := catalogServiceList(tx, entMeta, true)
	if err != nil {
		return 0, nil, fmt.Errorf("failed service lookup: %s", err)
	}
	ws.Add(services.WatchCh())

	// Gather all the services and apply the tag filter.
	var results structs.ServiceNodes
	for service := services.Next(); service != nil; service = services.Next() {
		svc := service.(*structs.ServiceNode)
		if svc.ServiceAddress == address {
			results = append(results, svc)
		} else {
			for _, addr := range svc.ServiceTaggedAddresses {
				if addr.Address == address {
					results = append(results, svc)
					break
				}
			}
		}
	}

	// Fill in the node details.
	results, err = parseServiceNodes(tx, ws, results)
	if err != nil {
		return 0, nil, fmt.Errorf("failed parsing service nodes: %s", err)
	}
	return 0, results, nil
}

// parseServiceNodes iterates over a services query and fills in the node details,
// returning a ServiceNodes slice.
func parseServiceNodes(tx ReadTxn, ws memdb.WatchSet, services structs.ServiceNodes) (structs.ServiceNodes, error) {
	// We don't want to track an unlimited number of nodes, so we pull a
	// top-level watch to use as a fallback.
	allNodes, err := tx.Get(tableNodes, indexID)
	if err != nil {
		return nil, fmt.Errorf("failed nodes lookup: %s", err)
	}
	allNodesCh := allNodes.WatchCh()

	// Fill in the node data for each service instance.
	var results structs.ServiceNodes
	for _, sn := range services {
		// Note that we have to clone here because we don't want to
		// modify the node-related fields on the object in the database,
		// which is what we are referencing.
		s := sn.PartialClone()

		// Grab the corresponding node record.
		watchCh, n, err := tx.FirstWatch(tableNodes, indexID, Query{Value: sn.Node})
		if err != nil {
			return nil, fmt.Errorf("failed node lookup: %s", err)
		}
		ws.AddWithLimit(watchLimit, watchCh, allNodesCh)

		// Populate the node-related fields. The tagged addresses may be
		// used by agents to perform address translation if they are
		// configured to do that.
		node := n.(*structs.Node)
		s.ID = node.ID
		s.Address = node.Address
		s.Datacenter = node.Datacenter
		s.TaggedAddresses = node.TaggedAddresses
		s.NodeMeta = node.Meta

		results = append(results, s)
	}
	return results, nil
}

// NodeService is used to retrieve a specific service associated with the given
// node.
func (s *Store) NodeService(nodeName string, serviceID string, entMeta *structs.EnterpriseMeta) (uint64, *structs.NodeService, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogServicesMaxIndex(tx, entMeta)

	// Query the service
	service, err := getNodeServiceTxn(tx, nodeName, serviceID, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed querying service for node %q: %s", nodeName, err)
	}

	return idx, service, nil
}

func getNodeServiceTxn(tx ReadTxn, nodeName, serviceID string, entMeta *structs.EnterpriseMeta) (*structs.NodeService, error) {
	// Query the service
	_, service, err := firstWatchCompoundWithTxn(tx, "services", "id", entMeta, nodeName, serviceID)
	if err != nil {
		return nil, fmt.Errorf("failed querying service for node %q: %s", nodeName, err)
	}

	if service != nil {
		return service.(*structs.ServiceNode).ToNodeService(), nil
	}

	return nil, nil
}

func (s *Store) nodeServices(ws memdb.WatchSet, nodeNameOrID string, entMeta *structs.EnterpriseMeta, allowWildcard bool) (bool, uint64, *structs.Node, memdb.ResultIterator, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogMaxIndex(tx, entMeta, false)

	// Query the node by node name
	watchCh, n, err := tx.FirstWatch(tableNodes, indexID, Query{Value: nodeNameOrID})
	if err != nil {
		return true, 0, nil, nil, fmt.Errorf("node lookup failed: %s", err)
	}

	if n != nil {
		ws.Add(watchCh)
	} else {
		if len(nodeNameOrID) < minUUIDLookupLen {
			ws.Add(watchCh)
			return true, 0, nil, nil, nil
		}

		// Attempt to lookup the node by its node ID
		iter, err := tx.Get("nodes", "uuid_prefix", resizeNodeLookupKey(nodeNameOrID))
		if err != nil {
			ws.Add(watchCh)
			// TODO(sean@): We could/should log an error re: the uuid_prefix lookup
			// failing once a logger has been introduced to the catalog.
			return true, 0, nil, nil, nil
		}

		n = iter.Next()
		if n == nil {
			// No nodes matched, even with the Node ID: add a watch on the node name.
			ws.Add(watchCh)
			return true, 0, nil, nil, nil
		}

		idWatchCh := iter.WatchCh()
		if iter.Next() != nil {
			// More than one match present: Watch on the node name channel and return
			// an empty result (node lookups can not be ambiguous).
			ws.Add(watchCh)
			return true, 0, nil, nil, nil
		}

		ws.Add(idWatchCh)
	}

	node := n.(*structs.Node)
	nodeName := node.Node

	// Read all of the services
	services, err := catalogServiceListByNode(tx, nodeName, entMeta, allowWildcard)
	if err != nil {
		return true, 0, nil, nil, fmt.Errorf("failed querying services for node %q: %s", nodeName, err)
	}
	ws.Add(services.WatchCh())

	return false, idx, node, services, nil
}

// NodeServices is used to query service registrations by node name or UUID.
func (s *Store) NodeServices(ws memdb.WatchSet, nodeNameOrID string, entMeta *structs.EnterpriseMeta) (uint64, *structs.NodeServices, error) {
	done, idx, node, services, err := s.nodeServices(ws, nodeNameOrID, entMeta, false)
	if done || err != nil {
		return idx, nil, err
	}

	// Initialize the node services struct
	ns := &structs.NodeServices{
		Node:     node,
		Services: make(map[string]*structs.NodeService),
	}

	if services != nil {
		// Add all of the services to the map.
		for service := services.Next(); service != nil; service = services.Next() {
			svc := service.(*structs.ServiceNode).ToNodeService()
			ns.Services[svc.ID] = svc
		}
	}

	return idx, ns, nil
}

// NodeServices is used to query service registrations by node name or UUID.
func (s *Store) NodeServiceList(ws memdb.WatchSet, nodeNameOrID string, entMeta *structs.EnterpriseMeta) (uint64, *structs.NodeServiceList, error) {
	done, idx, node, services, err := s.nodeServices(ws, nodeNameOrID, entMeta, true)
	if done || err != nil {
		return idx, nil, err
	}

	if idx == 0 {
		return 0, nil, nil
	}

	// Initialize the node services struct
	ns := &structs.NodeServiceList{
		Node: node,
	}

	if services != nil {
		// Add all of the services to the map.
		for service := services.Next(); service != nil; service = services.Next() {
			svc := service.(*structs.ServiceNode).ToNodeService()
			ns.Services = append(ns.Services, svc)
		}
	}

	return idx, ns, nil
}

// DeleteService is used to delete a given service associated with a node.
func (s *Store) DeleteService(idx uint64, nodeName, serviceID string, entMeta *structs.EnterpriseMeta) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	// Call the service deletion
	if err := s.deleteServiceTxn(tx, idx, nodeName, serviceID, entMeta); err != nil {
		return err
	}

	return tx.Commit()
}

// deleteServiceCASTxn is used to try doing a service delete operation with a given
// raft index. If the CAS index specified is not equal to the last observed index for
// the given service, then the call is a noop, otherwise a normal delete is invoked.
func (s *Store) deleteServiceCASTxn(tx WriteTxn, idx, cidx uint64, nodeName, serviceID string, entMeta *structs.EnterpriseMeta) (bool, error) {
	// Look up the service.
	service, err := getNodeServiceTxn(tx, nodeName, serviceID, entMeta)
	if err != nil {
		return false, fmt.Errorf("service lookup failed: %s", err)
	}
	if service == nil {
		return false, nil
	}

	// If the existing index does not match the provided CAS
	// index arg, then we shouldn't update anything and can safely
	// return early here.
	if service.ModifyIndex != cidx {
		return false, nil
	}

	// Call the actual deletion if the above passed.
	if err := s.deleteServiceTxn(tx, idx, nodeName, serviceID, entMeta); err != nil {
		return false, err
	}

	return true, nil
}

// deleteServiceTxn is the inner method called to remove a service
// registration within an existing transaction.
func (s *Store) deleteServiceTxn(tx WriteTxn, idx uint64, nodeName, serviceID string, entMeta *structs.EnterpriseMeta) error {
	// Look up the service.
	_, service, err := firstWatchCompoundWithTxn(tx, "services", "id", entMeta, nodeName, serviceID)
	if err != nil {
		return fmt.Errorf("failed service lookup: %s", err)
	}
	if service == nil {
		return nil
	}

	// TODO: accept a non-pointer value for EnterpriseMeta
	if entMeta == nil {
		entMeta = structs.DefaultEnterpriseMeta()
	}
	// Delete any checks associated with the service. This will invalidate
	// sessions as necessary.
	q := NodeServiceQuery{Node: nodeName, Service: serviceID, EnterpriseMeta: *entMeta}
	checks, err := tx.Get(tableChecks, indexNodeService, q)
	if err != nil {
		return fmt.Errorf("failed service check lookup: %s", err)
	}
	var deleteChecks []*structs.HealthCheck
	for check := checks.Next(); check != nil; check = checks.Next() {
		deleteChecks = append(deleteChecks, check.(*structs.HealthCheck))
	}

	// Do the delete in a separate loop so we don't trash the iterator.
	for _, check := range deleteChecks {
		if err := s.deleteCheckTxn(tx, idx, nodeName, check.CheckID, &check.EnterpriseMeta); err != nil {
			return err
		}
	}

	// Update the index.
	if err := catalogUpdateCheckIndexes(tx, idx, entMeta); err != nil {
		return err
	}

	// Delete the service and update the index
	if err := tx.Delete("services", service); err != nil {
		return fmt.Errorf("failed deleting service: %s", err)
	}
	if err := catalogUpdateServicesIndexes(tx, idx, entMeta); err != nil {
		return fmt.Errorf("failed updating index: %s", err)
	}

	svc := service.(*structs.ServiceNode)
	name := svc.CompoundServiceName()

	if err := catalogUpdateServiceKindIndexes(tx, svc.ServiceKind, idx, &svc.EnterpriseMeta); err != nil {
		return err
	}
	if err := cleanupMeshTopology(tx, idx, svc); err != nil {
		return fmt.Errorf("failed to clean up mesh-topology associations for %q: %v", name.String(), err)
	}

	if _, remainingService, err := firstWatchWithTxn(tx, "services", "service", svc.ServiceName, entMeta); err == nil {
		if remainingService != nil {
			// We have at least one remaining service, update the index
			if err := catalogUpdateServiceIndexes(tx, svc.ServiceName, idx, entMeta); err != nil {
				return err
			}
		} else {
			// There are no more service instances, cleanup the service.<serviceName> index
			_, serviceIndex, err := catalogServiceMaxIndex(tx, svc.ServiceName, entMeta)
			if err == nil && serviceIndex != nil {
				// we found service.<serviceName> index, garbage collect it
				if errW := tx.Delete(tableIndex, serviceIndex); errW != nil {
					return fmt.Errorf("[FAILED] deleting serviceIndex %s: %s", svc.ServiceName, err)
				}
			}

			if err := catalogUpdateServiceExtinctionIndex(tx, idx, entMeta); err != nil {
				return err
			}
			if err := cleanupGatewayWildcards(tx, idx, svc); err != nil {
				return fmt.Errorf("failed to clean up gateway-service associations for %q: %v", name.String(), err)
			}
		}
	} else {
		return fmt.Errorf("Could not find any service %s: %s", svc.ServiceName, err)
	}

	return nil
}

// EnsureCheck is used to store a check registration in the db.
func (s *Store) EnsureCheck(idx uint64, hc *structs.HealthCheck) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	// Call the check registration
	if err := s.ensureCheckTxn(tx, idx, false, hc); err != nil {
		return err
	}

	return tx.Commit()
}

// updateAllServiceIndexesOfNode updates the Raft index of all the services associated with this node
func updateAllServiceIndexesOfNode(tx WriteTxn, idx uint64, nodeID string) error {
	services, err := tx.Get("services", "node", nodeID)
	if err != nil {
		return fmt.Errorf("failed updating services for node %s: %s", nodeID, err)
	}
	for service := services.Next(); service != nil; service = services.Next() {
		svc := service.(*structs.ServiceNode)
		if err := catalogUpdateServiceIndexes(tx, svc.ServiceName, idx, &svc.EnterpriseMeta); err != nil {
			return err
		}
		if err := catalogUpdateServiceKindIndexes(tx, svc.ServiceKind, idx, &svc.EnterpriseMeta); err != nil {
			return err
		}
	}
	return nil
}

// ensureCheckCASTxn updates a check only if the existing index matches the given index.
// Returns a bool indicating if a write happened and any error.
func (s *Store) ensureCheckCASTxn(tx WriteTxn, idx uint64, hc *structs.HealthCheck) (bool, error) {
	// Retrieve the existing entry.
	_, existing, err := getNodeCheckTxn(tx, hc.Node, hc.CheckID, &hc.EnterpriseMeta)
	if err != nil {
		return false, fmt.Errorf("failed health check lookup: %s", err)
	}

	// Check if the we should do the set. A ModifyIndex of 0 means that
	// we are doing a set-if-not-exists.
	if hc.ModifyIndex == 0 && existing != nil {
		return false, nil
	}
	if hc.ModifyIndex != 0 && existing == nil {
		return false, nil
	}
	if existing != nil && hc.ModifyIndex != 0 && hc.ModifyIndex != existing.ModifyIndex {
		return false, nil
	}

	// Perform the update.
	if err := s.ensureCheckTxn(tx, idx, false, hc); err != nil {
		return false, err
	}

	return true, nil
}

// ensureCheckTxn is used as the inner method to handle inserting
// a health check into the state store. It ensures safety against inserting
// checks with no matching node or service.
func (s *Store) ensureCheckTxn(tx WriteTxn, idx uint64, preserveIndexes bool, hc *structs.HealthCheck) error {
	// Check if we have an existing health check
	_, existing, err := firstWatchCompoundWithTxn(tx, "checks", "id", &hc.EnterpriseMeta, hc.Node, string(hc.CheckID))
	if err != nil {
		return fmt.Errorf("failed health check lookup: %s", err)
	}

	// Set the indexes
	if existing != nil {
		existingCheck := existing.(*structs.HealthCheck)
		hc.CreateIndex = existingCheck.CreateIndex
		hc.ModifyIndex = existingCheck.ModifyIndex
	} else if !preserveIndexes {
		hc.CreateIndex = idx
	}

	// Use the default check status if none was provided
	if hc.Status == "" {
		hc.Status = api.HealthCritical
	}

	// Get the node
	node, err := tx.First(tableNodes, indexID, Query{Value: hc.Node})
	if err != nil {
		return fmt.Errorf("failed node lookup: %s", err)
	}
	if node == nil {
		return ErrMissingNode
	}

	modified := true
	// If the check is associated with a service, check that we have
	// a registration for the service.
	if hc.ServiceID != "" {
		_, service, err := firstWatchCompoundWithTxn(tx, "services", "id", &hc.EnterpriseMeta, hc.Node, hc.ServiceID)
		if err != nil {
			return fmt.Errorf("failed service lookup: %s", err)
		}
		if service == nil {
			return ErrMissingService
		}

		// Copy in the service name and tags
		svc := service.(*structs.ServiceNode)
		hc.ServiceName = svc.ServiceName
		hc.ServiceTags = svc.ServiceTags
		if existing != nil && existing.(*structs.HealthCheck).IsSame(hc) {
			modified = false
		} else {
			if err = catalogUpdateServiceIndexes(tx, svc.ServiceName, idx, &svc.EnterpriseMeta); err != nil {
				return err
			}
			if err := catalogUpdateServiceKindIndexes(tx, svc.ServiceKind, idx, &svc.EnterpriseMeta); err != nil {
				return err
			}
		}
	} else {
		if existing != nil && existing.(*structs.HealthCheck).IsSame(hc) {
			modified = false
		} else {
			// Since the check has been modified, it impacts all services of node
			// Update the status for all the services associated with this node
			err = updateAllServiceIndexesOfNode(tx, idx, hc.Node)
			if err != nil {
				return err
			}
		}
	}

	// Delete any sessions for this check if the health is critical.
	if hc.Status == api.HealthCritical {
		sessions, err := checkSessionsTxn(tx, hc)
		if err != nil {
			return err
		}

		// Delete the session in a separate loop so we don't trash the
		// iterator.
		for _, sess := range sessions {
			if err := s.deleteSessionTxn(tx, idx, sess.Session, &sess.EnterpriseMeta); err != nil {
				return fmt.Errorf("failed deleting session: %s", err)
			}
		}
	}
	if !modified {
		return nil
	}
	if !preserveIndexes {
		hc.ModifyIndex = idx
	}

	return catalogInsertCheck(tx, hc, idx)
}

// NodeCheck is used to retrieve a specific check associated with the given
// node.
func (s *Store) NodeCheck(nodeName string, checkID types.CheckID, entMeta *structs.EnterpriseMeta) (uint64, *structs.HealthCheck, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	return getNodeCheckTxn(tx, nodeName, checkID, entMeta)
}

// nodeCheckTxn is used as the inner method to handle reading a health check
// from the state store.
func getNodeCheckTxn(tx ReadTxn, nodeName string, checkID types.CheckID, entMeta *structs.EnterpriseMeta) (uint64, *structs.HealthCheck, error) {
	// Get the table index.
	idx := catalogChecksMaxIndex(tx, entMeta)

	// Return the check.
	_, check, err := firstWatchCompoundWithTxn(tx, "checks", "id", entMeta, nodeName, string(checkID))
	if err != nil {
		return 0, nil, fmt.Errorf("failed check lookup: %s", err)
	}

	if check != nil {
		return idx, check.(*structs.HealthCheck), nil
	}
	return idx, nil, nil
}

// NodeChecks is used to retrieve checks associated with the
// given node from the state store.
func (s *Store) NodeChecks(ws memdb.WatchSet, nodeName string, entMeta *structs.EnterpriseMeta) (uint64, structs.HealthChecks, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogChecksMaxIndex(tx, entMeta)

	// Return the checks.
	iter, err := catalogListChecksByNode(tx, nodeName, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed check lookup: %s", err)
	}
	ws.Add(iter.WatchCh())

	var results structs.HealthChecks
	for check := iter.Next(); check != nil; check = iter.Next() {
		results = append(results, check.(*structs.HealthCheck))
	}
	return idx, results, nil
}

// ServiceChecks is used to get all checks associated with a
// given service ID. The query is performed against a service
// _name_ instead of a service ID.
func (s *Store) ServiceChecks(ws memdb.WatchSet, serviceName string, entMeta *structs.EnterpriseMeta) (uint64, structs.HealthChecks, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogChecksMaxIndex(tx, entMeta)

	// Return the checks.
	iter, err := catalogListChecksByService(tx, serviceName, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed check lookup: %s", err)
	}
	ws.Add(iter.WatchCh())

	var results structs.HealthChecks
	for check := iter.Next(); check != nil; check = iter.Next() {
		results = append(results, check.(*structs.HealthCheck))
	}
	return idx, results, nil
}

// ServiceChecksByNodeMeta is used to get all checks associated with a
// given service ID, filtered by the given node metadata values. The query
// is performed against a service _name_ instead of a service ID.
func (s *Store) ServiceChecksByNodeMeta(ws memdb.WatchSet, serviceName string,
	filters map[string]string, entMeta *structs.EnterpriseMeta) (uint64, structs.HealthChecks, error) {

	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := maxIndexForService(tx, serviceName, true, true, entMeta)
	// Return the checks.
	iter, err := catalogListChecksByService(tx, serviceName, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed check lookup: %s", err)
	}
	ws.Add(iter.WatchCh())

	return parseChecksByNodeMeta(tx, ws, idx, iter, filters)
}

// ChecksInState is used to query the state store for all checks
// which are in the provided state.
func (s *Store) ChecksInState(ws memdb.WatchSet, state string, entMeta *structs.EnterpriseMeta) (uint64, structs.HealthChecks, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	idx, iter, err := checksInStateTxn(tx, ws, state, entMeta)
	if err != nil {
		return 0, nil, err
	}

	var results structs.HealthChecks
	for check := iter.Next(); check != nil; check = iter.Next() {
		results = append(results, check.(*structs.HealthCheck))
	}
	return idx, results, nil
}

// ChecksInStateByNodeMeta is used to query the state store for all checks
// which are in the provided state, filtered by the given node metadata values.
func (s *Store) ChecksInStateByNodeMeta(ws memdb.WatchSet, state string, filters map[string]string, entMeta *structs.EnterpriseMeta) (uint64, structs.HealthChecks, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	idx, iter, err := checksInStateTxn(tx, ws, state, entMeta)
	if err != nil {
		return 0, nil, err
	}

	return parseChecksByNodeMeta(tx, ws, idx, iter, filters)
}

func checksInStateTxn(tx ReadTxn, ws memdb.WatchSet, state string, entMeta *structs.EnterpriseMeta) (uint64, memdb.ResultIterator, error) {
	// Get the table index.
	idx := catalogChecksMaxIndex(tx, entMeta)

	// Query all checks if HealthAny is passed, otherwise use the index.
	var iter memdb.ResultIterator
	var err error
	if state == api.HealthAny {
		iter, err = catalogListChecks(tx, entMeta)
	} else {
		iter, err = catalogListChecksInState(tx, state, entMeta)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("failed check lookup: %s", err)
	}
	ws.Add(iter.WatchCh())

	return idx, iter, err
}

// parseChecksByNodeMeta is a helper function used to deduplicate some
// repetitive code for returning health checks filtered by node metadata fields.
func parseChecksByNodeMeta(tx ReadTxn, ws memdb.WatchSet,
	idx uint64, iter memdb.ResultIterator, filters map[string]string) (uint64, structs.HealthChecks, error) {

	// We don't want to track an unlimited number of nodes, so we pull a
	// top-level watch to use as a fallback.
	allNodes, err := tx.Get(tableNodes, indexID)
	if err != nil {
		return 0, nil, fmt.Errorf("failed nodes lookup: %s", err)
	}
	allNodesCh := allNodes.WatchCh()

	// Only take results for nodes that satisfy the node metadata filters.
	var results structs.HealthChecks
	for check := iter.Next(); check != nil; check = iter.Next() {
		healthCheck := check.(*structs.HealthCheck)
		watchCh, node, err := tx.FirstWatch(tableNodes, indexID, Query{Value: healthCheck.Node})
		if err != nil {
			return 0, nil, fmt.Errorf("failed node lookup: %s", err)
		}
		if node == nil {
			return 0, nil, ErrMissingNode
		}

		// Add even the filtered nodes so we wake up if the node metadata
		// changes.
		ws.AddWithLimit(watchLimit, watchCh, allNodesCh)
		if structs.SatisfiesMetaFilters(node.(*structs.Node).Meta, filters) {
			results = append(results, healthCheck)
		}
	}
	return idx, results, nil
}

// DeleteCheck is used to delete a health check registration.
func (s *Store) DeleteCheck(idx uint64, node string, checkID types.CheckID, entMeta *structs.EnterpriseMeta) error {
	tx := s.db.WriteTxn(idx)
	defer tx.Abort()

	// Call the check deletion
	if err := s.deleteCheckTxn(tx, idx, node, checkID, entMeta); err != nil {
		return err
	}

	return tx.Commit()
}

// deleteCheckCASTxn is used to try doing a check delete operation with a given
// raft index. If the CAS index specified is not equal to the last observed index for
// the given check, then the call is a noop, otherwise a normal check delete is invoked.
func (s *Store) deleteCheckCASTxn(tx WriteTxn, idx, cidx uint64, node string, checkID types.CheckID, entMeta *structs.EnterpriseMeta) (bool, error) {
	// Try to retrieve the existing health check.
	_, hc, err := getNodeCheckTxn(tx, node, checkID, entMeta)
	if err != nil {
		return false, fmt.Errorf("check lookup failed: %s", err)
	}
	if hc == nil {
		return false, nil
	}

	// If the existing index does not match the provided CAS
	// index arg, then we shouldn't update anything and can safely
	// return early here.
	if hc.ModifyIndex != cidx {
		return false, nil
	}

	// Call the actual deletion if the above passed.
	if err := s.deleteCheckTxn(tx, idx, node, checkID, entMeta); err != nil {
		return false, err
	}

	return true, nil
}

// NodeServiceQuery is a type used to query the checks table.
type NodeServiceQuery struct {
	Node    string
	Service string
	structs.EnterpriseMeta
}

// deleteCheckTxn is the inner method used to call a health
// check deletion within an existing transaction.
func (s *Store) deleteCheckTxn(tx WriteTxn, idx uint64, node string, checkID types.CheckID, entMeta *structs.EnterpriseMeta) error {
	// Try to retrieve the existing health check.
	_, hc, err := firstWatchCompoundWithTxn(tx, "checks", "id", entMeta, node, string(checkID))
	if err != nil {
		return fmt.Errorf("check lookup failed: %s", err)
	}
	if hc == nil {
		return nil
	}
	existing := hc.(*structs.HealthCheck)
	if existing != nil {
		// When no service is linked to this service, update all services of node
		if existing.ServiceID != "" {
			if err := catalogUpdateServiceIndexes(tx, existing.ServiceName, idx, &existing.EnterpriseMeta); err != nil {
				return err
			}

			_, svcRaw, err := firstWatchCompoundWithTxn(tx, "services", "id", &existing.EnterpriseMeta, existing.Node, existing.ServiceID)
			if err != nil {
				return fmt.Errorf("failed retrieving service from state store: %v", err)
			}

			svc := svcRaw.(*structs.ServiceNode)
			if err := catalogUpdateServiceKindIndexes(tx, svc.ServiceKind, idx, &svc.EnterpriseMeta); err != nil {
				return err
			}
		} else {
			if err := updateAllServiceIndexesOfNode(tx, idx, existing.Node); err != nil {
				return fmt.Errorf("Failed to update services linked to deleted healthcheck: %s", err)
			}
			if err := catalogUpdateServicesIndexes(tx, idx, entMeta); err != nil {
				return err
			}
		}
	}

	// Delete the check from the DB and update the index.
	if err := tx.Delete("checks", hc); err != nil {
		return fmt.Errorf("failed removing check: %s", err)
	}

	if err := catalogUpdateCheckIndexes(tx, idx, entMeta); err != nil {
		return err
	}

	// Delete any sessions for this check.
	sessions, err := checkSessionsTxn(tx, existing)
	if err != nil {
		return err
	}

	// Do the delete in a separate loop so we don't trash the iterator.
	for _, sess := range sessions {
		if err := s.deleteSessionTxn(tx, idx, sess.Session, &sess.EnterpriseMeta); err != nil {
			return fmt.Errorf("failed deleting session: %s", err)
		}
	}

	return nil
}

// CombinedCheckServiceNodes is used to query all nodes and checks for both typical and Connect endpoints of a service
func (s *Store) CombinedCheckServiceNodes(ws memdb.WatchSet, service structs.ServiceName) (uint64, structs.CheckServiceNodes, error) {
	var (
		resp   structs.CheckServiceNodes
		maxIdx uint64
	)
	idx, csn, err := s.CheckServiceNodes(ws, service.Name, &service.EnterpriseMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get downstream nodes for %q: %v", service, err)
	}
	if idx > maxIdx {
		maxIdx = idx
	}
	resp = append(resp, csn...)

	idx, csn, err = s.CheckConnectServiceNodes(ws, service.Name, &service.EnterpriseMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get downstream connect nodes for %q: %v", service, err)
	}
	if idx > maxIdx {
		maxIdx = idx
	}
	resp = append(resp, csn...)

	return maxIdx, resp, nil
}

// CheckServiceNodes is used to query all nodes and checks for a given service.
func (s *Store) CheckServiceNodes(ws memdb.WatchSet, serviceName string, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	return s.checkServiceNodes(ws, serviceName, false, entMeta)
}

// CheckConnectServiceNodes is used to query all nodes and checks for Connect
// compatible endpoints for a given service.
func (s *Store) CheckConnectServiceNodes(ws memdb.WatchSet, serviceName string, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	return s.checkServiceNodes(ws, serviceName, true, entMeta)
}

// CheckIngressServiceNodes is used to query all nodes and checks for ingress
// endpoints for a given service.
func (s *Store) CheckIngressServiceNodes(ws memdb.WatchSet, serviceName string, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	maxIdx, nodes, err := serviceGatewayNodes(tx, ws, serviceName, structs.ServiceKindIngressGateway, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed gateway nodes lookup: %v", err)
	}

	// TODO(ingress) : Deal with incorporating index from mapping table
	// Watch for index changes to the gateway nodes
	idx, chans := maxIndexAndWatchChsForServiceNodes(tx, nodes, false)
	for _, ch := range chans {
		ws.Add(ch)
	}
	maxIdx = lib.MaxUint64(maxIdx, idx)

	// TODO(ingress): Test namespace functionality here
	// De-dup services to lookup
	names := make(map[structs.ServiceName]struct{})
	for _, n := range nodes {
		names[n.CompoundServiceName()] = struct{}{}
	}

	var results structs.CheckServiceNodes
	for sn := range names {
		idx, n, err := checkServiceNodesTxn(tx, ws, sn.Name, false, &sn.EnterpriseMeta)
		if err != nil {
			return 0, nil, err
		}
		maxIdx = lib.MaxUint64(maxIdx, idx)
		results = append(results, n...)
	}
	return maxIdx, results, nil
}

func (s *Store) checkServiceNodes(ws memdb.WatchSet, serviceName string, connect bool, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	return checkServiceNodesTxn(tx, ws, serviceName, connect, entMeta)
}

func checkServiceNodesTxn(tx ReadTxn, ws memdb.WatchSet, serviceName string, connect bool, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	// Function for lookup
	index := "service"
	if connect {
		index = "connect"
	}

	// Query the state store for the service.
	iter, err := catalogServiceNodeList(tx, serviceName, index, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed service lookup: %s", err)
	}
	// Note we decide if we want to watch this iterator or not down below. We need
	// to see if it returned anything first.

	// Return the results.
	var results structs.ServiceNodes

	// For connect queries we need a list of any proxy service names in the result
	// set. Rather than have different code path for connect and non-connect, we
	// use the same one in both cases. For non-empty non-connect results,
	// serviceNames will always have exactly one element which is the same as
	// serviceName. For Connect there might be multiple different service names -
	// one for each service name a proxy is registered under, and the target
	// service name IFF there is at least one Connect-native instance of that
	// service. Either way there is usually only one distinct name if proxies are
	// named consistently but could be multiple.
	serviceNames := make(map[structs.ServiceName]struct{}, 2)
	for service := iter.Next(); service != nil; service = iter.Next() {
		sn := service.(*structs.ServiceNode)
		results = append(results, sn)

		name := structs.NewServiceName(sn.ServiceName, &sn.EnterpriseMeta)
		serviceNames[name] = struct{}{}
	}

	// If we are querying for Connect nodes, the associated proxy might be a terminating-gateway.
	// Gateways are tracked in a separate table, and we append them to the result set.
	// We append rather than replace since it allows users to migrate a service
	// to the mesh with a mix of sidecars and gateways until all its instances have a sidecar.
	var idx uint64
	if connect {
		// Look up gateway nodes associated with the service
		gwIdx, nodes, err := serviceGatewayNodes(tx, ws, serviceName, structs.ServiceKindTerminatingGateway, entMeta)
		if err != nil {
			return 0, nil, fmt.Errorf("failed gateway nodes lookup: %v", err)
		}
		idx = lib.MaxUint64(idx, gwIdx)
		for i := 0; i < len(nodes); i++ {
			results = append(results, nodes[i])

			name := structs.NewServiceName(nodes[i].ServiceName, &nodes[i].EnterpriseMeta)
			serviceNames[name] = struct{}{}
		}
	}

	// watchOptimized tracks if we meet the necessary condition to optimize
	// WatchSet size. That is that every service name represented in the result
	// set must have a service-specific index we can watch instead of many radix
	// nodes for all the actual nodes touched. This saves us watching potentially
	// thousands of watch chans for large services which may need many goroutines.
	// It also avoids the performance cliff that is hit when watchLimit is hit
	// (~682 service instances). See
	// https://github.com/hashicorp/consul/issues/4984
	watchOptimized := false
	if len(serviceNames) > 0 {
		// Assume optimization will work since it really should at this point. For
		// safety we'll sanity check this below for each service name.
		watchOptimized = true

		// Fetch indexes for all names services in result set.
		for n := range serviceNames {
			// We know service values should exist since the serviceNames map is only
			// populated if there is at least one result above. so serviceExists arg
			// below is always true.
			svcIdx, svcCh := maxIndexAndWatchChForService(tx, n.Name, true, true, &n.EnterpriseMeta)
			// Take the max index represented
			idx = lib.MaxUint64(idx, svcIdx)
			if svcCh != nil {
				// Watch the service-specific index for changes in liu of all iradix nodes
				// for checks etc.
				ws.Add(svcCh)
			} else {
				// Nil svcCh shouldn't really happen since all existent services should
				// have a service-specific index but just in case it does due to a bug,
				// fall back to the more expensive old way of watching every radix node
				// we touch.
				watchOptimized = false
			}
		}
	} else {
		// If we have no results, we should use the index of the last service
		// extinction event so we don't go backwards when services deregister. We
		// use target serviceName here but it actually doesn't matter. No chan will
		// be returned as we can't use the optimization in this case (and don't need
		// to as there is only one chan to watch anyway).
		svcIdx, _ := maxIndexAndWatchChForService(tx, serviceName, false, true, entMeta)
		idx = lib.MaxUint64(idx, svcIdx)
	}

	// Create a nil watchset to pass below, we'll only pass the real one if we
	// need to. Nil watchers are safe/allowed and saves some allocation too.
	var fallbackWS memdb.WatchSet
	if !watchOptimized {
		// We weren't able to use the optimization of watching only service indexes
		// for some reason. That means we need to fallback to watching everything we
		// touch in the DB as normal. We plumb the caller's watchset through (note
		// it's a map so this is a by-reference assignment.)
		fallbackWS = ws
		// We also need to watch the iterator from earlier too.
		fallbackWS.Add(iter.WatchCh())
	} else if connect {
		// If this is a connect query then there is a subtlety to watch out for.
		// In addition to watching the proxy service indexes for changes above, we
		// need to still keep an eye on the connect service index in case a new
		// proxy with a new name registers - we are only watching proxy service
		// names we know about above so we'd miss that otherwise. Thankfully this
		// is only ever one extra chan to watch and will catch any changes to
		// proxy registrations for this target service.
		ws.Add(iter.WatchCh())
	}

	return parseCheckServiceNodes(tx, fallbackWS, idx, results, err)
}

// CheckServiceTagNodes is used to query all nodes and checks for a given
// service, filtering out services that don't contain the given tag.
func (s *Store) CheckServiceTagNodes(ws memdb.WatchSet, serviceName string, tags []string, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Query the state store for the service.
	iter, err := catalogServiceNodeList(tx, serviceName, "service", entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed service lookup: %s", err)
	}
	ws.Add(iter.WatchCh())

	// Return the results, filtering by tag.
	serviceExists := false
	var results structs.ServiceNodes
	for service := iter.Next(); service != nil; service = iter.Next() {
		svc := service.(*structs.ServiceNode)
		serviceExists = true
		if !serviceTagsFilter(svc, tags) {
			results = append(results, svc)
		}
	}

	// Get the table index.
	idx := maxIndexForService(tx, serviceName, serviceExists, true, entMeta)
	return parseCheckServiceNodes(tx, ws, idx, results, err)
}

// GatewayServices is used to query all services associated with a gateway
func (s *Store) GatewayServices(ws memdb.WatchSet, gateway string, entMeta *structs.EnterpriseMeta) (uint64, structs.GatewayServices, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	iter, err := gatewayServices(tx, gateway, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed gateway services lookup: %s", err)
	}
	ws.Add(iter.WatchCh())

	maxIdx, results, err := s.collectGatewayServices(tx, ws, iter)
	if err != nil {
		return 0, nil, err
	}
	idx := maxIndexTxn(tx, tableGatewayServices)

	return lib.MaxUint64(maxIdx, idx), results, nil
}

// parseCheckServiceNodes is used to parse through a given set of services,
// and query for an associated node and a set of checks. This is the inner
// method used to return a rich set of results from a more simple query.
//
// TODO: idx parameter is not used except as a return value. Remove it.
// TODO: err parameter is only used for early return. Remove it and check from the
// caller.
func parseCheckServiceNodes(
	tx ReadTxn, ws memdb.WatchSet, idx uint64,
	services structs.ServiceNodes,
	err error) (uint64, structs.CheckServiceNodes, error) {
	if err != nil {
		return 0, nil, err
	}

	// Special-case the zero return value to nil, since this ends up in
	// external APIs.
	if len(services) == 0 {
		return idx, nil, nil
	}

	// We don't want to track an unlimited number of nodes, so we pull a
	// top-level watch to use as a fallback.
	allNodes, err := tx.Get(tableNodes, indexID)
	if err != nil {
		return 0, nil, fmt.Errorf("failed nodes lookup: %s", err)
	}
	allNodesCh := allNodes.WatchCh()

	// We need a similar fallback for checks. Since services need the
	// status of node + service-specific checks, we pull in a top-level
	// watch over all checks.
	allChecks, err := tx.Get("checks", "id")
	if err != nil {
		return 0, nil, fmt.Errorf("failed checks lookup: %s", err)
	}
	allChecksCh := allChecks.WatchCh()

	results := make(structs.CheckServiceNodes, 0, len(services))
	for _, sn := range services {
		// Retrieve the node.
		watchCh, n, err := tx.FirstWatch(tableNodes, indexID, Query{Value: sn.Node})
		if err != nil {
			return 0, nil, fmt.Errorf("failed node lookup: %s", err)
		}
		ws.AddWithLimit(watchLimit, watchCh, allNodesCh)

		if n == nil {
			return 0, nil, ErrMissingNode
		}
		node := n.(*structs.Node)

		// First add the node-level checks. These always apply to any
		// service on the node.
		var checks structs.HealthChecks
		q := NodeServiceQuery{Node: sn.Node, EnterpriseMeta: *structs.DefaultEnterpriseMeta()}
		iter, err := tx.Get(tableChecks, indexNodeService, q)
		if err != nil {
			return 0, nil, err
		}
		ws.AddWithLimit(watchLimit, iter.WatchCh(), allChecksCh)
		for check := iter.Next(); check != nil; check = iter.Next() {
			checks = append(checks, check.(*structs.HealthCheck))
		}

		// Now add the service-specific checks.
		q = NodeServiceQuery{Node: sn.Node, Service: sn.ServiceID, EnterpriseMeta: sn.EnterpriseMeta}
		iter, err = tx.Get(tableChecks, indexNodeService, q)
		if err != nil {
			return 0, nil, err
		}
		ws.AddWithLimit(watchLimit, iter.WatchCh(), allChecksCh)
		for check := iter.Next(); check != nil; check = iter.Next() {
			checks = append(checks, check.(*structs.HealthCheck))
		}

		// Append to the results.
		results = append(results, structs.CheckServiceNode{
			Node:    node,
			Service: sn.ToNodeService(),
			Checks:  checks,
		})
	}

	return idx, results, nil
}

// NodeInfo is used to generate a dump of a single node. The dump includes
// all services and checks which are registered against the node.
func (s *Store) NodeInfo(ws memdb.WatchSet, node string, entMeta *structs.EnterpriseMeta) (uint64, structs.NodeDump, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogMaxIndex(tx, entMeta, true)

	// Query the node by the passed node
	nodes, err := tx.Get(tableNodes, indexID, Query{Value: node})
	if err != nil {
		return 0, nil, fmt.Errorf("failed node lookup: %s", err)
	}
	ws.Add(nodes.WatchCh())
	return parseNodes(tx, ws, idx, nodes, entMeta)
}

// NodeDump is used to generate a dump of all nodes. This call is expensive
// as it has to query every node, service, and check. The response can also
// be quite large since there is currently no filtering applied.
func (s *Store) NodeDump(ws memdb.WatchSet, entMeta *structs.EnterpriseMeta) (uint64, structs.NodeDump, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	// Get the table index.
	idx := catalogMaxIndex(tx, entMeta, true)

	// Fetch all of the registered nodes
	nodes, err := tx.Get(tableNodes, indexID)
	if err != nil {
		return 0, nil, fmt.Errorf("failed node lookup: %s", err)
	}
	ws.Add(nodes.WatchCh())
	return parseNodes(tx, ws, idx, nodes, entMeta)
}

func (s *Store) ServiceDump(ws memdb.WatchSet, kind structs.ServiceKind, useKind bool, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	tx := s.db.Txn(false)
	defer tx.Abort()

	if useKind {
		return serviceDumpKindTxn(tx, ws, kind, entMeta)
	} else {
		return serviceDumpAllTxn(tx, ws, entMeta)
	}
}

func serviceDumpAllTxn(tx ReadTxn, ws memdb.WatchSet, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	// Get the table index
	idx := catalogMaxIndexWatch(tx, ws, entMeta, true)

	services, err := catalogServiceList(tx, entMeta, true)
	if err != nil {
		return 0, nil, fmt.Errorf("failed service lookup: %s", err)
	}

	var results structs.ServiceNodes
	for service := services.Next(); service != nil; service = services.Next() {
		sn := service.(*structs.ServiceNode)
		results = append(results, sn)
	}

	return parseCheckServiceNodes(tx, nil, idx, results, err)
}

func serviceDumpKindTxn(tx ReadTxn, ws memdb.WatchSet, kind structs.ServiceKind, entMeta *structs.EnterpriseMeta) (uint64, structs.CheckServiceNodes, error) {
	// unlike when we are dumping all services here we only need to watch the kind specific index entry for changing (or nodes, checks)
	// updating any services, nodes or checks will bump the appropriate service kind index so there is no need to watch any of the individual
	// entries
	idx := catalogServiceKindMaxIndex(tx, ws, kind, entMeta)

	// Query the state store for the service.
	services, err := catalogServiceListByKind(tx, kind, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed service lookup: %s", err)
	}

	var results structs.ServiceNodes
	for service := services.Next(); service != nil; service = services.Next() {
		sn := service.(*structs.ServiceNode)
		results = append(results, sn)
	}

	return parseCheckServiceNodes(tx, nil, idx, results, err)
}

// parseNodes takes an iterator over a set of nodes and returns a struct
// containing the nodes along with all of their associated services
// and/or health checks.
func parseNodes(tx ReadTxn, ws memdb.WatchSet, idx uint64,
	iter memdb.ResultIterator, entMeta *structs.EnterpriseMeta) (uint64, structs.NodeDump, error) {

	// We don't want to track an unlimited number of services, so we pull a
	// top-level watch to use as a fallback.
	allServices, err := tx.Get("services", "id")
	if err != nil {
		return 0, nil, fmt.Errorf("failed services lookup: %s", err)
	}
	allServicesCh := allServices.WatchCh()

	// We need a similar fallback for checks.
	allChecks, err := tx.Get("checks", "id")
	if err != nil {
		return 0, nil, fmt.Errorf("failed checks lookup: %s", err)
	}
	allChecksCh := allChecks.WatchCh()

	var results structs.NodeDump
	for n := iter.Next(); n != nil; n = iter.Next() {
		node := n.(*structs.Node)

		// Create the wrapped node
		dump := &structs.NodeInfo{
			ID:              node.ID,
			Node:            node.Node,
			Address:         node.Address,
			TaggedAddresses: node.TaggedAddresses,
			Meta:            node.Meta,
		}

		// Query the node services
		services, err := catalogServiceListByNode(tx, node.Node, entMeta, true)
		if err != nil {
			return 0, nil, fmt.Errorf("failed services lookup: %s", err)
		}
		ws.AddWithLimit(watchLimit, services.WatchCh(), allServicesCh)
		for service := services.Next(); service != nil; service = services.Next() {
			ns := service.(*structs.ServiceNode).ToNodeService()
			dump.Services = append(dump.Services, ns)
		}

		// Query the service level checks
		checks, err := catalogListChecksByNode(tx, node.Node, entMeta)
		if err != nil {
			return 0, nil, fmt.Errorf("failed node lookup: %s", err)
		}
		ws.AddWithLimit(watchLimit, checks.WatchCh(), allChecksCh)
		for check := checks.Next(); check != nil; check = checks.Next() {
			hc := check.(*structs.HealthCheck)
			dump.Checks = append(dump.Checks, hc)
		}

		// Add the result to the slice
		results = append(results, dump)
	}
	return idx, results, nil
}

// checkSessionsTxn returns the IDs of all sessions associated with a health check
func checkSessionsTxn(tx ReadTxn, hc *structs.HealthCheck) ([]*sessionCheck, error) {
	mappings, err := getCompoundWithTxn(tx, "session_checks", "node_check", &hc.EnterpriseMeta, hc.Node, string(hc.CheckID))
	if err != nil {
		return nil, fmt.Errorf("failed session checks lookup: %s", err)
	}

	var sessions []*sessionCheck
	for mapping := mappings.Next(); mapping != nil; mapping = mappings.Next() {
		sessions = append(sessions, mapping.(*sessionCheck))
	}
	return sessions, nil
}

// updateGatewayServices associates services with gateways as specified in a gateway config entry
func updateGatewayServices(tx WriteTxn, idx uint64, conf structs.ConfigEntry, entMeta *structs.EnterpriseMeta) error {
	var (
		noChange        bool
		gatewayServices structs.GatewayServices
		err             error
	)

	gateway := structs.NewServiceName(conf.GetName(), entMeta)
	switch conf.GetKind() {
	case structs.IngressGateway:
		noChange, gatewayServices, err = ingressConfigGatewayServices(tx, gateway, conf, entMeta)
	case structs.TerminatingGateway:
		noChange, gatewayServices, err = terminatingConfigGatewayServices(tx, gateway, conf, entMeta)
	default:
		return fmt.Errorf("config entry kind %q does not need gateway-services", conf.GetKind())
	}
	// Return early if there is an error OR we don't have any services to update
	if err != nil || noChange {
		return err
	}

	// Delete all associated with gateway first, to avoid keeping mappings that were removed
	sn := structs.NewServiceName(conf.GetName(), entMeta)

	if _, err := tx.DeleteAll(tableGatewayServices, "gateway", sn); err != nil {
		return fmt.Errorf("failed to truncate gateway services table: %v", err)
	}
	if err := truncateGatewayServiceTopologyMappings(tx, idx, sn, conf.GetKind()); err != nil {
		return fmt.Errorf("failed to truncate mesh topology for gateway: %v", err)
	}

	for _, svc := range gatewayServices {
		// If the service is a wildcard we need to target all services within the namespace
		if svc.Service.Name == structs.WildcardSpecifier {
			if err := updateGatewayNamespace(tx, idx, svc, entMeta); err != nil {
				return fmt.Errorf("failed to associate gateway %q with wildcard: %v", gateway.String(), err)
			}
			// Skip service-specific update below if there was a wildcard update
			continue
		}

		// Since this service was specified on its own, and not with a wildcard,
		// if there is an existing entry, we overwrite it. The service entry is the source of truth.
		//
		// By extension, if TLS creds are provided with a wildcard but are not provided in
		// the service entry, the service does not inherit the creds from the wildcard.
		err = updateGatewayService(tx, idx, svc)
		if err != nil {
			return err
		}
	}

	if err := indexUpdateMaxTxn(tx, idx, tableGatewayServices); err != nil {
		return fmt.Errorf("failed updating gateway-services index: %v", err)
	}
	return nil
}

// ingressConfigGatewayServices constructs a list of GatewayService structs for
// insertion into the memdb table, specific to ingress gateways. The boolean
// returned indicates that there are no changes necessary to the memdb table.
func ingressConfigGatewayServices(
	tx ReadTxn,
	gateway structs.ServiceName,
	conf structs.ConfigEntry,
	entMeta *structs.EnterpriseMeta,
) (bool, structs.GatewayServices, error) {
	entry, ok := conf.(*structs.IngressGatewayConfigEntry)
	if !ok {
		return false, nil, fmt.Errorf("unexpected config entry type: %T", conf)
	}

	// Check if service list matches the last known list for the config entry, if it does, skip the update
	_, c, err := configEntryTxn(tx, nil, conf.GetKind(), conf.GetName(), entMeta)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get config entry: %v", err)
	}
	if cfg, ok := c.(*structs.IngressGatewayConfigEntry); ok && cfg != nil {
		if reflect.DeepEqual(cfg.Listeners, entry.Listeners) {
			// Services are the same, nothing to update
			return true, nil, nil
		}
	}

	var gatewayServices structs.GatewayServices
	for _, listener := range entry.Listeners {
		for _, service := range listener.Services {
			mapping := &structs.GatewayService{
				Gateway:     gateway,
				Service:     service.ToServiceName(),
				GatewayKind: structs.ServiceKindIngressGateway,
				Hosts:       service.Hosts,
				Port:        listener.Port,
				Protocol:    listener.Protocol,
			}

			gatewayServices = append(gatewayServices, mapping)
		}
	}
	return false, gatewayServices, nil
}

// terminatingConfigGatewayServices constructs a list of GatewayService structs
// for insertion into the memdb table, specific to terminating gateways. The
// boolean returned indicates that there are no changes necessary to the memdb
// table.
func terminatingConfigGatewayServices(
	tx ReadTxn,
	gateway structs.ServiceName,
	conf structs.ConfigEntry,
	entMeta *structs.EnterpriseMeta,
) (bool, structs.GatewayServices, error) {
	entry, ok := conf.(*structs.TerminatingGatewayConfigEntry)
	if !ok {
		return false, nil, fmt.Errorf("unexpected config entry type: %T", conf)
	}

	// Check if service list matches the last known list for the config entry, if it does, skip the update
	_, c, err := configEntryTxn(tx, nil, conf.GetKind(), conf.GetName(), entMeta)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get config entry: %v", err)
	}
	if cfg, ok := c.(*structs.TerminatingGatewayConfigEntry); ok && cfg != nil {
		if reflect.DeepEqual(cfg.Services, entry.Services) {
			// Services are the same, nothing to update
			return true, nil, nil
		}
	}

	var gatewayServices structs.GatewayServices
	for _, svc := range entry.Services {
		mapping := &structs.GatewayService{
			Gateway:     gateway,
			Service:     structs.NewServiceName(svc.Name, &svc.EnterpriseMeta),
			GatewayKind: structs.ServiceKindTerminatingGateway,
			KeyFile:     svc.KeyFile,
			CertFile:    svc.CertFile,
			CAFile:      svc.CAFile,
			SNI:         svc.SNI,
		}

		gatewayServices = append(gatewayServices, mapping)
	}
	return false, gatewayServices, nil
}

// updateGatewayNamespace is used to target all services within a namespace
func updateGatewayNamespace(tx WriteTxn, idx uint64, service *structs.GatewayService, entMeta *structs.EnterpriseMeta) error {
	services, err := catalogServiceListByKind(tx, structs.ServiceKindTypical, entMeta)
	if err != nil {
		return fmt.Errorf("failed querying services: %s", err)
	}

	// Iterate over services in namespace and insert mapping for each
	for svc := services.Next(); svc != nil; svc = services.Next() {
		sn := svc.(*structs.ServiceNode)

		// Only associate non-consul services with gateways
		if sn.ServiceName == "consul" {
			continue
		}

		existing, err := tx.First(tableGatewayServices, "id", service.Gateway, sn.CompoundServiceName(), service.Port)
		if err != nil {
			return fmt.Errorf("gateway service lookup failed: %s", err)
		}
		if existing != nil {
			// If there's an existing service associated with this gateway then we skip it.
			// This means the service was specified on its own, and the service entry overrides the wildcard entry.
			continue
		}

		mapping := service.Clone()

		mapping.Service = structs.NewServiceName(sn.ServiceName, &service.Service.EnterpriseMeta)
		mapping.FromWildcard = true

		err = updateGatewayService(tx, idx, mapping)
		if err != nil {
			return err
		}
	}

	// Also store a mapping for the wildcard so that the TLS creds can be pulled
	// for new services registered in its namespace
	err = updateGatewayService(tx, idx, service)
	if err != nil {
		return err
	}
	return nil
}

// updateGatewayService associates services with gateways after an eligible event
// ie. Registering a service in a namespace targeted by a gateway
func updateGatewayService(tx WriteTxn, idx uint64, mapping *structs.GatewayService) error {
	// Check if mapping already exists in table if it's already in the table
	// Avoid insert if nothing changed
	existing, err := tx.First(tableGatewayServices, "id", mapping.Gateway, mapping.Service, mapping.Port)
	if err != nil {
		return fmt.Errorf("gateway service lookup failed: %s", err)
	}
	if gs, ok := existing.(*structs.GatewayService); ok && gs != nil {
		mapping.CreateIndex = gs.CreateIndex
		if gs.IsSame(mapping) {
			return nil
		}
	} else {
		// We have a new mapping
		mapping.CreateIndex = idx
	}
	mapping.ModifyIndex = idx

	if err := tx.Insert(tableGatewayServices, mapping); err != nil {
		return fmt.Errorf("failed inserting gateway service mapping: %s", err)
	}

	if err := indexUpdateMaxTxn(tx, idx, tableGatewayServices); err != nil {
		return fmt.Errorf("failed updating gateway-services index: %v", err)
	}

	if err := insertGatewayServiceTopologyMapping(tx, idx, mapping); err != nil {
		return fmt.Errorf("failed to reconcile mesh topology for gateway: %v", err)
	}
	return nil
}

// checkWildcardForGatewaysAndUpdate checks whether a service matches a
// wildcard definition in gateway config entries and if so adds it the the
// gateway-services table.
func checkGatewayWildcardsAndUpdate(tx WriteTxn, idx uint64, svc *structs.NodeService) error {
	// Do not associate non-typical services with gateways or consul services
	if svc.Kind != structs.ServiceKindTypical || svc.Service == "consul" {
		return nil
	}

	svcGateways, err := serviceGateways(tx, structs.WildcardSpecifier, &svc.EnterpriseMeta)
	if err != nil {
		return fmt.Errorf("failed gateway lookup for %q: %s", svc.Service, err)
	}
	for service := svcGateways.Next(); service != nil; service = svcGateways.Next() {
		if wildcardSvc, ok := service.(*structs.GatewayService); ok && wildcardSvc != nil {

			// Copy the wildcard mapping and modify it
			gatewaySvc := wildcardSvc.Clone()

			gatewaySvc.Service = structs.NewServiceName(svc.Service, &svc.EnterpriseMeta)
			gatewaySvc.FromWildcard = true

			if err = updateGatewayService(tx, idx, gatewaySvc); err != nil {
				return fmt.Errorf("Failed to associate service %q with gateway %q", gatewaySvc.Service.String(), gatewaySvc.Gateway.String())
			}
		}
	}
	return nil
}

func cleanupGatewayWildcards(tx WriteTxn, idx uint64, svc *structs.ServiceNode) error {
	// Clean up association between service name and gateways if needed
	gateways, err := serviceGateways(tx, svc.ServiceName, &svc.EnterpriseMeta)
	if err != nil {
		return fmt.Errorf("failed gateway lookup for %q: %s", svc.ServiceName, err)
	}

	mappings := make([]*structs.GatewayService, 0)
	for mapping := gateways.Next(); mapping != nil; mapping = gateways.Next() {
		if gs, ok := mapping.(*structs.GatewayService); ok && gs != nil {
			mappings = append(mappings, gs)
		}
	}

	// Do the updates in a separate loop so we don't trash the iterator.
	for _, m := range mappings {
		// Only delete if association was created by a wildcard specifier.
		// Otherwise the service was specified in the config entry, and the association should be maintained
		// for when the service is re-registered
		if m.FromWildcard {
			if err := tx.Delete(tableGatewayServices, m); err != nil {
				return fmt.Errorf("failed to truncate gateway services table: %v", err)
			}
			if err := indexUpdateMaxTxn(tx, idx, tableGatewayServices); err != nil {
				return fmt.Errorf("failed updating gateway-services index: %v", err)
			}
			if err := deleteGatewayServiceTopologyMapping(tx, idx, m); err != nil {
				return fmt.Errorf("failed to reconcile mesh topology for gateway: %v", err)
			}
		}
	}
	return nil
}

// serviceGateways returns all GatewayService entries with the given service name. This effectively looks up
// all the gateways mapped to this service.
func serviceGateways(tx ReadTxn, name string, entMeta *structs.EnterpriseMeta) (memdb.ResultIterator, error) {
	return tx.Get(tableGatewayServices, "service", structs.NewServiceName(name, entMeta))
}

func gatewayServices(tx ReadTxn, name string, entMeta *structs.EnterpriseMeta) (memdb.ResultIterator, error) {
	return tx.Get(tableGatewayServices, "gateway", structs.NewServiceName(name, entMeta))
}

func (s *Store) DumpGatewayServices(ws memdb.WatchSet) (uint64, structs.GatewayServices, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	iter, err := tx.Get(tableGatewayServices, "id")
	if err != nil {
		return 0, nil, fmt.Errorf("failed to dump gateway-services: %s", err)
	}
	ws.Add(iter.WatchCh())

	maxIdx, results, err := s.collectGatewayServices(tx, ws, iter)
	if err != nil {
		return 0, nil, err
	}
	idx := maxIndexTxn(tx, tableGatewayServices)

	return lib.MaxUint64(maxIdx, idx), results, nil
}

func (s *Store) collectGatewayServices(tx ReadTxn, ws memdb.WatchSet, iter memdb.ResultIterator) (uint64, structs.GatewayServices, error) {
	var maxIdx uint64
	var results structs.GatewayServices

	for obj := iter.Next(); obj != nil; obj = iter.Next() {
		gs := obj.(*structs.GatewayService)
		maxIdx = lib.MaxUint64(maxIdx, gs.ModifyIndex)

		if gs.Service.Name != structs.WildcardSpecifier {
			idx, matches, err := checkProtocolMatch(tx, ws, gs)
			if err != nil {
				return 0, nil, fmt.Errorf("failed checking protocol: %s", err)
			}
			maxIdx = lib.MaxUint64(maxIdx, idx)

			if matches {
				results = append(results, gs)
			}
		}
	}
	return maxIdx, results, nil
}

// TODO(ingress): How to handle index rolling back when a config entry is
// deleted that references a service?
// We might need something like the service_last_extinction index?
func serviceGatewayNodes(tx ReadTxn, ws memdb.WatchSet, service string, kind structs.ServiceKind, entMeta *structs.EnterpriseMeta) (uint64, structs.ServiceNodes, error) {
	// Look up gateway name associated with the service
	gws, err := serviceGateways(tx, service, entMeta)
	if err != nil {
		return 0, nil, fmt.Errorf("failed gateway lookup: %s", err)
	}

	// Adding this channel to the WatchSet means that the watch will fire if a config entry targeting the service is added.
	// Otherwise, if there's no associated gateway, then no watch channel would be returned
	ws.Add(gws.WatchCh())

	var ret structs.ServiceNodes
	var maxIdx uint64

	for gateway := gws.Next(); gateway != nil; gateway = gws.Next() {
		mapping := gateway.(*structs.GatewayService)
		// TODO(ingress): Test this conditional
		if mapping.GatewayKind != kind {
			continue
		}
		maxIdx = lib.MaxUint64(maxIdx, mapping.ModifyIndex)

		// Look up nodes for gateway
		gwServices, err := catalogServiceNodeList(tx, mapping.Gateway.Name, "service", &mapping.Gateway.EnterpriseMeta)
		if err != nil {
			return 0, nil, fmt.Errorf("failed service lookup: %s", err)
		}

		var exists bool
		for svc := gwServices.Next(); svc != nil; svc = gwServices.Next() {
			sn := svc.(*structs.ServiceNode)
			ret = append(ret, sn)

			// Tracking existence to know whether we should check extinction index for service
			exists = true
		}

		// This prevents the index from sliding back if case all instances of the gateway service are deregistered
		svcIdx := maxIndexForService(tx, mapping.Gateway.Name, exists, false, &mapping.Gateway.EnterpriseMeta)
		maxIdx = lib.MaxUint64(maxIdx, svcIdx)

		// Ensure that blocking queries wake up if the gateway-service mapping exists, but the gateway does not exist yet
		if !exists {
			ws.Add(gwServices.WatchCh())
		}
	}
	return maxIdx, ret, nil
}

// metricsProtocolForIngressGateway determines the protocol that should be used when fetching metrics for an ingress gateway
// Since ingress gateways may have listeners with different protocols, favor capturing all traffic by only returning HTTP
// when all listeners are HTTP-like.
func metricsProtocolForIngressGateway(tx ReadTxn, ws memdb.WatchSet, sn structs.ServiceName) (uint64, string, error) {
	idx, conf, err := configEntryTxn(tx, ws, structs.IngressGateway, sn.Name, &sn.EnterpriseMeta)
	if err != nil {
		return 0, "", fmt.Errorf("failed to get ingress-gateway config entry for %q: %v", sn.String(), err)
	}
	if conf == nil {
		return 0, "", nil
	}
	entry, ok := conf.(*structs.IngressGatewayConfigEntry)
	if !ok {
		return 0, "", fmt.Errorf("unexpected config entry type: %T", conf)
	}
	counts := make(map[string]int)
	for _, l := range entry.Listeners {
		if structs.IsProtocolHTTPLike(l.Protocol) {
			counts["http"] += 1
		} else {
			counts["tcp"] += 1
		}
	}
	protocol := "tcp"
	if counts["tcp"] == 0 && counts["http"] > 0 {
		protocol = "http"
	}
	return idx, protocol, nil
}

// checkProtocolMatch filters out any GatewayService entries added from a wildcard with a protocol
// that doesn't match the one configured in their discovery chain.
func checkProtocolMatch(tx ReadTxn, ws memdb.WatchSet, svc *structs.GatewayService) (uint64, bool, error) {
	if svc.GatewayKind != structs.ServiceKindIngressGateway || !svc.FromWildcard {
		return 0, true, nil
	}

	idx, protocol, err := protocolForService(tx, ws, svc.Service)
	if err != nil {
		return 0, false, err
	}

	return idx, svc.Protocol == protocol, nil
}

func (s *Store) ServiceTopology(
	ws memdb.WatchSet,
	dc, service string,
	kind structs.ServiceKind,
	defaultAllow acl.EnforcementDecision,
	entMeta *structs.EnterpriseMeta,
) (uint64, *structs.ServiceTopology, error) {
	tx := s.db.ReadTxn()
	defer tx.Abort()

	var (
		maxIdx   uint64
		protocol string
		err      error

		sn = structs.NewServiceName(service, entMeta)
	)

	switch kind {
	case structs.ServiceKindIngressGateway:
		maxIdx, protocol, err = metricsProtocolForIngressGateway(tx, ws, sn)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to fetch protocol for service %s: %v", sn.String(), err)
		}

	case structs.ServiceKindTypical:
		maxIdx, protocol, err = protocolForService(tx, ws, sn)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to fetch protocol for service %s: %v", sn.String(), err)
		}

	default:
		return 0, nil, fmt.Errorf("unsupported kind %q", kind)
	}

	idx, upstreamNames, err := upstreamsFromRegistrationTxn(tx, ws, sn)
	if err != nil {
		return 0, nil, err
	}
	if idx > maxIdx {
		maxIdx = idx
	}
	idx, upstreams, err := s.combinedServiceNodesTxn(tx, ws, upstreamNames)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get upstreams for %q: %v", sn.String(), err)
	}
	if idx > maxIdx {
		maxIdx = idx
	}

	upstreamDecisions := make(map[string]structs.IntentionDecisionSummary)

	// The given service is the source relative to upstreams
	sourceURI := connect.SpiffeIDService{
		Namespace: entMeta.NamespaceOrDefault(),
		Service:   service,
	}
	for _, un := range upstreamNames {
		decision, err := s.IntentionDecision(&sourceURI, un.Name, un.NamespaceOrDefault(), defaultAllow)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to get intention decision from (%s/%s) to (%s/%s): %v",
				sourceURI.Namespace, sourceURI.Service, un.Name, un.NamespaceOrDefault(), err)
		}
		upstreamDecisions[un.String()] = decision
	}

	idx, downstreamNames, err := s.downstreamsForServiceTxn(tx, ws, dc, sn)
	if err != nil {
		return 0, nil, err
	}
	if idx > maxIdx {
		maxIdx = idx
	}
	idx, downstreams, err := s.combinedServiceNodesTxn(tx, ws, downstreamNames)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get downstreams for %q: %v", sn.String(), err)
	}
	if idx > maxIdx {
		maxIdx = idx
	}

	downstreamDecisions := make(map[string]structs.IntentionDecisionSummary)
	for _, dn := range downstreamNames {
		// Downstreams are the source relative to the given service
		sourceURI := connect.SpiffeIDService{
			Namespace: dn.NamespaceOrDefault(),
			Service:   dn.Name,
		}
		decision, err := s.IntentionDecision(&sourceURI, service, entMeta.NamespaceOrDefault(), defaultAllow)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to get intention decision from (%s/%s) to (%s/%s): %v",
				sourceURI.Namespace, sourceURI.Service, service, dn.NamespaceOrDefault(), err)
		}
		downstreamDecisions[dn.String()] = decision
	}

	resp := &structs.ServiceTopology{
		MetricsProtocol:     protocol,
		Upstreams:           upstreams,
		Downstreams:         downstreams,
		UpstreamDecisions:   upstreamDecisions,
		DownstreamDecisions: downstreamDecisions,
	}
	return maxIdx, resp, nil
}

// combinedServiceNodesTxn returns typical and connect endpoints for a list of services.
// This enabled aggregating checks statuses across both.
func (s *Store) combinedServiceNodesTxn(tx ReadTxn, ws memdb.WatchSet, names []structs.ServiceName) (uint64, structs.CheckServiceNodes, error) {
	var (
		maxIdx uint64
		resp   structs.CheckServiceNodes
	)
	for _, u := range names {
		// Collect typical then connect instances
		idx, csn, err := checkServiceNodesTxn(tx, ws, u.Name, false, &u.EnterpriseMeta)
		if err != nil {
			return 0, nil, err
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		resp = append(resp, csn...)

		idx, csn, err = checkServiceNodesTxn(tx, ws, u.Name, true, &u.EnterpriseMeta)
		if err != nil {
			return 0, nil, err
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		resp = append(resp, csn...)
	}
	return maxIdx, resp, nil
}

// downstreamsForServiceTxn will find all downstream services that could route traffic to the input service.
// There are two factors at play. Upstreams defined in a proxy registration, and the discovery chain for those upstreams.
// TODO (freddy): Account for ingress gateways
func (s *Store) downstreamsForServiceTxn(tx ReadTxn, ws memdb.WatchSet, dc string, service structs.ServiceName) (uint64, []structs.ServiceName, error) {
	// First fetch services that have discovery chains that eventually route to the target service
	idx, sources, err := s.discoveryChainSourcesTxn(tx, ws, dc, service)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get sources for discovery chain target %q: %v", service.String(), err)
	}

	var maxIdx uint64
	if idx > maxIdx {
		maxIdx = idx
	}

	var (
		resp []structs.ServiceName
		seen = make(map[structs.ServiceName]bool)
	)
	for _, s := range sources {
		// We then follow these sources one level down to the services defining them as an upstream.
		idx, downstreams, err := downstreamsFromRegistrationTxn(tx, ws, s)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to get registration downstreams for %q: %v", s.String(), err)
		}
		if idx > maxIdx {
			maxIdx = idx
		}
		for _, d := range downstreams {
			if !seen[d] {
				resp = append(resp, d)
				seen[d] = true
			}
		}
	}
	return maxIdx, resp, nil
}

// upstreamsFromRegistrationTxn returns the ServiceNames of the upstreams defined across instances of the input
func upstreamsFromRegistrationTxn(tx ReadTxn, ws memdb.WatchSet, sn structs.ServiceName) (uint64, []structs.ServiceName, error) {
	return linkedFromRegistrationTxn(tx, ws, sn, false)
}

// downstreamsFromRegistrationTxn returns the ServiceNames of downstream services based on registrations across instances of the input
func downstreamsFromRegistrationTxn(tx ReadTxn, ws memdb.WatchSet, sn structs.ServiceName) (uint64, []structs.ServiceName, error) {
	return linkedFromRegistrationTxn(tx, ws, sn, true)
}

func linkedFromRegistrationTxn(tx ReadTxn, ws memdb.WatchSet, service structs.ServiceName, downstreams bool) (uint64, []structs.ServiceName, error) {
	// To fetch upstreams we query services that have the input listed as a downstream
	// To fetch downstreams we query services that have the input listed as an upstream
	index := "downstream"
	if downstreams {
		index = "upstream"
	}

	iter, err := tx.Get(tableMeshTopology, index, service)
	if err != nil {
		return 0, nil, fmt.Errorf("%q lookup failed: %v", tableMeshTopology, err)
	}
	ws.Add(iter.WatchCh())

	var (
		idx  uint64
		resp []structs.ServiceName
	)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		entry := raw.(*structs.UpstreamDownstream)
		if entry.ModifyIndex > idx {
			idx = entry.ModifyIndex
		}

		linked := entry.Upstream
		if downstreams {
			linked = entry.Downstream
		}
		resp = append(resp, linked)
	}

	// TODO (freddy) This needs a tombstone to avoid the index sliding back on mapping deletion
	//  Using the table index here means that blocking queries will wake up more often than they should
	tableIdx := maxIndexTxn(tx, tableMeshTopology)
	if tableIdx > idx {
		idx = tableIdx
	}
	return idx, resp, nil
}

// updateMeshTopology creates associations between the input service and its upstreams in the topology table
func updateMeshTopology(tx WriteTxn, idx uint64, node string, svc *structs.NodeService, existing interface{}) error {
	oldUpstreams := make(map[structs.ServiceName]bool)
	if e, ok := existing.(*structs.ServiceNode); ok {
		for _, u := range e.ServiceProxy.Upstreams {
			upstreamMeta := structs.NewEnterpriseMeta(u.DestinationNamespace)
			sn := structs.NewServiceName(u.DestinationName, &upstreamMeta)

			oldUpstreams[sn] = true
		}
	}

	// Despite the name "destination", this service name is downstream of the proxy
	downstream := structs.NewServiceName(svc.Proxy.DestinationServiceName, &svc.EnterpriseMeta)
	inserted := make(map[structs.ServiceName]bool)
	for _, u := range svc.Proxy.Upstreams {
		if u.DestinationType == structs.UpstreamDestTypePreparedQuery {
			continue
		}

		// TODO (freddy): Account for upstream datacenter
		upstreamMeta := structs.NewEnterpriseMeta(u.DestinationNamespace)
		upstream := structs.NewServiceName(u.DestinationName, &upstreamMeta)

		obj, err := tx.First(tableMeshTopology, "id", upstream, downstream)
		if err != nil {
			return fmt.Errorf("%q lookup failed: %v", tableMeshTopology, err)
		}
		sid := svc.CompoundServiceID()
		uid := structs.UniqueID(node, sid.String())

		var mapping *structs.UpstreamDownstream
		if existing, ok := obj.(*structs.UpstreamDownstream); ok {
			rawCopy, err := copystructure.Copy(existing)
			if err != nil {
				return fmt.Errorf("failed to copy existing topology mapping: %v", err)
			}
			mapping, ok = rawCopy.(*structs.UpstreamDownstream)
			if !ok {
				return fmt.Errorf("unexpected topology type %T", rawCopy)
			}
			mapping.Refs[uid] = struct{}{}
			mapping.ModifyIndex = idx

			inserted[upstream] = true
		}
		if mapping == nil {
			mapping = &structs.UpstreamDownstream{
				Upstream:   upstream,
				Downstream: downstream,
				Refs:       map[string]struct{}{uid: {}},
				RaftIndex: structs.RaftIndex{
					CreateIndex: idx,
					ModifyIndex: idx,
				},
			}
		}
		if err := tx.Insert(tableMeshTopology, mapping); err != nil {
			return fmt.Errorf("failed inserting %s mapping: %s", tableMeshTopology, err)
		}
		if err := indexUpdateMaxTxn(tx, idx, tableMeshTopology); err != nil {
			return fmt.Errorf("failed updating %s index: %v", tableMeshTopology, err)
		}
		inserted[upstream] = true
	}

	for u := range oldUpstreams {
		if !inserted[u] {
			if _, err := tx.DeleteAll(tableMeshTopology, "id", u, downstream); err != nil {
				return fmt.Errorf("failed to truncate %s table: %v", tableMeshTopology, err)
			}
			if err := indexUpdateMaxTxn(tx, idx, tableMeshTopology); err != nil {
				return fmt.Errorf("failed updating %s index: %v", tableMeshTopology, err)
			}
		}
	}
	return nil
}

// cleanupMeshTopology removes a service from the mesh topology table
// This is only safe to call when there are no more known instances of this proxy
func cleanupMeshTopology(tx WriteTxn, idx uint64, service *structs.ServiceNode) error {
	if service.ServiceKind != structs.ServiceKindConnectProxy {
		return nil
	}
	sn := structs.NewServiceName(service.ServiceProxy.DestinationServiceName, &service.EnterpriseMeta)

	sid := service.CompoundServiceID()
	uid := structs.UniqueID(service.Node, sid.String())

	iter, err := tx.Get(tableMeshTopology, "downstream", sn)
	if err != nil {
		return fmt.Errorf("%q lookup failed: %v", tableMeshTopology, err)
	}

	mappings := make([]*structs.UpstreamDownstream, 0)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		mappings = append(mappings, raw.(*structs.UpstreamDownstream))
	}

	// Do the updates in a separate loop so we don't trash the iterator.
	for _, m := range mappings {
		rawCopy, err := copystructure.Copy(m)
		if err != nil {
			return fmt.Errorf("failed to copy existing topology mapping: %v", err)
		}
		copy, ok := rawCopy.(*structs.UpstreamDownstream)
		if !ok {
			return fmt.Errorf("unexpected topology type %T", rawCopy)
		}

		// Bail early if there's no reference to the proxy ID we're deleting
		if _, ok := copy.Refs[uid]; !ok {
			continue
		}

		delete(copy.Refs, uid)
		if len(copy.Refs) == 0 {
			if err := tx.Delete(tableMeshTopology, m); err != nil {
				return fmt.Errorf("failed to truncate %s table: %v", tableMeshTopology, err)
			}
			if err := indexUpdateMaxTxn(tx, idx, tableMeshTopology); err != nil {
				return fmt.Errorf("failed updating %s index: %v", tableMeshTopology, err)
			}
			continue

		}
		if err := tx.Insert(tableMeshTopology, copy); err != nil {
			return fmt.Errorf("failed inserting %s mapping: %s", tableMeshTopology, err)
		}
	}
	return nil
}

func insertGatewayServiceTopologyMapping(tx WriteTxn, idx uint64, gs *structs.GatewayService) error {
	// Only ingress gateways are standalone items in the mesh topology viz
	if gs.GatewayKind != structs.ServiceKindIngressGateway || gs.Service.Name == structs.WildcardSpecifier {
		return nil
	}

	mapping := structs.UpstreamDownstream{
		Upstream:   gs.Service,
		Downstream: gs.Gateway,
		RaftIndex:  gs.RaftIndex,
	}
	if err := tx.Insert(tableMeshTopology, &mapping); err != nil {
		return fmt.Errorf("failed inserting %s mapping: %s", tableMeshTopology, err)
	}
	if err := indexUpdateMaxTxn(tx, idx, tableMeshTopology); err != nil {
		return fmt.Errorf("failed updating %s index: %v", tableMeshTopology, err)
	}

	return nil
}

func deleteGatewayServiceTopologyMapping(tx WriteTxn, idx uint64, gs *structs.GatewayService) error {
	// Only ingress gateways are standalone items in the mesh topology viz
	if gs.GatewayKind != structs.ServiceKindIngressGateway {
		return nil
	}

	if _, err := tx.DeleteAll(tableMeshTopology, "id", gs.Service, gs.Gateway); err != nil {
		return fmt.Errorf("failed to truncate %s table: %v", tableMeshTopology, err)
	}
	if err := indexUpdateMaxTxn(tx, idx, tableMeshTopology); err != nil {
		return fmt.Errorf("failed updating %s index: %v", tableMeshTopology, err)
	}

	return nil
}

func truncateGatewayServiceTopologyMappings(tx WriteTxn, idx uint64, gateway structs.ServiceName, kind string) error {
	// Only ingress gateways are standalone items in the mesh topology viz
	if kind != string(structs.ServiceKindIngressGateway) {
		return nil
	}

	if _, err := tx.DeleteAll(tableMeshTopology, "downstream", gateway); err != nil {
		return fmt.Errorf("failed to truncate %s table: %v", tableMeshTopology, err)
	}
	if err := indexUpdateMaxTxn(tx, idx, tableMeshTopology); err != nil {
		return fmt.Errorf("failed updating %s index: %v", tableMeshTopology, err)
	}

	return nil
}
