package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
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

// User is a person who may sign in.
type User struct {
	ID          uuid.UUID
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	DisabledAt  time.Time
	CreatedAt   time.Time
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
	UserID      uuid.UUID
	Email       string
	DisplayName string
	Role        authz.Role
	Disabled    bool

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

// MembershipOf reads the current Membership for one User.
func (p *Database) MembershipOf(
	ctx context.Context, organization uuid.UUID, user uuid.UUID,
) (authz.Membership, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return authz.Membership{}, err
	}
	return membershipOf(ctx, pool, user)
}

// querier is a pool or a transaction. Reads that run both standalone and inside a mutation's
// transaction take it, so the same SQL serves both.
type querier interface {
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

// membershipOf resolves what a person may reach RIGHT NOW. Row presence grants the stored
// Role, so a removal takes effect on the person's next request rather than their next sign-in.
func membershipOf(ctx context.Context, on querier, user uuid.UUID) (authz.Membership, error) {
	var organization uuid.UUID
	var displayName, role string
	err := on.QueryRow(ctx, `
		SELECT membership.org_id, organization.display_name,
		       membership.role
		  FROM organization_membership membership
		  JOIN organization ON organization.org_id = membership.org_id
		 WHERE membership.user_id = $1`, user).Scan(&organization, &displayName, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return authz.Membership{}, ErrMembershipUnknown
	}
	if err != nil {
		return authz.Membership{}, fmt.Errorf("reading a membership: %w", err)
	}
	parsed, known := authz.ParseRole(role)
	if !known {
		return authz.Membership{}, ErrMembershipUnknown
	}
	return authz.Membership{
		Organization: organization, DisplayName: displayName, Role: parsed,
	}, nil
}

// ListMembers reports who may reach an organization and as what.
func (p *Database) ListMembers(
	ctx context.Context, principal authz.Principal, organization uuid.UUID, page Page,
) (MemberList, error) {
	if principal.Organization() != organization {
		return MemberList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return MemberList{}, err
	}
	after, afterID, err := decodeCursor(page.After, "createdAt")
	if err != nil {
		return MemberList{}, err
	}

	limit := pageLimit(page.Limit)
	rows, err := pool.Query(ctx, `
		SELECT membership.user_id, person.email, person.display_name,
		       membership.role, person.disabled_at, membership.created_at
		  FROM organization_membership membership
		  JOIN app_user person ON person.user_id = membership.user_id
		 WHERE membership.org_id = $1
		   AND ($2::TIMESTAMPTZ IS NULL
		        OR (membership.created_at, membership.user_id) > ($2::TIMESTAMPTZ, $3::UUID))
		 ORDER BY membership.created_at, membership.user_id
		 LIMIT $4`,
		organization, after, afterID, limit+1)
	if err != nil {
		return MemberList{}, fmt.Errorf("reading members: %w", err)
	}
	defer rows.Close()

	members := make([]Member, 0, limit)
	var next string
	for rows.Next() {
		var (
			member   Member
			role     string
			disabled *time.Time
		)
		if err := rows.Scan(&member.UserID, &member.Email, &member.DisplayName, &role,
			&disabled, &member.CreatedAt); err != nil {
			return MemberList{}, fmt.Errorf("scanning a member: %w", err)
		}
		if len(members) == limit {
			last := members[limit-1]
			next = encodeCursor("createdAt", last.CreatedAt, last.UserID)
			break
		}
		member.Role = authz.Role(role)
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
// It refuses the change that would leave the Organization with no Admin. That check and the
// write share one transaction, so two administrators demoting the last two Admins at once cannot
// both pass it.
func (p *Database) SetMembership(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	user uuid.UUID, role authz.Role,
) (Member, error) {
	return auditedWithAction(ctx, p, principal, organization,
		func(ctx context.Context, transaction pgx.Tx) (
			Member, audit.Action, audit.Target, audit.Detail, error,
		) {
			var held string
			err := transaction.QueryRow(ctx, `
				SELECT role FROM organization_membership
				 WHERE org_id = $1 AND user_id = $2 FOR UPDATE`,
				organization, user).Scan(&held)
			previous := held
			if errors.Is(err, pgx.ErrNoRows) {
				previous = ""
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return Member{}, "", audit.Target{}, nil,
					fmt.Errorf("reading a membership: %w", err)
			}
			if previous == string(authz.Admin) && role != authz.Admin {
				if err := refuseIfLastAdmin(ctx, transaction, organization, user); err != nil {
					return Member{}, "", audit.Target{}, nil, err
				}
			}

			var member Member
			if err := transaction.QueryRow(ctx, `
				INSERT INTO organization_membership (org_id, user_id, role)
				VALUES ($1, $2, $3)
				ON CONFLICT (org_id, user_id) DO UPDATE
				    SET role = EXCLUDED.role
				RETURNING user_id, role, created_at`,
				organization, user, string(role)).Scan(&member.UserID,
				&member.Role, &member.CreatedAt); err != nil {
				if isForeignKeyViolation(err) {
					return Member{}, "", audit.Target{}, nil, ErrUserUnknown
				}
				return Member{}, "", audit.Target{}, nil,
					fmt.Errorf("writing a membership: %w", err)
			}

			action := audit.ActionMembershipChanged
			detail := audit.Detail{"beforeRole": previous, "afterRole": string(role)}
			if previous == "" {
				action = audit.ActionMembershipGranted
				detail = audit.Detail{"role": string(role)}
			}
			return member, action,
				audit.Target{Kind: audit.TargetUser, ID: user.String()},
				detail,
				nil
		})
}

// UpdateMembership changes the supported Role in one audited transaction.
func (p *Database) UpdateMembership(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	user uuid.UUID, wantedRole authz.Role,
) (Member, error) {
	return audited(ctx, p, principal, organization, audit.ActionMembershipChanged,
		func(ctx context.Context, transaction pgx.Tx) (Member, audit.Target, audit.Detail, error) {
			var currentRole string
			err := transaction.QueryRow(ctx, `
				SELECT role
				  FROM organization_membership
				 WHERE org_id = $1 AND user_id = $2 FOR UPDATE`,
				organization, user).Scan(&currentRole)
			if errors.Is(err, pgx.ErrNoRows) {
				return Member{}, audit.Target{}, nil, ErrMembershipUnknown
			}
			if err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("reading a membership: %w", err)
			}
			role := string(wantedRole)
			if currentRole == string(authz.Admin) && role != string(authz.Admin) {
				if err := refuseIfLastAdmin(ctx, transaction, organization, user); err != nil {
					return Member{}, audit.Target{}, nil, err
				}
			}
			if _, err = transaction.Exec(ctx, `
				UPDATE organization_membership
				   SET role = $3
				 WHERE org_id = $1 AND user_id = $2`, organization, user, role,
			); err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("updating a membership: %w", err)
			}

			var member Member
			var disabled *time.Time
			var storedRole string
			err = transaction.QueryRow(ctx, `
				SELECT membership.user_id, person.email,
				       person.display_name, membership.role, person.disabled_at,
				       membership.created_at
				  FROM organization_membership membership
				  JOIN app_user person ON person.user_id = membership.user_id
				 WHERE membership.user_id = $1 AND membership.org_id = $2`,
				user, organization).Scan(
				&member.UserID, &member.Email, &member.DisplayName,
				&storedRole, &disabled, &member.CreatedAt)
			if err != nil {
				return Member{}, audit.Target{}, nil, fmt.Errorf("reading updated membership: %w", err)
			}
			member.Role = authz.Role(storedRole)
			member.Disabled = disabled != nil
			return member, audit.Target{Kind: audit.TargetUser, ID: user.String()},
				audit.Detail{
					"beforeRole": currentRole, "afterRole": role,
				}, nil
		})
}

// RemoveMembership ends a person's access to one organization. Their user row and their place
// in the record survive: deleting the person would leave every event they produced naming an
// identifier nothing resolves.
func (p *Database) RemoveMembership(
	ctx context.Context, principal authz.Principal, organization uuid.UUID,
	user uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionMembershipRevoked,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var held string
			err := transaction.QueryRow(ctx, `
				SELECT role FROM organization_membership
				 WHERE org_id = $1 AND user_id = $2 FOR UPDATE`,
				organization, user).Scan(&held)
			role := held
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
				DELETE FROM organization_membership WHERE org_id = $1 AND user_id = $2`,
				organization, user); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("removing a membership: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetUser, ID: user.String()},
				audit.Detail{"before": role}, nil
		})
	return err
}

// refuseIfLastAdmin refuses a change that would leave the organization with no admin —
// a tenant nobody can administer is a lockout, not a configuration.
func refuseIfLastAdmin(
	ctx context.Context, transaction pgx.Tx, organization uuid.UUID, except uuid.UUID,
) error {
	var remaining int
	if err := transaction.QueryRow(ctx, `
		SELECT count(*) FROM organization_membership
		 WHERE org_id = $1 AND role = $2 AND user_id <> $3`,
		organization, string(authz.Admin), except).Scan(&remaining); err != nil {
		return fmt.Errorf("counting admins: %w", err)
	}
	if remaining == 0 {
		return ErrLastAdmin
	}
	return nil
}
