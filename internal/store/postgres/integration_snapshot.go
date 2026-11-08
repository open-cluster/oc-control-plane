package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Lock the checked access settings until the mutation and its audit record commit.
func checkIntegrationSnapshot(ctx context.Context, transaction pgx.Tx, organization, id uuid.UUID, expectedIntegration *integrations.Integration) error {
	if expectedIntegration == nil {
		return nil
	}
	integration, err := scanIntegration(transaction.QueryRow(ctx,
		`SELECT `+integrationColumns+` FROM integration WHERE integration_id = $1 AND org_id = $2 FOR UPDATE`, id, organization), organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return integrations.ErrUnknown
	}
	if err != nil {
		return err
	}
	if integration.Status != expectedIntegration.Status || !integration.VerifiedAt.Equal(expectedIntegration.VerifiedAt) ||
		!reflect.DeepEqual(integration.VerificationGrants, expectedIntegration.VerificationGrants) ||
		!bytes.Equal(integration.WebhookSecretDigest, expectedIntegration.WebhookSecretDigest) ||
		integration.Disabled != expectedIntegration.Disabled || integration.RelayID != expectedIntegration.RelayID ||
		!bytes.Equal(integration.CredentialSealed, expectedIntegration.CredentialSealed) ||
		!reflect.DeepEqual(integration.Configuration, expectedIntegration.Configuration) ||
		!reflect.DeepEqual(integration.Installation, expectedIntegration.Installation) {
		return integrations.ErrConflict
	}
	return nil
}
