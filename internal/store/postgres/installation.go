package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// THE ROUTING RECORD AN INBOUND EVENT RESOLVES THROUGH.
//
// It is written in the SAME TRANSACTION as the Integration it belongs to. The alternative
// considered — a hook the provider runs after the Integration is recorded — keeps the shared
// connect flow free of any vendor vocabulary and buys that with atomicity, and the failure it
// permits has no good answer: a customer who pressed Connect, authorized in their workspace,
// and now holds a connected integration whose mentions silently never work.
//
// THE TABLE IS NEUTRAL, AND THAT IS WHAT KEEPS THIS FILE FREE OF ANY PROVIDER. Provider
// adapters own their ordered installation keys; persistence only enforces that each key is
// complete, matches its parent Integration, and has one owner deployment-wide.
//
// ErrInstallationTaken is what deployment-wide uniqueness produces. It is a refusal rather
// than a failure because resolving one provider installation to two tenants must be impossible.

// installationInsert is the one write both paths make. Written once because the two differ only
// in what they do about a row that already exists, and a second copy of the column list is a
// column added to one path and forgotten in the other.
const installationInsert = `
		INSERT INTO integration_installation
			(org_id, integration_id, provider, installation_key, provider_actor_id)
		VALUES ($1, $2, $3, $4, $5)`

// recordInstallation writes the routing record for a newly created Integration, inside the
// transaction that created it.
func recordInstallation(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
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

// recordInstallationIn writes or refreshes the routing record inside a transaction that is
// already changing the Integration.
//
// The RECONNECT path's half of the same story the create path tells, and it is in that path's
// transaction for the same reason: authorizing again replaces the credential AND can issue a
// new agent identity, and a credential replaced without its routing refreshed is a live
// credential with stale routing — an agent that answers as somebody it no longer is.
func recordInstallationIn(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
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
	organization tenancy.Organization, integration uuid.UUID,
	provider integrations.Provider, installed integrations.Installation,
) []any {
	return []any{
		organization.String(), integration, provider,
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

// IntegrationByInstallation resolves an inbound event's workspace to exactly one Integration
// and reports the organization it belongs to.
//
// It is the FIRST HOP and the only one that starts from a vendor's identifier. Everything after
// it is scoped by the organization this returns, which is what makes a vendor identifier from
// one tenant unable to reach another tenant's records: the lookup never starts from a vendor
// identifier alone, it starts from the deployment-unique installation key.
//
// Like IntegrationByID, it takes no organization, and for the same reason: an inbound caller
// names no tenant, because a caller who could name one could try every one.
func (p *Database) IntegrationByInstallation(
	ctx context.Context, provider integrations.Provider, key integrations.InstallationKey,
) (integrations.Integration, integrations.Installation, error) {
	if !key.Complete() {
		return integrations.Integration{}, integrations.Installation{}, integrations.ErrUnknown
	}

	var (
		organization    string
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

	organizationName, err := tenancy.NewOrganization(organization)
	if err != nil {
		return integrations.Integration{}, integrations.Installation{},
			fmt.Errorf("an installation names an organization that is not a name: %w", err)
	}
	integration, err := p.Integration(ctx, organizationName, integrationID)
	if err != nil {
		return integrations.Integration{}, integrations.Installation{}, err
	}
	return integration, installed, nil
}

// InstallationOf reports the routing record an Integration was connected with, and false where
// it has none. A pasted credential names no installation, which is exactly what tells an
// integration that can be spoken to from one that can only be read.
func (p *Database) InstallationOf(
	ctx context.Context, organization tenancy.Organization, integration uuid.UUID,
) (integrations.Installation, bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return integrations.Installation{}, false, err
	}
	var installed integrations.Installation
	var providerActor *string
	var installationKey []string
	err = pool.QueryRow(ctx, `
		SELECT installation_key, provider_actor_id
		  FROM integration_installation
		 WHERE integration_id = $1 AND org_id = $2`,
		integration, organization.String()).Scan(&installationKey, &providerActor)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return integrations.Installation{}, false, nil
	case err != nil:
		return integrations.Installation{}, false,
			fmt.Errorf("reading an integration installation: %w", err)
	}
	if providerActor != nil {
		installed.ProviderActorID = *providerActor
	}
	installed.Key = integrations.InstallationKey(installationKey)
	return installed, true, nil
}

func orEmptyGrants(grants []string) []string {
	if grants == nil {
		return []string{}
	}
	return grants
}
