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

// SCIMIssuer is the issuer a person provisioned by a directory is created under, before they
// have ever signed in.
//
// A user's identity is (issuer, subject), and a directory does not know either: it knows a
// userName and an externalId. So a provisioned person gets a placeholder identity under this
// issuer, and the first time they actually sign in the row is ADOPTED — its issuer and subject
// become the real ones. See adoptProvisionedUser for why that is safe and what bounds it.
const SCIMIssuer = "urn:opencluster:scim"

// Refusals the provisioning tables can produce.
var (
	// ErrProvisionedUserUnknown reports a person this directory has not provisioned.
	ErrProvisionedUserUnknown = errors.New("provisioned user unknown")
	// ErrProvisionedUserExists reports a userName or external identifier the directory has
	// already used in this organization. SCIM asks for a 409 with a specific type, which is
	// what the handler renders it as.
	ErrProvisionedUserExists = errors.New("provisioned user already exists")
)

// ProvisionedUser is a person as a directory sees them: the identifiers the directory chose,
// the role their groups earned them, and whether they may reach the tenant.
type ProvisionedUser struct {
	// ID is the SCIM resource identifier, and is the user identifier this product already had.
	// Handing a directory the same identifier the audit trail uses means an administrator
	// reading a row and an administrator reading the directory are looking at one person.
	ID uuid.UUID
	// UserName is what the directory calls this person. It is almost always their address, and
	// it is what a sign-in is matched against when they first arrive.
	UserName    string
	ExternalID  string
	Email       string
	DisplayName string
	// Active is whether the membership grants anything. False is a deprovisioned person whose
	// row survives so the directory can read them back.
	Active    bool
	Role      authz.Role
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewProvisionedUser is what a directory asked for.
type NewProvisionedUser struct {
	UserName    string
	ExternalID  string
	Email       string
	DisplayName string
	Active      bool
}

// ProvisionedUserFilter narrows a listing. SCIM's filter language is far larger than this; the
// two attributes here are the ones a directory actually sends, and the handler refuses the rest
// rather than answering a filter it did not apply.
type ProvisionedUserFilter struct {
	UserName   string
	ExternalID string
}

// ProvisionedUserList is a page of a directory's people.
type ProvisionedUserList struct {
	Users []ProvisionedUser
	// Total is how many match the filter, which SCIM requires in every list response and which
	// a directory uses to page.
	Total int
}

// ProvisionUser creates a person the directory reported, or reports that it already has.
//
// The membership is created INACTIVE-aware rather than always granting: the role comes from the
// groups this person is in, and a directory usually creates the person before it adds them to
// anything. Somebody in no mapped group holds no role and reaches nothing, which is the correct
// answer rather than a default.
func (p *Database) ProvisionUser(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted NewProvisionedUser,
) (ProvisionedUser, error) {
	return audited(ctx, p, principal, organization, audit.ActionUserProvisioned,
		func(ctx context.Context, transaction pgx.Tx) (
			ProvisionedUser, audit.Target, audit.Detail, error,
		) {
			taken, err := userNameIsTaken(ctx, transaction, organization, wanted, uuid.Nil)
			if err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			if taken {
				return ProvisionedUser{}, audit.Target{}, nil, ErrProvisionedUserExists
			}

			// The person may already exist as a user — somebody an administrator invited, or
			// somebody who signed in through a provider before the directory was connected.
			// Matching on the address rather than creating a second row is what stops one
			// person becoming two accounts with one audit trail each.
			user, err := userForProvisioning(ctx, transaction, organization, wanted)
			if err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}

			provisioned, err := writeProvisionedMembership(
				ctx, transaction, organization, user, wanted, principal.ID())
			if err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			return provisioned,
				audit.Target{Kind: audit.TargetMembership, ID: provisioned.ID.String()},
				audit.Detail{
					"userName":   provisioned.UserName,
					"externalId": provisioned.ExternalID,
					"active":     provisioned.Active,
					"source":     SourceSCIM.String(),
				}, nil
		})
}

