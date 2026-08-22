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
// and now holds a connected integration whose mentions silently never work. This costs one
// dispatch from an Integration's type to the table that holds its installation, in this file,
// which is a price paid once and visible in one place.
//
// ErrWorkspaceTaken is what the deployment-wide uniqueness produces, and it is a REFUSAL
// rather than a failure: the workspace is already installed against another Integration, and
// resolving one event to two tenants is the thing the constraint exists to make impossible.

// recordInstallation writes the routing record for a newly created Integration, inside the
// transaction that created it.
//
// A type this build has no installation table for is a programming error rather than a
// no-op: it would mean a provider returned a routing key that nothing can route, and
// discovering that at the first inbound event — as silence — is the worst available time.
func recordInstallation(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	integration uuid.UUID, typeID integrations.TypeID, installed integrations.Installation,
) error {
	if !installed.Key().Complete() {
		return fmt.Errorf("%w: an installation must name an application and a workspace",
			integrations.ErrInvalidInstallation)
	}

	switch typeID {
	case integrations.TypeSlack:
		_, err := transaction.Exec(ctx, `
			INSERT INTO slack_installation
				(integration_id, org_id, app_id, enterprise_id, team_id,
				 is_enterprise_install, bot_user_id, authed_user_id, scopes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			integration, organization.String(),
			installed.Application, installed.Enterprise, installed.Workspace,
			installed.Enterprise != "", installed.Agent, installed.Authorizer,
			orEmptyGrants(installed.Grants))
		if isUniqueViolation(err, "slack_installation_is_one_workspace") {
			return integrations.ErrWorkspaceTaken
		}
		if err != nil {
			return fmt.Errorf("recording a slack installation: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: type %d has no installation record",
			integrations.ErrInvalidInstallation, typeID)
	}
}

// IntegrationByInstallation resolves an inbound event's workspace to exactly one Integration,
// across every placement this deployment serves, and reports the organization it belongs to.
//
// It is the FIRST HOP and the only one that starts from a vendor's identifier. Everything
// after it is scoped by the organization this returns, which is what makes a Slack identifier
// from one tenant unable to reach another tenant's records: the lookup never starts from a
// Slack identifier alone, it starts from the deployment-unique installation key.
//
// Like IntegrationByID, it takes no organization, and for the same reason: an inbound caller
// names no tenant, because a caller who could name one could try every one.
func (p *Placements) IntegrationByInstallation(
	ctx context.Context, typeID integrations.TypeID, key integrations.InstallationKey,
) (integrations.Integration, integrations.Installation, error) {
	if !key.Complete() {
		return integrations.Integration{}, integrations.Installation{}, integrations.ErrUnknown
	}
	if typeID != integrations.TypeSlack {
		return integrations.Integration{}, integrations.Installation{},
			fmt.Errorf("%w: type %d has no installation record",
				integrations.ErrInvalidInstallation, typeID)
	}

	for _, name := range p.names() {
		var (
			organization  string
			installed     integrations.Installation
			integrationID uuid.UUID
		)
		row := p.pools[name].QueryRow(ctx, `
			SELECT org_id, integration_id, app_id, enterprise_id, team_id,
			       bot_user_id, authed_user_id, scopes
			  FROM slack_installation
			 WHERE app_id = $1 AND enterprise_id = $2 AND team_id = $3`,
			key.Application, key.Enterprise, key.Workspace)
		err := row.Scan(&organization, &integrationID,
			&installed.Application, &installed.Enterprise, &installed.Workspace,
			&installed.Agent, &installed.Authorizer, &installed.Grants)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			continue
		case err != nil:
			return integrations.Integration{}, integrations.Installation{},
				fmt.Errorf("resolving a slack installation: %w", err)
		}

		organizationName, err := tenancy.NewOrganization(organization)
		if err != nil {
			return integrations.Integration{}, integrations.Installation{},
				fmt.Errorf("a slack installation names an organization that is not a name: %w",
					err)
		}
		integration, err := p.Integration(ctx, organizationName, integrationID)
		if err != nil {
			return integrations.Integration{}, integrations.Installation{}, err
		}
		return integration, installed, nil
	}
	return integrations.Integration{}, integrations.Installation{}, integrations.ErrUnknown
}

func orEmptyGrants(grants []string) []string {
	if grants == nil {
		return []string{}
	}
	return grants
}

// RecordInstallation writes or refreshes the routing record for an Integration that already
// exists.
//
// It is the RECONNECT path's half of the same story the create path tells. Authorizing again
// can change what the installation carries — a reinstall issues a new bot identity, and a
// re-authorization can widen or narrow the granted scopes — and the bot identity is what
// stops the agent answering its own messages, so a stale one is a loop rather than a cosmetic
// drift.
//
// The workspace key itself is part of what is written, so a reconnect that somehow named a
// workspace another Integration holds is refused here exactly as it would be at creation.
func (p *Placements) RecordInstallation(
	ctx context.Context, organization tenancy.Organization, integration uuid.UUID,
	typeID integrations.TypeID, installed integrations.Installation,
) error {
	if !installed.Key().Complete() {
		return fmt.Errorf("%w: an installation must name an application and a workspace",
			integrations.ErrInvalidInstallation)
	}
	if typeID != integrations.TypeSlack {
		return fmt.Errorf("%w: type %d has no installation record",
			integrations.ErrInvalidInstallation, typeID)
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO slack_installation
			(integration_id, org_id, app_id, enterprise_id, team_id,
			 is_enterprise_install, bot_user_id, authed_user_id, scopes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (integration_id) DO UPDATE
		   SET app_id                = EXCLUDED.app_id,
		       enterprise_id         = EXCLUDED.enterprise_id,
		       team_id               = EXCLUDED.team_id,
		       is_enterprise_install = EXCLUDED.is_enterprise_install,
		       bot_user_id           = EXCLUDED.bot_user_id,
		       authed_user_id        = EXCLUDED.authed_user_id,
		       scopes                = EXCLUDED.scopes,
		       updated_at            = now()`,
		integration, organization.String(),
		installed.Application, installed.Enterprise, installed.Workspace,
		installed.Enterprise != "", installed.Agent, installed.Authorizer,
		orEmptyGrants(installed.Grants))
	switch {
	case isUniqueViolation(err, "slack_installation_is_one_workspace"):
		return integrations.ErrWorkspaceTaken
	case err != nil:
		return fmt.Errorf("recording a slack installation: %w", err)
	}
	return nil
}
