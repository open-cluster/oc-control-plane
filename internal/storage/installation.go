package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE ROUTING RECORD AN INBOUND EVENT RESOLVES THROUGH.
//
// It is written in the SAME TRANSACTION as the Integration it belongs to. The alternative
// considered — a hook the provider runs after the Integration is recorded — keeps the shared
// connect flow free of any vendor vocabulary and buys that with atomicity, and the failure it
// permits has no good answer: a customer who pressed Connect, authorized in their workspace,
// and now holds a connected integration whose mentions silently never work.
//
// THE TABLE IS NEUTRAL, AND THAT IS WHAT KEEPS THIS FILE FREE OF ANY PROVIDER. One table per
// vendor would mean dispatching from an Integration's type to a table, and this repository
// permits no switch over integration types anywhere. What every inbound installation has in
// common is exactly the routing key and the identity we answer as, which is all this holds — so
// a second provider reuses it and adds no schema, the way integration_connect_flow already is.
//
// ErrWorkspaceTaken is what the deployment-wide uniqueness produces, and it is a REFUSAL rather
// than a failure: the workspace is already installed against another Integration, and resolving
// one event to two tenants is the thing the constraint exists to make impossible.

// installationInsert is the one write both paths make. Written once because the two differ only
// in what they do about a row that already exists, and a second copy of the column list is a
// column added to one path and forgotten in the other.
const installationInsert = `
		INSERT INTO integration_installation
			(integration_id, org_id, integration_type_id, application, enterprise,
			 workspace, enterprise_wide, agent, authorizer, grants)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// recordInstallation writes the routing record for a newly created Integration, inside the
// transaction that created it.
func recordInstallation(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	integration uuid.UUID, typeID integrations.TypeID, installed integrations.Installation,
) error {
	if !installed.Key().Complete() {
		return fmt.Errorf("%w: an installation must name an application and a workspace",
			integrations.ErrInvalidInstallation)
	}
	_, err := transaction.Exec(ctx, installationInsert,
		installationValues(organization, integration, typeID, installed)...)
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
	integration uuid.UUID, typeID integrations.TypeID, installed integrations.Installation,
) error {
	if !installed.Key().Complete() {
		return fmt.Errorf("%w: an installation must name an application and a workspace",
			integrations.ErrInvalidInstallation)
	}
	_, err := transaction.Exec(ctx, installationInsert+`
		ON CONFLICT (integration_id) DO UPDATE
		   SET integration_type_id = EXCLUDED.integration_type_id,
		       application         = EXCLUDED.application,
		       enterprise          = EXCLUDED.enterprise,
		       workspace           = EXCLUDED.workspace,
		       enterprise_wide     = EXCLUDED.enterprise_wide,
		       agent               = EXCLUDED.agent,
		       authorizer          = EXCLUDED.authorizer,
		       grants              = EXCLUDED.grants,
		       updated_at          = now()`,
		installationValues(organization, integration, typeID, installed)...)
	return installationError(err)
}

func installationValues(
	organization tenancy.Organization, integration uuid.UUID,
	typeID integrations.TypeID, installed integrations.Installation,
) []any {
	return []any{
		integration, organization.String(), int16(typeID),
		installed.Application, installed.Enterprise, installed.Workspace,
		installed.EnterpriseWide, installed.Agent, installed.Authorizer,
		orEmptyGrants(installed.Grants),
	}
}

func installationError(err error) error {
	switch {
	case isUniqueViolation(err, "integration_installation_is_one_workspace"):
		return integrations.ErrWorkspaceTaken
	case err != nil:
		return fmt.Errorf("recording an integration installation: %w", err)
	}
	return nil
}

// IntegrationByInstallation resolves an inbound event's workspace to exactly one Integration,
// across every placement this deployment serves, and reports the organization it belongs to.
//
// It is the FIRST HOP and the only one that starts from a vendor's identifier. Everything after
// it is scoped by the organization this returns, which is what makes a vendor identifier from
// one tenant unable to reach another tenant's records: the lookup never starts from a vendor
// identifier alone, it starts from the deployment-unique installation key.
//
// Like IntegrationByID, it takes no organization, and for the same reason: an inbound caller
// names no tenant, because a caller who could name one could try every one.
func (p *Placements) IntegrationByInstallation(
	ctx context.Context, typeID integrations.TypeID, key integrations.InstallationKey,
) (integrations.Integration, integrations.Installation, error) {
	if !key.Complete() {
		return integrations.Integration{}, integrations.Installation{}, integrations.ErrUnknown
	}

	for _, name := range p.names() {
		var (
			organization  string
			installed     integrations.Installation
			integrationID uuid.UUID
		)
		row := p.pools[name].QueryRow(ctx, `
			SELECT org_id, integration_id, application, enterprise, workspace,
			       enterprise_wide, agent, authorizer, grants
			  FROM integration_installation
			 WHERE integration_type_id = $1
			   AND application = $2 AND enterprise = $3 AND workspace = $4`,
			int16(typeID), key.Application, key.Enterprise, key.Workspace)
		err := row.Scan(&organization, &integrationID,
			&installed.Application, &installed.Enterprise, &installed.Workspace,
			&installed.EnterpriseWide, &installed.Agent, &installed.Authorizer,
			&installed.Grants)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			continue
		case err != nil:
			return integrations.Integration{}, integrations.Installation{},
				fmt.Errorf("resolving an integration installation: %w", err)
		}

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
	return integrations.Integration{}, integrations.Installation{}, integrations.ErrUnknown
}

// InstallationOf reports the routing record an Integration was connected with, and false where
// it has none. A pasted credential names no installation, which is exactly what tells an
// integration that can be spoken to from one that can only be read.
func (p *Placements) InstallationOf(
	ctx context.Context, organization tenancy.Organization, integration uuid.UUID,
) (integrations.Installation, bool, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return integrations.Installation{}, false, err
	}
	var installed integrations.Installation
	err = pool.QueryRow(ctx, `
		SELECT application, enterprise, workspace, enterprise_wide, agent, authorizer, grants
		  FROM integration_installation
		 WHERE integration_id = $1 AND org_id = $2`,
		integration, organization.String()).Scan(
		&installed.Application, &installed.Enterprise, &installed.Workspace,
		&installed.EnterpriseWide, &installed.Agent, &installed.Authorizer, &installed.Grants)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return integrations.Installation{}, false, nil
	case err != nil:
		return integrations.Installation{}, false,
			fmt.Errorf("reading an integration installation: %w", err)
	}
	return installed, true, nil
}

func orEmptyGrants(grants []string) []string {
	if grants == nil {
		return []string{}
	}
	return grants
}
