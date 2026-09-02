package agent

import (
	"context"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// DeploymentResolver selects the model deployment for an explicit Organization.
// The OSS composition is static; cloud compositions may resolve managed or encrypted
// organization-owned credentials without changing the investigation loop or Provider.
type DeploymentResolver interface {
	Resolve(context.Context, tenancy.Organization) (Deployment, error)
}

// StaticDeploymentResolver returns the process's mounted-file deployment.
type StaticDeploymentResolver struct {
	Deployment Deployment
}

func (r StaticDeploymentResolver) Resolve(
	_ context.Context, organization tenancy.Organization,
) (Deployment, error) {
	if organization.IsEmpty() {
		return Deployment{}, fmt.Errorf("resolving a model deployment requires an Organization")
	}
	return r.Deployment, nil
}
