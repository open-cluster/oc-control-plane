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

// Refusals the automation tables can produce.
var (
	// ErrServiceAccountUnknown reports an account this organization does not have.
	ErrServiceAccountUnknown = errors.New("service account unknown")
	// ErrServiceAccountNameTaken reports a name another account in this organization holds.
	ErrServiceAccountNameTaken = errors.New("service account name is already used")
	// ErrTokenUnknown reports a token this organization does not have. It also covers a token
	// presented at authentication that is expired or revoked, because to whoever presented it
	// those are the same fact: it does not work.
	ErrTokenUnknown = errors.New("api token unknown")
)

// lastUsedResolution is how stale a token's last-used stamp may get. Story 29 asks which
// tokens nobody needs, and an answer accurate to the minute answers that question exactly as
// well as one accurate to the request while costing one write per minute instead of one per
// call.
const lastUsedResolution = time.Minute

// ServiceAccount is an automation identity. It holds no role of its own: the role is on each
// token, so one automation can hold a broad token and a narrow one without a second account.
type ServiceAccount struct {
	ID           uuid.UUID
	Organization string
	Name         string
	Description  string
	DisabledAt   time.Time
	CreatedAt    time.Time
	CreatedBy    string
}

// APIToken is a scoped automation credential as an operator sees it: never the token itself,
// which exists in a readable form exactly once, in the response that created it.
type APIToken struct {
	ID               uuid.UUID
	Organization     string
	ServiceAccountID uuid.UUID
	// Prefix is the first characters of the token, so an operator can tell which of theirs a
	// row is without the system holding a readable copy of any of them.
	Prefix     string
	Role       authz.Role
	ExpiresAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
	CreatedAt  time.Time
	CreatedBy  string
}

// Revoked reports whether this token has been withdrawn.
func (t APIToken) Revoked() bool { return !t.RevokedAt.IsZero() }

// NewAPIToken is what an administrator asked for.
type NewAPIToken struct {
	ServiceAccountID uuid.UUID
	Digest           []byte
	Prefix           string
	Role             authz.Role
	ExpiresAt        time.Time
}

// CreateServiceAccount records an automation identity, so that automation runs as something
// other than a person.
func (p *Placements) CreateServiceAccount(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	name, description string,
) (ServiceAccount, error) {
	return audited(ctx, p, principal, organization, audit.ActionServiceAccountCreated,
		func(ctx context.Context, transaction pgx.Tx) (
			ServiceAccount, audit.Target, audit.Detail, error,
		) {
			account := ServiceAccount{Organization: organization.String()}
			var disabled *time.Time
			if err := transaction.QueryRow(ctx, `
				INSERT INTO service_account (service_account_id, organization, name, description,
				                             created_by)
				VALUES ($1, $2, $3, $4, $5)
				RETURNING service_account_id, name, description, disabled_at, created_at, created_by`,
				uuid.New(), organization.String(), name, description, principal.ID()).
				Scan(&account.ID, &account.Name, &account.Description, &disabled,
					&account.CreatedAt, &account.CreatedBy); err != nil {
				if isUniqueViolation(err, "service_account_name_is_unique_per_organization") {
					return ServiceAccount{}, audit.Target{}, nil, ErrServiceAccountNameTaken
				}
				return ServiceAccount{}, audit.Target{}, nil,
					fmt.Errorf("creating a service account: %w", err)
			}
			if disabled != nil {
				account.DisabledAt = *disabled
			}
			return account,
				audit.Target{Kind: audit.TargetServiceAccount, ID: account.ID.String()},
				audit.Detail{"name": account.Name}, nil
		})
}

