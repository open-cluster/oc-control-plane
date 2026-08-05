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
	// ErrUserUnknown reports a user this placement does not have.
	ErrUserUnknown = errors.New("user unknown")
	// ErrUserDisabled reports a user who exists and may sign in to nothing.
	ErrUserDisabled = errors.New("user disabled")
	// ErrMembershipUnknown reports a membership this organization does not have.
	ErrMembershipUnknown = errors.New("membership unknown")
	// ErrLastOwner reports the change that would leave an organization with no owner. It is
	// refused, because a tenant nobody can administer needs a support ticket to recover and
	// the mistake is one keystroke away from an ordinary role change.
	ErrLastOwner = errors.New("an organization must keep at least one owner")
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
	Disabled     bool
	CreatedAt    time.Time
}

// MemberList is a page of an organization's members.
type MemberList struct {
	Members []Member
	Next    string
}

// ResolveUser finds or creates the person an identity provider just asserted, and returns them
// with the memberships that decide what they may reach.
//
// It is placement-wide in the same sense ConnectionByID is, and for the same reason: the
// caller is completing a sign-in against a provider configured by one organization, so the
// organization IS known — it is passed in, and the placement resolves from it. Nothing about
// the identity selects the tenant.
//
// grant, when non-zero, is the role a first-time signer-in is provisioned with. Zero means
// just-in-time provisioning is off for this provider: an unknown person is then created as a
// user with no membership, which signs them in to nothing and lets an administrator find them
// rather than making them invisible.
func (p *Placements) ResolveUser(
	ctx context.Context, organization tenancy.Organization,
	identity Identity, grant authz.Role,
) (User, []authz.Membership, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return User{}, nil, err
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return User{}, nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// One statement, so two concurrent sign-ins by the same person cannot both insert. The
	// update is what keeps a renamed or re-verified account current without a second write.
	var user User
	var disabled *time.Time
	if err := transaction.QueryRow(ctx, `
		INSERT INTO app_user (user_id, issuer, subject, email, email_verified, display_name,
		                      last_sign_in)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (issuer, subject) DO UPDATE
		    SET email          = EXCLUDED.email,
		        email_verified = EXCLUDED.email_verified,
		        display_name   = EXCLUDED.display_name,
		        last_sign_in   = now(),
		        updated_at     = now()
		RETURNING user_id, issuer, subject, email, email_verified, display_name, disabled_at,
		          created_at`,
		uuid.New(), identity.Issuer, identity.Subject, identity.Email, identity.EmailVerified,
		identity.DisplayName).Scan(&user.ID, &user.Issuer, &user.Subject, &user.Email,
		&user.EmailVerified, &user.DisplayName, &disabled, &user.CreatedAt); err != nil {
		return User{}, nil, fmt.Errorf("resolving a user: %w", err)
	}
	if disabled != nil {
		user.DisabledAt = *disabled
	}
	if user.Disabled() {
		return user, nil, ErrUserDisabled
	}

	if authz.KnownRole(grant) {
		// DO NOTHING rather than an update: a person who already holds a role in this tenant
		// keeps it. Re-granting on every sign-in would silently undo an administrator's
		// deliberate change the next time the person signed in.
		tag, err := transaction.Exec(ctx, `
			INSERT INTO organization_membership (membership_id, organization, user_id, role,
			                                     source, granted_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (organization, user_id) DO NOTHING`,
			uuid.New(), organization.String(), user.ID, string(grant),
			int16(SourceJIT), "just-in-time provisioning")
		if err != nil {
			return User{}, nil, fmt.Errorf("provisioning a membership: %w", err)
		}
		// Only when a membership was actually CREATED. Every sign-in reaches this statement and
		// almost all of them are conflicts; recording those would fill the trail with a row per
		// sign-in saying nothing happened, and the one row that matters — somebody gained
		// access to this tenant without an administrator doing anything — would be lost in it.
		if tag.RowsAffected() == 1 {
			if err := writeEvent(ctx, transaction, audit.Event{
				Organization: organization.String(),
				Actor: audit.Actor{
					Kind:        audit.ActorUser,
					ID:          user.ID.String(),
					DisplayName: user.DisplayName,
				},
				Action:  audit.ActionUserProvisioned,
				Target:  audit.Target{Kind: audit.TargetMembership, ID: user.ID.String()},
				Outcome: audit.OutcomeAllowed,
				Detail: audit.Detail{
					"role":   string(grant),
					"source": SourceJIT.String(),
					"email":  user.Email,
				},
			}); err != nil {
				return User{}, nil, err
			}
		}
	}

	memberships, err := membershipsOf(ctx, transaction, user.ID)
	if err != nil {
		return User{}, nil, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return User{}, nil, fmt.Errorf("commit: %w", err)
	}
	return user, memberships, nil
}

