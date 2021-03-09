route "dc" {
  path = "/:dc"

  route "services" {
  	path = "/services"

  	route "show" {
  	  path = "/:name"
  	  route "instances" {
  	    path = "/instances"
  	  }
  	  route "intentions" {
  	    path = "/intentions"
  	    route "edit" {
  	      path = "/intention_id"
  	    }
  	    route "create" {
  	      path = "/create"
  	    }
  	  }
  	  route "topology" {
  	    path = "/topology"
  	  }
  	  route "services" {
  	    path = "/services"
  	  }
  	  route "upstreams" {
  	    path = "/upstreams"
  	  }
  	  route "routing" {
  	    path = "/routing"
  	  }
  	  route "tags" {
  	    path = "/tags"
  	  }
  	}

  	route "instance" {
  	  path = "/:name/instances/:node/:id"
  	  route "healthchecks" {
  	    path = "/healthchecks"
  	  }
  	  route "upstreams" {
  	    path = "/upstreams"
  	  }
  	  route "exposedpaths" {
  	    path = "/exposed-paths"
  	  }
  	  route "addresses" {
  	    path = "/addresses"
  	  }
  	  route "metadata" {
  	    path = "/metadata"
  	  }
  	}
  	route "notfound" {
  	  path = "/:name/:node/:id"
  	}
  }

  route "nodes" {
  	path = "/nodes"
    route "show" {
  	  path = "/:name"
      route "healthchecks" {
  	    path = "/:health-checks"
      }
      route "services" {
  	    path = "/service-instances"
      }
      route "rtt" {
  	    path = "/round-trip-time"
      }
      route "sessions" {
  	    path = "/lock-sessions"
      }
      route "metadata" {
  	    path = "/metadata"
      }
    }
  }

  route "intentions" {
  	path = "/intentions"
    route "edit" {
  	  path = "/:intention_id"
  	  abilities = ['read intentions']
    }
    route "create" {
  	  path = "/create"
  	  abilities = ['create intentions']
    }
  }

  route "kv" {
  	path = "/kv"
    route "folder" {
  	  path = "/*key"
    }
    route "edit" {
  	  path = "/*key/edit"
    }
    route "create" {
  	  path = "/*key/create"
  	  abilities = ['create kvs']
    }
    route "root-create" {
  	  path = "/create"
  	  abilities = ['create kvs']
    }
  }

  route "acls" {
  	path = "/acls"
    route "edit" {
  	  path = "/:id"
    }
    route "create" {
  	  path = "/create"
    }

    route "policies" {
  	  path = "/policies"
      route "edit" {
  	    path = "/:id"
      }
      route "create" {
  	    path = "/create"
      }
    }

    route "roles" {
  	  path = "/roles"
      route "edit" {
  	    path = "/:id"
      }
      route "create" {
  	    path = "/create"
      }
    }

    route "tokens" {
  	  path = "/tokens"
      route "edit" {
  	    path = "/:id"
      }
      route "create" {
  	    path = "/create"
      }
    }

    route "auth-methods" {
  	  path = "/auth-methods"
      route "show" {
  	    path = "/show"
      }
    }

  }
}
route "index" {
  path = "/"
}

route "settings" {
  path = "/setting"
}

route "notfound" {
  path = "/*path"
}