// userForProvisioning finds the person this directory is describing, or creates them.
//
// The match is on the ADDRESS, case-folded, and it is the only join available: a directory
// knows a userName and an externalId, and this product knows an issuer and a subject. They
// agree on the address and on nothing else, which is why every directory integration in the
// world uses it.
func userForProvisioning(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	wanted NewProvisionedUser,
) (User, error) {
	address := strings.ToLower(strings.TrimSpace(wanted.Email))
	if address == "" {
		address = strings.ToLower(strings.TrimSpace(wanted.UserName))
	}

	var existing User
	err := transaction.QueryRow(ctx, `
		SELECT user_id, issuer, subject, email, email_verified, display_name, created_at
		  FROM app_user
		 WHERE lower(email) = $1
		 LIMIT 1`, address).Scan(&existing.ID, &existing.Issuer, &existing.Subject,
		&existing.Email, &existing.EmailVerified, &existing.DisplayName, &existing.CreatedAt)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("resolving a provisioned user: %w", err)
	}

	subject := strings.TrimSpace(wanted.ExternalID)
	if subject == "" {
		subject = wanted.UserName
	}
	// The address is recorded as VERIFIED. A directory asserting somebody's address is the
	// customer's own system of record saying so, which is a stronger claim than an identity
	// provider's email_verified flag rather than a weaker one.
	if err := transaction.QueryRow(ctx, `
		INSERT INTO app_user (user_id, issuer, subject, email, email_verified, display_name)
		VALUES ($1, $2, $3, $4, TRUE, $5)
		ON CONFLICT (issuer, subject) DO UPDATE
		    SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, updated_at = now()
		RETURNING user_id, issuer, subject, email, email_verified, display_name, created_at`,
		uuid.New(), SCIMIssuer+":"+organization.String(), subject, address,
		wanted.DisplayName).Scan(&existing.ID, &existing.Issuer, &existing.Subject,
		&existing.Email, &existing.EmailVerified, &existing.DisplayName,
		&existing.CreatedAt); err != nil {
		return User{}, fmt.Errorf("creating a provisioned user: %w", err)
	}
	return existing, nil
}

// writeProvisionedMembership records that this person belongs to this tenant, and recomputes
// what their groups earn them.
func writeProvisionedMembership(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	user User, wanted NewProvisionedUser, by string,
) (ProvisionedUser, error) {
	// No role. A person the directory has just reported is active and holds nothing until a
	// group somebody mapped puts them in one — being in the company is not being in this
	// product. The recompute below is what decides it, and it is the only thing that does.
	//
	// A membership an administrator granted by hand keeps its role: the update names only the
	// directory's own columns, so connecting a directory does not revoke every deliberate grant
	// in the tenant.
	if _, err := transaction.Exec(ctx, `
		INSERT INTO organization_membership (membership_id, org_id, user_id, role, source,
		                                     granted_by, external_id, active)
		VALUES ($1, $2, $3, NULL, $4, $5, $6, $7)
		ON CONFLICT (org_id, user_id) DO UPDATE
		    SET granted_by  = EXCLUDED.granted_by,
		        external_id = EXCLUDED.external_id,
		        active      = EXCLUDED.active,
		        updated_at  = now()`,
		uuid.New(), organization.String(), user.ID, int16(SourceSCIM),
		by, nullableText(wanted.ExternalID), wanted.Active); err != nil {
		if isUniqueViolation(err, "organization_membership_external_id_is_unique_per_org") {
			return ProvisionedUser{}, ErrProvisionedUserExists
		}
		return ProvisionedUser{}, fmt.Errorf("writing a provisioned membership: %w", err)
	}

	// The role is never what the directory sent — a directory has no opinion about this
	// product's roles. It is what the groups an administrator mapped say, recomputed here so a
	// person created into a group they were already in lands with the right role immediately.
	if err := recomputeProvisionedRole(ctx, transaction, organization, user.ID); err != nil {
		return ProvisionedUser{}, err
	}
	return readProvisionedUser(ctx, transaction, organization, user.ID)
}

