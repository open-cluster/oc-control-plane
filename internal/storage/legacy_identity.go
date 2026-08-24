package storage

import (
	"context"
	"fmt"
)

// LegacyIdentityActive reports whether retained identity data would still have controlled
// sign-in or directory membership in the previous release.
func (p *Database) LegacyIdentityActive(ctx context.Context) (bool, error) {
	var active bool
	err := p.pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM identity_provider WHERE disabled_at IS NULL)
		OR EXISTS (SELECT 1 FROM scim_group WHERE role IS NOT NULL)`).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("checking retained identity configuration: %w", err)
	}
	return active, nil
}