// MembershipsOf reports every organization a user holds a role in, as served from one
// placement. It is placement-wide because a user is: the row is not tenant-scoped, and the
// memberships it carries are the answer rather than the question.
func (p *Placements) MembershipsOf(
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

func membershipsOf(ctx context.Context, on querier, user uuid.UUID) ([]authz.Membership, error) {
	rows, err := on.Query(ctx, `
		SELECT organization, role
		  FROM organization_membership
		 WHERE user_id = $1
		 ORDER BY organization`, user)
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
func (p *Placements) ListMembers(
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
		       membership.role, membership.source, person.disabled_at, membership.created_at
		  FROM organization_membership membership
		  JOIN app_user person ON person.user_id = membership.user_id
		 WHERE membership.organization = $1
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
			role     string
			source   int16
			disabled *time.Time
		)
		if err := rows.Scan(&member.MembershipID, &member.UserID, &member.Email,
			&member.DisplayName, &role, &source, &disabled, &member.CreatedAt); err != nil {
			return MemberList{}, fmt.Errorf("scanning a member: %w", err)
		}
		if len(members) == limit {
			last := members[limit-1]
			next = encodeCursor(last.CreatedAt, last.MembershipID)
			break
		}
		member.Role = authz.Role(role)
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
func (p *Placements) SetMembership(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	user uuid.UUID, role authz.Role,
) (Member, error) {
	action := audit.ActionMembershipChanged
	return audited(ctx, p, principal, organization, action,
		func(ctx context.Context, transaction pgx.Tx) (Member, audit.Target, audit.Detail, error) {
			var previous string
			err := transaction.QueryRow(ctx, `
				SELECT role FROM organization_membership
				 WHERE organization = $1 AND user_id = $2 FOR UPDATE`,
				organization.String(), user).Scan(&previous)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return Member{}, audit.Target{}, nil, fmt.Errorf("reading a membership: %w", err)
			}
			if previous == string(authz.OrganizationOwner) && role != authz.OrganizationOwner {
				if err := refuseIfLastOwner(ctx, transaction, organization, user); err != nil {
					return Member{}, audit.Target{}, nil, err
				}
			}

			var member Member
			if err := transaction.QueryRow(ctx, `
				INSERT INTO organization_membership (membership_id, organization, user_id, role,
				                                     source, granted_by)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (organization, user_id) DO UPDATE
				    SET role       = EXCLUDED.role,
				        source     = EXCLUDED.source,
				        granted_by = EXCLUDED.granted_by,
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
func (p *Placements) RemoveMembership(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	user uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionMembershipRevoked,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var role string
			var membership uuid.UUID
			err := transaction.QueryRow(ctx, `
				SELECT membership_id, role FROM organization_membership
				 WHERE organization = $1 AND user_id = $2 FOR UPDATE`,
				organization.String(), user).Scan(&membership, &role)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, ErrMembershipUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("reading a membership: %w", err)
			}
			if role == string(authz.OrganizationOwner) {
				if err := refuseIfLastOwner(ctx, transaction, organization, user); err != nil {
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
				 WHERE user_id = $1 AND organization = $2 AND revoked_at IS NULL`,
				user, organization.String(), principal.ID()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("revoking sessions: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetMembership, ID: membership.String()},
				audit.Detail{"userId": user.String(), "before": role}, nil
		})
	return err
}

// refuseIfLastOwner refuses a change that would leave the organization with no owner.
func refuseIfLastOwner(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization, except uuid.UUID,
) error {
	var remaining int
	if err := transaction.QueryRow(ctx, `
		SELECT count(*) FROM organization_membership
		 WHERE organization = $1 AND role = $2 AND user_id <> $3`,
		organization.String(), string(authz.OrganizationOwner), except).Scan(&remaining); err != nil {
		return fmt.Errorf("counting owners: %w", err)
	}
	if remaining == 0 {
		return ErrLastOwner
	}
	return nil
}