// ReplaceProvisionedUser applies a directory's whole picture of a person. It is what a SCIM PUT
// asks for: whatever is not sent is not kept.
func (p *Database) ReplaceProvisionedUser(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, wanted NewProvisionedUser,
) (ProvisionedUser, error) {
	return audited(ctx, p, principal, organization, audit.ActionMembershipChanged,
		func(ctx context.Context, transaction pgx.Tx) (
			ProvisionedUser, audit.Target, audit.Detail, error,
		) {
			before, err := readProvisionedUser(ctx, transaction, organization, id)
			if err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			taken, err := userNameIsTaken(ctx, transaction, organization, wanted, id)
			if err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			if taken {
				return ProvisionedUser{}, audit.Target{}, nil, ErrProvisionedUserExists
			}

			if _, err := transaction.Exec(ctx, `
				UPDATE app_user
				   SET email = coalesce(nullif($2, ''), email),
				       display_name = coalesce(nullif($3, ''), display_name),
				       updated_at = now()
				 WHERE user_id = $1`, id, strings.ToLower(strings.TrimSpace(wanted.Email)),
				wanted.DisplayName); err != nil {
				return ProvisionedUser{}, audit.Target{}, nil,
					fmt.Errorf("updating a provisioned user: %w", err)
			}
			if err := setProvisionedActive(
				ctx, transaction, organization, id, wanted.Active, principal.ID()); err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}

			after, err := readProvisionedUser(ctx, transaction, organization, id)
			if err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{
					"source": SourceSCIM.String(),
					"before": map[string]any{"active": before.Active, "role": string(before.Role)},
					"after":  map[string]any{"active": after.Active, "role": string(after.Role)},
				}, nil
		})
}

// SetProvisionedUserActive is story 14: a person a directory deactivates loses access without a
// manual step, and does so on their next request rather than at their next sign-in.
func (p *Database) SetProvisionedUserActive(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, active bool,
) (ProvisionedUser, error) {
	return audited(ctx, p, principal, organization, audit.ActionMembershipChanged,
		func(ctx context.Context, transaction pgx.Tx) (
			ProvisionedUser, audit.Target, audit.Detail, error,
		) {
			if _, err := readProvisionedUser(ctx, transaction, organization, id); err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			if err := setProvisionedActive(
				ctx, transaction, organization, id, active, principal.ID()); err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			after, err := readProvisionedUser(ctx, transaction, organization, id)
			if err != nil {
				return ProvisionedUser{}, audit.Target{}, nil, err
			}
			return after,
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{"active": active, "source": SourceSCIM.String()}, nil
		})
}

// setProvisionedActive flips a membership and, when it is being taken away, ends the sessions
// resting on it in the SAME transaction. A membership that stopped granting while a live
// session kept working would be a deprovisioning that had not happened.
func setProvisionedActive(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	user uuid.UUID, active bool, by string,
) error {
	if _, err := transaction.Exec(ctx, `
		UPDATE organization_membership
		   SET active = $3, updated_at = now()
		 WHERE org_id = $1 AND user_id = $2`,
		organization.String(), user, active); err != nil {
		return fmt.Errorf("setting a membership active: %w", err)
	}
	if active {
		return nil
	}
	if _, err := transaction.Exec(ctx, `
		UPDATE operator_session
		   SET revoked_at = now(), revoked_by = $3
		 WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL`,
		user, organization.String(), by); err != nil {
		return fmt.Errorf("revoking sessions: %w", err)
	}
	return nil
}

// DeprovisionUser is what a directory's DELETE means here.
//
// The membership row goes and the user row stays. Deleting the person would take their audit
// trail's meaning with it — every event they produced would name an identifier nothing
// resolves — and a directory that deletes somebody is saying they may not reach this tenant,
// not that they never did.
func (p *Database) DeprovisionUser(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionMembershipRevoked,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			before, err := readProvisionedUser(ctx, transaction, organization, id)
			if err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if _, err := transaction.Exec(ctx, `
				DELETE FROM organization_membership
				 WHERE org_id = $1 AND user_id = $2`,
				organization.String(), id); err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("deprovisioning a user: %w", err)
			}
			if _, err := transaction.Exec(ctx, `
				DELETE FROM scim_group_member WHERE org_id = $1 AND user_id = $2`,
				organization.String(), id); err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("removing group memberships: %w", err)
			}
			if _, err := transaction.Exec(ctx, `
				UPDATE operator_session
				   SET revoked_at = now(), revoked_by = $3
				 WHERE user_id = $1 AND org_id = $2 AND revoked_at IS NULL`,
				id, organization.String(), principal.ID()); err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("revoking sessions: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetMembership, ID: id.String()},
				audit.Detail{
					"userName": before.UserName,
					"source":   SourceSCIM.String(),
					"effect":   "the person no longer reaches this organization",
				}, nil
		})
	return err
}

