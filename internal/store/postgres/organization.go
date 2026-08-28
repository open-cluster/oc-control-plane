package storage

import (
	"context"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// OrganizationExists reports whether durable identity state names the Organization.
func (d *Database) OrganizationExists(
	ctx context.Context, organization tenancy.Organization,
) (bool, error) {
	pool, err := d.Pool(organization)
	if err != nil {
		return false, err
	}
	var exists bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM organization_membership WHERE org_id = $1
	)`, organization.String()).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("resolve organization: %w", err)
	}
	return exists, nil
}
