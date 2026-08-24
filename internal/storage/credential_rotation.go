package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

// RewrapIntegrationCredentials advances one bounded deployment-wide page to the active key.
// Discovery crosses Organizations because key rotation is a deployment operation; every write
// still predicates on both org_id and integration_id.
func (d *Database) RewrapIntegrationCredentials(
	ctx context.Context, sealer seal.Sealer, limit int,
) (changed int, done bool, err error) {
	if !sealer.Configured() || limit <= 0 {
		return 0, false, errors.New("credential rotation requires a sealer and positive limit")
	}
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("beginning credential rotation: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	rows, err := transaction.Query(ctx, `
		SELECT org_id, integration_id, credential_sealed
		  FROM integration
		 WHERE credential_sealed IS NOT NULL
		   AND credential_key_id IS DISTINCT FROM $1
		 ORDER BY integration_id
		 FOR UPDATE
		 LIMIT $2`, sealer.ActiveKeyID(), limit)
	if err != nil {
		return 0, false, fmt.Errorf("reading credentials for rotation: %w", err)
	}
	type credential struct {
		organization string
		integration  uuid.UUID
		sealed       []byte
	}
	page := make([]credential, 0, limit)
	for rows.Next() {
		var item credential
		if err = rows.Scan(&item.organization, &item.integration, &item.sealed); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("scanning a credential for rotation: %w", err)
		}
		page = append(page, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, false, fmt.Errorf("reading credentials for rotation: %w", err)
	}
	rows.Close()
	done = len(page) < limit

	for _, item := range page {
		rewrapped, needsWrite, rewrapErr := sealer.Rewrap(item.sealed,
			integrations.CredentialBinding(item.integration))
		if rewrapErr != nil {
			return 0, false, fmt.Errorf("rewrapping integration credential: %w", rewrapErr)
		}
		if !needsWrite {
			continue
		}
		tag, updateErr := transaction.Exec(ctx, `
			UPDATE integration
			   SET credential_sealed = $3, credential_key_id = $4,
			       credential_rotated_at = now()
			 WHERE org_id = $1 AND integration_id = $2 AND credential_sealed = $5`,
			item.organization, item.integration, rewrapped, sealer.ActiveKeyID(), item.sealed)
		if updateErr != nil {
			return 0, false, fmt.Errorf("storing a rewrapped credential: %w", updateErr)
		}
		changed += int(tag.RowsAffected())
	}
	if err = transaction.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("committing credential rotation: %w", err)
	}
	return changed, done, nil
}
