package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// ResolveUser finds or creates the person an identity provider just asserted, and returns them
// with the memberships that decide what they may reach.
//
// The caller is completing a sign-in against a provider configured by one organization,
// so the organization is explicit and nothing about the identity selects the tenant.
//
// grant, when non-zero, is the role a first-time signer-in is provisioned with. Zero means
// just-in-time provisioning is off for this provider: an unknown person is then created as a
// user with no membership, which signs them in to nothing and lets an administrator find them
// rather than making them invisible.
func (p *Database) ResolveUser(
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

	// A person the DIRECTORY provisioned exists already, under a placeholder identity, because
	// a directory knows a userName and this product knows an issuer and a subject. This is
	// where those become one row rather than two.
	if err := adoptProvisionedUser(ctx, transaction, organization, identity); err != nil {
		return User{}, nil, err
	}

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
			INSERT INTO organization_membership (membership_id, org_id, user_id, role,
			                                     source, granted_by)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (org_id, user_id) DO NOTHING`,
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

// adoptProvisionedUser turns a directory's placeholder identity into the real one, the first
// time the person it describes actually signs in.
//
// Without it a provisioned person signing in would create a SECOND user row: the directory
// created one keyed on a placeholder issuer and a userName, and the sign-in is keyed on the
// provider's issuer and subject. They would have two identities, one membership, and an audit
// trail split between them — and the sign-in would land on the half with no membership and
// reach nothing, which is a support ticket rather than an error.
//
// Two bounds make it safe, and both matter.
//
// It adopts ONLY a row this product created for a directory — the placeholder issuer is
// namespaced to the organization and nothing else can hold it. A row belonging to a real
// issuer is never touched, so one provider's user cannot be taken over by another's assertion
// about the same address.
//
// And it matches on the ADDRESS, which is the only join available: a directory and an identity
// provider agree on that and on nothing else. Within one tenant, two accounts at one address
// are one person by definition — the customer's own directory said so.
func adoptProvisionedUser(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	identity Identity,
) error {
	address := strings.ToLower(strings.TrimSpace(identity.Email))
	if address == "" {
		return nil
	}

	if _, err := transaction.Exec(ctx, `
		UPDATE app_user
		   SET issuer = $3, subject = $4, updated_at = now()
		 WHERE lower(email) = $1
		   AND issuer LIKE $2
		   AND EXISTS (SELECT 1 FROM organization_membership
		                WHERE organization_membership.user_id = app_user.user_id
		                  AND organization_membership.org_id = $5)
		   -- Nothing to do if a row already holds the real identity, and the unique constraint
		   -- would refuse the update anyway. Guarding here makes it a no-op rather than an error
		   -- on every sign-in after the first.
		   AND NOT EXISTS (SELECT 1 FROM app_user existing
		                    WHERE existing.issuer = $3 AND existing.subject = $4)`,
		address, SCIMIssuer+":%", identity.Issuer, identity.Subject,
		organization.String()); err != nil {
		return fmt.Errorf("adopting a provisioned user: %w", err)
	}
	return nil
}

// MembershipsOf reports every organization a user holds a role in. A user is deployment-wide;
// the memberships it carries are the answer rather than the question.
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