// ListServiceAccounts reports what automation this tenant has.
func (p *Placements) ListServiceAccounts(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
) ([]ServiceAccount, error) {
	if !principal.MemberOf(organization) {
		return nil, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT service_account_id, name, description, disabled_at, created_at, created_by
		  FROM service_account
		 WHERE organization = $1
		 ORDER BY created_at, service_account_id
		 LIMIT $2`, organization.String(), maxPageSize)
	if err != nil {
		return nil, fmt.Errorf("reading service accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]ServiceAccount, 0, 8)
	for rows.Next() {
		account := ServiceAccount{Organization: organization.String()}
		var disabled *time.Time
		if err := rows.Scan(&account.ID, &account.Name, &account.Description, &disabled,
			&account.CreatedAt, &account.CreatedBy); err != nil {
			return nil, fmt.Errorf("scanning a service account: %w", err)
		}
		if disabled != nil {
			account.DisabledAt = *disabled
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading service accounts: %w", err)
	}
	return accounts, nil
}

// RemoveServiceAccount deletes an automation identity and, with it, every token it holds. The
// cascade is the point: an account removed while its tokens still authenticated would be a
// deletion that deleted nothing that mattered.
func (p *Placements) RemoveServiceAccount(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionServiceAccountRemoved,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var name string
			err := transaction.QueryRow(ctx, `
				DELETE FROM service_account
				 WHERE organization = $1 AND service_account_id = $2
				RETURNING name`, organization.String(), id).Scan(&name)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, ErrServiceAccountUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("removing a service account: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetServiceAccount, ID: id.String()},
				audit.Detail{"name": name}, nil
		})
	return err
}

// IssueAPIToken records a scoped credential. Only the digest is stored; the token itself is
// shown once by the handler that called this and is not recoverable.
func (p *Placements) IssueAPIToken(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted NewAPIToken,
) (APIToken, error) {
	return audited(ctx, p, principal, organization, audit.ActionTokenIssued,
		func(ctx context.Context, transaction pgx.Tx) (APIToken, audit.Target, audit.Detail, error) {
			token := APIToken{Organization: organization.String()}
			var role string
			var lastUsed, revoked *time.Time
			if err := transaction.QueryRow(ctx, `
				INSERT INTO api_token (token_id, organization, service_account_id, token_digest,
				                       prefix, role, expires_at, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				RETURNING token_id, service_account_id, prefix, role, expires_at, last_used_at,
				          revoked_at, created_at, created_by`,
				uuid.New(), organization.String(), wanted.ServiceAccountID, wanted.Digest,
				wanted.Prefix, string(wanted.Role), wanted.ExpiresAt, principal.ID()).
				Scan(&token.ID, &token.ServiceAccountID, &token.Prefix, &role, &token.ExpiresAt,
					&lastUsed, &revoked, &token.CreatedAt, &token.CreatedBy); err != nil {
				if isForeignKeyViolation(err) {
					return APIToken{}, audit.Target{}, nil, ErrServiceAccountUnknown
				}
				return APIToken{}, audit.Target{}, nil, fmt.Errorf("issuing a token: %w", err)
			}
			token.Role = authz.Role(role)
			if lastUsed != nil {
				token.LastUsedAt = *lastUsed
			}
			if revoked != nil {
				token.RevokedAt = *revoked
			}
			// The prefix and the role are on the record; the token is not, and could not be —
			// the detail drops anything named like a credential on the way in.
			return token,
				audit.Target{Kind: audit.TargetAPIToken, ID: token.ID.String()},
				audit.Detail{
					"serviceAccountId": token.ServiceAccountID.String(),
					"prefix":           token.Prefix,
					"role":             role,
					"expiresAt":        token.ExpiresAt.UTC().Format(time.RFC3339),
				}, nil
		})
}

// ListAPITokens reports a tenant's tokens, including when each was last used, so an operator
// can retire the ones nobody needs.
func (p *Placements) ListAPITokens(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
) ([]APIToken, error) {
	if !principal.MemberOf(organization) {
		return nil, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT token_id, service_account_id, prefix, role, expires_at, last_used_at,
		       revoked_at, created_at, created_by
		  FROM api_token
		 WHERE organization = $1
		 ORDER BY created_at DESC, token_id
		 LIMIT $2`, organization.String(), maxPageSize)
	if err != nil {
		return nil, fmt.Errorf("reading tokens: %w", err)
	}
	defer rows.Close()

	tokens := make([]APIToken, 0, 8)
	for rows.Next() {
		token := APIToken{Organization: organization.String()}
		var role string
		var lastUsed, revoked *time.Time
		if err := rows.Scan(&token.ID, &token.ServiceAccountID, &token.Prefix, &role,
			&token.ExpiresAt, &lastUsed, &revoked, &token.CreatedAt, &token.CreatedBy); err != nil {
			return nil, fmt.Errorf("scanning a token: %w", err)
		}
		token.Role = authz.Role(role)
		if lastUsed != nil {
			token.LastUsedAt = *lastUsed
		}
		if revoked != nil {
			token.RevokedAt = *revoked
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading tokens: %w", err)
	}
	return tokens, nil
}

// RevokeAPIToken withdraws a credential. It marks rather than deletes, so that a leak
// investigation can still see when the token was last used and who issued it.
func (p *Placements) RevokeAPIToken(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionTokenRevoked,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			var prefix string
			err := transaction.QueryRow(ctx, `
				UPDATE api_token
				   SET revoked_at = coalesce(revoked_at, now()), revoked_by = $3
				 WHERE organization = $1 AND token_id = $2
				RETURNING prefix`, organization.String(), id, principal.ID()).Scan(&prefix)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, audit.Target{}, nil, ErrTokenUnknown
			}
			if err != nil {
				return struct{}{}, audit.Target{}, nil, fmt.Errorf("revoking a token: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetAPIToken, ID: id.String()},
				audit.Detail{"prefix": prefix}, nil
		})
	return err
}

// BearerPrincipal resolves an API token into who is holding it.
//
// It is placement-wide for the reason the session lookup is: a bearer token names no tenant,
// and a caller who could name one could try every one. The row that is found is itself the
// authority for the organization and the role.
//
// A token that is expired, revoked, or belongs to a disabled account is refused with the same
// error an unknown one gets. Telling them apart tells whoever is presenting a guess which half
// of it landed.
func (p *Placements) BearerPrincipal(
	ctx context.Context, digest []byte,
) (authz.Principal, error) {
	for _, name := range p.names() {
		principal, err := bearerFrom(ctx, p.pools[name], digest)
		if errors.Is(err, ErrTokenUnknown) {
			continue
		}
		if err != nil {
			// Reported rather than skipped: continuing would turn one database's outage into
			// "this token does not exist", which automation answers by retrying forever against
			// a credential that is in fact valid.
			return authz.Principal{}, fmt.Errorf("resolving a token in placement %q: %w", name, err)
		}
		return principal, nil
	}
	return authz.Principal{}, ErrTokenUnknown
}

func bearerFrom(ctx context.Context, on querier, digest []byte) (authz.Principal, error) {
	var (
		tokenID      uuid.UUID
		accountID    uuid.UUID
		accountName  string
		organization string
		role         string
	)
	// The last-used stamp is refreshed by the same statement that authenticates, and only when
	// it has gone stale, so this is a write at most once a minute per token rather than once
	// per call. The WHERE clause is what refuses an expired, revoked or disabled credential:
	// doing it in SQL means there is no window between the read and the decision.
	err := on.QueryRow(ctx, `
		UPDATE api_token
		   SET last_used_at = CASE WHEN last_used_at IS NULL
		                             OR last_used_at < now() - $2::INTERVAL
		                           THEN now() ELSE last_used_at END
		  FROM service_account account
		 WHERE api_token.token_digest = $1
		   AND api_token.revoked_at IS NULL
		   AND api_token.expires_at > now()
		   AND account.service_account_id = api_token.service_account_id
		   AND account.disabled_at IS NULL
		RETURNING api_token.token_id, api_token.organization, api_token.role,
		          account.service_account_id, account.name`,
		digest, lastUsedResolution).Scan(&tokenID, &organization, &role, &accountID, &accountName)
	if errors.Is(err, pgx.ErrNoRows) {
		return authz.Principal{}, ErrTokenUnknown
	}
	if err != nil {
		return authz.Principal{}, fmt.Errorf("reading a token: %w", err)
	}

	tenant, err := tenancy.NewOrganization(organization)
	if err != nil {
		return authz.Principal{}, fmt.Errorf("a token names an unusable organization: %w", err)
	}
	parsed, known := authz.ParseRole(role)
	if !known {
		// A token whose role this build no longer has authenticates to nothing rather than to
		// something guessed. The token is real; what it may do is not decidable, and guessing
		// either way is worse than refusing.
		return authz.Principal{}, ErrTokenUnknown
	}

	// The token is bound to ONE organization and ONE role, which is the whole difference
	// between this and the ambient root credential it replaces: the membership list it
	// produces has exactly one entry, whatever the account is.
	principal, err := authz.NewPrincipal(authz.KindServiceAccount, accountID.String(), accountName,
		[]authz.Membership{{Organization: tenant, Role: parsed}})
	if err != nil {
		return authz.Principal{}, err
	}
	return principal.WithCredential(tokenID.String()), nil
}
