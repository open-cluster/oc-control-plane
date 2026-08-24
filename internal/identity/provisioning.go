package identity

import "github.com/open-cluster/oc-control-plane/internal/authz"

type admission struct {
	Role authz.Role
}