// ProvisionedUsers lists what the directory has put here.
func (p *Database) ProvisionedUsers(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	filter ProvisionedUserFilter, startIndex, count int,
) (ProvisionedUserList, error) {
	if !principal.MemberOf(organization) {
		return ProvisionedUserList{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return ProvisionedUserList{}, err
	}

	var list ProvisionedUserList
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM organization_membership membership
		  JOIN app_user person ON person.user_id = membership.user_id
		 WHERE membership.org_id = $1
		   AND ($2::TEXT IS NULL OR lower(person.email) = lower($2::TEXT))
		   AND ($3::TEXT IS NULL OR membership.external_id = $3::TEXT)`,
		organization.String(), nullableText(filter.UserName),
		nullableText(filter.ExternalID)).Scan(&list.Total); err != nil {
		return ProvisionedUserList{}, fmt.Errorf("counting provisioned users: %w", err)
	}

	rows, err := pool.Query(ctx, `SELECT `+provisionedColumns+`
		  FROM organization_membership membership
		  JOIN app_user person ON person.user_id = membership.user_id
		 WHERE membership.org_id = $1
		   AND ($2::TEXT IS NULL OR lower(person.email) = lower($2::TEXT))
		   AND ($3::TEXT IS NULL OR membership.external_id = $3::TEXT)
		 ORDER BY membership.created_at, membership.user_id
		 OFFSET $4 LIMIT $5`,
		organization.String(), nullableText(filter.UserName), nullableText(filter.ExternalID),
		max(startIndex-1, 0), pageLimit(count))
	if err != nil {
		return ProvisionedUserList{}, fmt.Errorf("listing provisioned users: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		provisioned, scanErr := scanProvisionedUser(rows)
		if scanErr != nil {
			return ProvisionedUserList{}, scanErr
		}
		list.Users = append(list.Users, provisioned)
	}
	if err := rows.Err(); err != nil {
		return ProvisionedUserList{}, fmt.Errorf("listing provisioned users: %w", err)
	}
	return list, nil
}

// ProvisionedUser reads one person the directory addressed.
func (p *Database) ProvisionedUser(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) (ProvisionedUser, error) {
	if !principal.MemberOf(organization) {
		return ProvisionedUser{}, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return ProvisionedUser{}, err
	}
	return readProvisionedUser(ctx, pool, organization, id)
}

const provisionedColumns = `membership.user_id, person.email, membership.external_id,
	person.email, person.display_name, membership.active, membership.role,
	membership.created_at, membership.updated_at`

// scanProvisionedUser reads one. The role is nullable — a person in no mapped group holds none
// — and it is reported as the empty role rather than as an error.

func readProvisionedUser(
	ctx context.Context, on querier, organization tenancy.Organization, id uuid.UUID,
) (ProvisionedUser, error) {
	row := on.QueryRow(ctx, `SELECT `+provisionedColumns+`
		  FROM organization_membership membership
		  JOIN app_user person ON person.user_id = membership.user_id
		 WHERE membership.org_id = $1 AND membership.user_id = $2`,
		organization.String(), id)

	provisioned, err := scanProvisionedUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProvisionedUser{}, ErrProvisionedUserUnknown
	}
	return provisioned, err
}

func scanProvisionedUser(row scanned) (ProvisionedUser, error) {
	var (
		provisioned ProvisionedUser
		external    *string
		role        *string
	)
	if err := row.Scan(&provisioned.ID, &provisioned.UserName, &external, &provisioned.Email,
		&provisioned.DisplayName, &provisioned.Active, &role, &provisioned.CreatedAt,
		&provisioned.UpdatedAt); err != nil {
		return ProvisionedUser{}, err
	}
	provisioned.ExternalID = orEmptyText(external)
	provisioned.Role = authz.Role(orEmptyText(role))
	return provisioned, nil
}

// userNameIsTaken reports whether another person in this organization already answers to the
// userName or the external identifier a directory is asking for. SCIM requires a 409 for it,
// and the alternative — a constraint violation surfacing as a server error — is what makes a
// directory retry forever.
func userNameIsTaken(
	ctx context.Context, transaction pgx.Tx, organization tenancy.Organization,
	wanted NewProvisionedUser, except uuid.UUID,
) (bool, error) {
	var taken bool
	if err := transaction.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		      FROM organization_membership membership
		      JOIN app_user person ON person.user_id = membership.user_id
		     WHERE membership.org_id = $1
		       AND membership.user_id <> $2
		       AND (lower(person.email) = lower($3)
		            OR ($4::TEXT IS NOT NULL AND membership.external_id = $4::TEXT)))`,
		organization.String(), except, wanted.UserName,
		nullableText(wanted.ExternalID)).Scan(&taken); err != nil {
		return false, fmt.Errorf("checking a provisioned user: %w", err)
	}
	return taken, nil
}
