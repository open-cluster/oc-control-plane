package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Refusals the identity tables can produce.
var (
	// ErrUserUnknown reports a user this database does not have.
	ErrUserUnknown = errors.New("user unknown")
	// ErrUserDisabled reports a user who exists and may sign in to nothing.
	ErrUserDisabled = errors.New("user disabled")
	// ErrMembershipUnknown reports a membership this organization does not have.
	ErrMembershipUnknown = errors.New("membership unknown")
	// ErrLastAdmin reports the change that would leave an organization with no admin. It is
	// refused, because a tenant nobody can administer needs a support ticket to recover and
	// the mistake is one keystroke away from an ordinary role change.
	ErrLastAdmin = errors.New("an organization must keep at least one admin")
)

// MembershipSource is how a membership came to exist. It is persisted as an integer and
// constrained by a CHECK in migration 0011.
type MembershipSource int16

const (
	// SourceManual is a membership an administrator granted.
	SourceManual MembershipSource = 1
	// SourceJIT is one created at a first sign-in under the provider's policy.
	SourceJIT MembershipSource = 2
	// SourceSCIM is one a directory owns.
	SourceSCIM MembershipSource = 3
)

func (s MembershipSource) String() string {
	switch s {
	case SourceManual:
		return "manual"
	case SourceJIT:
		return "jit"
	case SourceSCIM:
		return "scim"
	default:
		return "unrecognised"
	}
}

// User is a person who may sign in.
type User struct {
	ID            uuid.UUID
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	DisabledAt    time.Time
	LastSignIn    time.Time
	CreatedAt     time.Time
}

// Disabled reports whether this user may sign in to anything.
func (u User) Disabled() bool { return !u.DisabledAt.IsZero() }

// Identity is what an identity provider asserted about a person at sign-in.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
}

// Member is one person's membership in one organization, with enough of the person to render a
// list without a second read.
type Member struct {
	MembershipID uuid.UUID
	UserID       uuid.UUID
	Email        string
	DisplayName  string
	Role         authz.Role
	Source       MembershipSource
	// ExternalID is the directory's own identifier for this person, and is empty for a
	// membership an administrator granted by hand.
	ExternalID string
	// Active is whether this membership grants anything. A directory sets it false rather than
	// removing the row, and an administrator reading the list needs to see the difference
	// before they act on it.
	Active   bool
	Disabled bool

	CreatedAt time.Time
}

// MemberList is a page of an organization's members.
type MemberList struct {
	Members []Member
	Next    string
}

func orEmptyText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// MembershipsOf reads the current memberships for one user.
func (p *Database) MembershipsOf(
	ctx context.Context, organization tenancy.Organization, user uuid.UUID,
) ([]authz.Membership, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	return membershipsOf(ctx, pool, user)
}

