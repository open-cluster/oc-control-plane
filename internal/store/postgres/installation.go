package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const installationInsert = `
		INSERT INTO integration_installation
			(org_id, integration_id, provider, installation_key, provider_actor_id)
		VALUES ($1, $2, $3, $4, $5)`

func recordInstallation(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	integration uuid.UUID, provider integrations.Provider, installed integrations.Installation,
) error {
	if !installed.Key.Complete() {
		return fmt.Errorf("%w: an installation key must contain only non-empty values",
			integrations.ErrInvalidInstallation)
	}
	_, err := transaction.Exec(ctx, installationInsert,
		installationValues(organization, integration, provider, installed)...)
	return installationError(err)
}

func recordInstallationIn(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID,
	integration uuid.UUID, provider integrations.Provider, installed integrations.Installation,
) error {
	if !installed.Key.Complete() {
		return fmt.Errorf("%w: an installation key must contain only non-empty values",
			integrations.ErrInvalidInstallation)
	}
	_, err := transaction.Exec(ctx, installationInsert+`
		ON CONFLICT (org_id, integration_id) DO UPDATE
		   SET provider          = EXCLUDED.provider,
		       installation_key = EXCLUDED.installation_key,
		       provider_actor_id = EXCLUDED.provider_actor_id`,
		installationValues(organization, integration, provider, installed)...)
	return installationError(err)
}

func installationValues(
	organization uuid.UUID, integration uuid.UUID,
	provider integrations.Provider, installed integrations.Installation,
) []any {
	return []any{
		organization, integration, provider,
		[]string(installed.Key), nullableText(installed.ProviderActorID),
	}
}

func installationError(err error) error {
	switch {
	case isUniqueViolation(err, "integration_installation_provider_key_unique"):
		return integrations.ErrInstallationTaken
	case err != nil:
		return fmt.Errorf("recording an integration installation: %w", err)
	}
	return nil
}

func (p *Database) IntegrationByInstallation(
	ctx context.Context, provider integrations.Provider, key integrations.InstallationKey,
) (integrations.Integration, integrations.Installation, error) {
	if !key.Complete() {
		return integrations.Integration{}, integrations.Installation{}, integrations.ErrUnknown
	}

	var (
		organization    uuid.UUID
		installed       integrations.Installation
		integrationID   uuid.UUID
		providerActor   *string
		installationKey []string
	)
	row := p.pool.QueryRow(ctx, `
			SELECT org_id, integration_id, installation_key, provider_actor_id
			  FROM integration_installation
			 WHERE provider = $1
			   AND installation_key = $2`,
		provider, []string(key))
	err := row.Scan(&organization, &integrationID, &installationKey, &providerActor)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return integrations.Integration{}, integrations.Installation{}, integrations.ErrUnknown
	case err != nil:
		return integrations.Integration{}, integrations.Installation{},
			fmt.Errorf("resolving an integration installation: %w", err)
	}
	if providerActor != nil {
		installed.ProviderActorID = *providerActor
	}
	installed.Key = integrations.InstallationKey(installationKey)

	integration, err := p.Integration(ctx, organization, integrationID)
	if err != nil {
		return integrations.Integration{}, integrations.Installation{}, err
	}
	return integration, installed, nil
}

func orEmptyGrants(grants []string) []string {
	if grants == nil {
		return []string{}
	}
	return grants
}
