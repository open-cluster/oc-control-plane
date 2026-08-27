package storage

import (
	"context"
	"fmt"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// ReconcileIntegrationTypes makes provider manifests authoritative for catalog metadata.
func (d *Database) ReconcileIntegrationTypes(
	ctx context.Context, manifests []integrations.Manifest,
) error {
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting integration catalog reconciliation: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	ids := make([]int16, 0, len(manifests))
	for _, manifest := range manifests {
		ids = append(ids, int16(manifest.ID))
		_, err = transaction.Exec(ctx, `
			INSERT INTO integration_type
				(integration_type_id, key, name, description, logo, category, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())
			ON CONFLICT (integration_type_id) DO UPDATE SET
				key = EXCLUDED.key, name = EXCLUDED.name,
				description = EXCLUDED.description, logo = EXCLUDED.logo,
				category = EXCLUDED.category, updated_at = now()`,
			int16(manifest.ID), manifest.Key, manifest.Name, manifest.Description,
			manifest.Logo, string(manifest.Category))
		if err != nil {
			return fmt.Errorf("reconciling integration manifest %q: %w", manifest.Key, err)
		}
	}
	if _, err = transaction.Exec(ctx, `DELETE FROM integration_type
		WHERE NOT (integration_type_id = ANY($1::smallint[]))`, ids); err != nil {
		return fmt.Errorf("removing integration types absent from manifests: %w", err)
	}
	if err = transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing integration catalog reconciliation: %w", err)
	}
	return nil
}
