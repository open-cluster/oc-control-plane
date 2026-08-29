package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// ErrOrganizationExists reports an Organization name already held by durable truth.
var ErrOrganizationExists = errors.New("organization exists")

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
		SELECT 1 FROM organization WHERE org_id = $1
	)`, organization.String()).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("resolve organization: %w", err)
	}
	return exists, nil
}

// CreateOrganization records a new Organization and its creator's Admin membership atomically.
func (d *Database) CreateOrganization(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	displayName string,
) (authz.Membership, error) {
	userID, err := uuid.Parse(principal.ID())
	if err != nil || principal.Kind() != authz.KindUser {
		return authz.Membership{}, ErrUserUnknown
	}
	pool, err := d.Pool(organization)
	if err != nil {
		return authz.Membership{}, err
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return authz.Membership{}, fmt.Errorf("beginning organization creation: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	if _, err = transaction.Exec(ctx,
		`INSERT INTO organization (org_id, display_name, created_by) VALUES ($1, $2, $3)`,
		organization.String(), displayName, principal.ID()); err != nil {
		if isUniqueViolation(err, "organization_pkey") {
			return authz.Membership{}, ErrOrganizationExists
		}
		return authz.Membership{}, fmt.Errorf("creating organization: %w", err)
	}
	membershipID := uuid.New()
	if _, err = transaction.Exec(ctx, `
		INSERT INTO organization_membership
			(membership_id, org_id, user_id, role, source, granted_by)
		VALUES ($1, $2, $3, $4, $5, $6)`, membershipID, organization.String(), userID,
		string(authz.Admin), int16(SourceManual), principal.ID()); err != nil {
		return authz.Membership{}, fmt.Errorf("granting organization creator: %w", err)
	}
	if err = writeEvent(ctx, transaction, audit.Event{
		Organization: organization.String(),
		Actor:        principal.Actor(),
		Action:       audit.ActionMembershipGranted,
		Target:       audit.Target{Kind: audit.TargetMembership, ID: membershipID.String()},
		Outcome:      audit.OutcomeAllowed,
		Detail: audit.Detail{
			"role": string(authz.Admin), "source": SourceManual.String(),
		},
	}); err != nil {
		return authz.Membership{}, err
	}
	if err = transaction.Commit(ctx); err != nil {
		return authz.Membership{}, fmt.Errorf("committing organization creation: %w", err)
	}
	return authz.Membership{
		ID: membershipID.String(), Organization: organization, Role: authz.Admin,
	}, nil
}
