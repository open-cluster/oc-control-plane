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

var (
	ErrUserDisabled      = errors.New("user disabled")
	ErrMembershipUnknown = errors.New("membership unknown")
	ErrLastAdmin         = errors.New("an organization must keep at least one admin")
)

type User struct {
	ID          uuid.UUID
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	DisabledAt  time.Time
	CreatedAt   time.Time
}

func (u User) Disabled() bool { return !u.DisabledAt.IsZero() }

type Identity struct {
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
}

type Member struct {
	UserID      uuid.UUID
	Email       string
	DisplayName string
	Role        authz.Role
	Disabled    bool

	CreatedAt time.Time
}

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

type querier interface {
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, arguments ...any) pgx.Row
}

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