// querier is a pool or a transaction. Reads that run both standalone and inside a mutation's
// transaction take it, so the same SQL serves both.
type querier interface {
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

// membershipsOf resolves what a person may reach RIGHT NOW.
//
// Two things must both hold, and they are different facts. The membership must be ACTIVE — the
// directory's statement that this person is enabled, which it sets false rather than deleting,
// because SCIM has no "gone". And it must HOLD A ROLE — a directory-provisioned person in no
// mapped group has none, and being in the company is not being in this product.
//
// Filtering here rather than at each call site is what makes both take effect on the person's
// next request rather than at their next sign-in, for every route at once.
func membershipsOf(ctx context.Context, on querier, user uuid.UUID) ([]authz.Membership, error) {
	rows, err := on.Query(ctx, `
		SELECT org_id, role
		  FROM organization_membership
		 WHERE user_id = $1 AND active AND role IS NOT NULL
		 ORDER BY org_id`, user)
	if err != nil {
		return nil, fmt.Errorf("reading memberships: %w", err)
	}
	defer rows.Close()

	memberships := make([]authz.Membership, 0, 4)
	for rows.Next() {
		var name, role string
		if err := rows.Scan(&name, &role); err != nil {
			return nil, fmt.Errorf("scanning a membership: %w", err)
		}
		organization, err := tenancy.NewOrganization(name)
		if err != nil {
			continue
		}
		parsed, known := authz.ParseRole(role)
		if !known {
			// A role this build no longer has is DROPPED rather than failing the sign-in. A
			// rename must not be an outage, and a dropped membership answers 404 — the safe
			// direction to fail.
			continue
		}
		memberships = append(memberships, authz.Membership{Organization: organization, Role: parsed})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading memberships: %w", err)
	}
	return memberships, nil
}

// ListMembers reports who may reach an organization and as what.
func (p *Database) ListMembers(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization, page Page,
) (MemberList, error) {
	if !principal.MemberOf(organization) {
		return MemberList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return MemberList{}, err
	}
	after, afterID, err := decodeCursor(page.After)
	if err != nil {
		return MemberList{}, err
	}

	limit := pageLimit(page.Limit)
	rows, err := pool.Query(ctx, `
		SELECT membership.membership_id, membership.user_id, person.email, person.display_name,
		       membership.role, membership.source, membership.external_id, membership.active,
		       person.disabled_at, membership.created_at
		  FROM organization_membership membership
		  JOIN app_user person ON person.user_id = membership.user_id
		 WHERE membership.org_id = $1
		   AND ($2::TIMESTAMPTZ IS NULL
		        OR (membership.created_at, membership.membership_id) > ($2::TIMESTAMPTZ, $3::UUID))
		 ORDER BY membership.created_at, membership.membership_id
		 LIMIT $4`,
		organization.String(), after, afterID, limit+1)
	if err != nil {
		return MemberList{}, fmt.Errorf("reading members: %w", err)
	}
	defer rows.Close()

	members := make([]Member, 0, limit)
	var next string
	for rows.Next() {
		var (
			member   Member
			role     *string
			source   int16
			external *string
			disabled *time.Time
		)
		if err := rows.Scan(&member.MembershipID, &member.UserID, &member.Email,
			&member.DisplayName, &role, &source, &external, &member.Active, &disabled,
			&member.CreatedAt); err != nil {
			return MemberList{}, fmt.Errorf("scanning a member: %w", err)
		}
		member.ExternalID = orEmptyText(external)
		if len(members) == limit {
			last := members[limit-1]
			next = encodeCursor(last.CreatedAt, last.MembershipID)
			break
		}
		member.Source = MembershipSource(source)
		member.Disabled = disabled != nil
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return MemberList{}, fmt.Errorf("reading members: %w", err)
	}
	return MemberList{Members: members, Next: next}, nil
}

// SetMembership grants or changes a person's role in an organization.
//
// It refuses the change that would leave the tenant with no owner. That check and the write
// share one transaction, so two administrators demoting the last two owners at once cannot
// both pass it.
func (p *Database) SetMembership(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	user uuid.UUID, role authz.Role,
) (Member, error) {
	action := audit.ActionMembershipChanged
	return audited(ctx, p, principal, organization, action,
		func(ctx context.Context, transaction pgx.Tx) (Member, audit.Target, audit.Detail, error) {
			var held *string
			err := transaction.QueryRow(ctx, `
				SELECT role FROM organization_membership
				 WHERE org_id = $1 AND user_id = $2 AND active FOR UPDATE`,
				organization.String(), user).Scan(&held)
			previous := orEmptyText(held)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return Member{}, audit.Target{}, nil, fmt.Errorf("reading a membership: %w", err)
			}
			if previous == string(authz.Admin) && role != authz.Admin {
				if err := refuseIfLastAdmin(ctx, transaction, organization, user); err != nil {
					return Member{}, audit.Target{}, nil, err
				}
			}

			var member Member
			if err := transaction.QueryRow(ctx, `
				INSERT INTO organization_membership (membership_id, org_id, user_id, role,
				                                     source, granted_by)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (org_id, user_id) DO UPDATE
				    SET role       = EXCLUDED.role,
				        source     = EXCLUDED.source,
				        granted_by = EXCLUDED.granted_by,
				        -- An administrator granting a role means the person may reach the
				        -- tenant, whatever a directory said about them. A change that left the
				        -- membership inactive would be one that appeared to work and did not.
				        active     = TRUE,
				        updated_at = now()
				RETURNING membership_id, user_id, role, source, created_at`,
				uuid.New(), organization.String(), user, string(role),
				int16(SourceManual), principal.ID()).Scan(&member.MembershipID, &member.UserID,
				&member.Role, &member.Source, &member.CreatedAt); err != nil {
				if isForeignKeyViolation(err) {
					return Member{}, audit.Target{}, nil, ErrUserUnknown
				}
				return Member{}, audit.Target{}, nil, fmt.Errorf("writing a membership: %w", err)
			}

			if previous == "" {
				action = audit.ActionMembershipGranted
			}
			return member,
				audit.Target{Kind: audit.TargetMembership, ID: member.MembershipID.String()},
				audit.Detail{"userId": user.String(), "before": previous, "after": string(role)},
				nil
		})
}

// RemoveMembership ends a person's access to one organization. Their user row and their place
// in the record survive: deleting the person would leave every event they produced naming an
// identifier nothing resolves.
func (p *Database) RemoveMembership(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	user uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionMembershipRevoked,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var held *string
			var membership uuid.UUID
			err := transaction.QueryRow(ctx, `
				SELECT membership_id, role FROM organization_membership
				 WHERE org_id = $1 AND user_id = $2 FOR UPDATE`,
				organization.String(), user).Scan(&membership, &held)
			role := orEmptyText(held)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, ErrMembershipUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("reading a membership: %w", err)
			}
			if role == string(authz.Admin) {
				if err := refuseIfLastAdmin(ctx, transaction, organization, user); err != nil {
					return struct{}{}, audit.Target{}, nil, err
				}
			}

			if _, err := transaction.Exec(ctx, `
				DELETE FROM organization_membership WHERE membership_id = $1`,
				membership); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("removing a membership: %w", err)
			}
			// Story 10 and story 14: access ends now, not at the next token refresh. The
			// sessions go with the membership, in the same transaction, so there is no window
			// in which the row is gone and the credential still works.
			if _, err := transaction.Exec(ctx, `
				UPDATE operator_session
				   SET revoked_at = now(), revoked_by = $3
				 WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL`,
				user, organization.String(), principal.ID()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("revoking sessions: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetMembership, ID: membership.String()},
				audit.Detail{"userId": user.String(), "before": role}, nil
		})
	return err
}

// refuseIfLastAdmin refuses a change that would leave the organization with no admin —
// a tenant nobody can administer is a lockout, not a configuration.
func refuseIfLastAdmin(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, except uuid.UUID,
) error {
	var remaining int
	if err := transaction.QueryRow(ctx, `
		SELECT count(*) FROM organization_membership
		 WHERE org_id = $1 AND role = $2 AND user_id <> $3 AND active`,
		organization.String(), string(authz.Admin), except).Scan(&remaining); err != nil {
		return fmt.Errorf("counting admins: %w", err)
	}
	if remaining == 0 {
		return ErrLastAdmin
	}
	return nil
}
