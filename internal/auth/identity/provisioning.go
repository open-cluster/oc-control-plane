package identity

import "github.com/open-cluster/oc-control-plane/internal/auth/authz"

type admission struct {
	Role authz.Role
}
