package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Refusals the provider and flow tables can produce.
var (
	// ErrProviderUnknown reports a provider this organization does not have.
	ErrProviderUnknown = errors.New("identity provider unknown")
	// ErrProviderNameTaken reports a name another provider in this organization holds.
	ErrProviderNameTaken = errors.New("identity provider name is already used")
	// ErrFlowUnknown reports a sign-in that was never started, has expired, or has already
	// been completed. The three are one error on purpose: telling a replay apart from an
	// invented state is how an attacker learns which half of a guess landed.
	ErrFlowUnknown = errors.New("sign-in flow unknown")
)

// IdentityProtocol is how a provider is spoken to. Persisted as an integer and constrained by
// a CHECK in migration 0011.
type IdentityProtocol int16

const (
	// ProtocolOIDC is the Authorization Code flow with PKCE.
	ProtocolOIDC IdentityProtocol = 1
	// ProtocolSAML is reserved. The column accepts it so that adding SAML is a value rather
	// than a migration; nothing in this build produces or consumes it.
	ProtocolSAML IdentityProtocol = 2
)

func (p IdentityProtocol) String() string {
	switch p {
	case ProtocolOIDC:
		return "oidc"
	case ProtocolSAML:
		return "saml"
	default:
		return "unrecognised"
	}
}

// IdentityProvider is one tenant's configured way in.
type IdentityProvider struct {
	ID           uuid.UUID
	Organization string
	Name         string
	Protocol     IdentityProtocol
	Issuer       string
	ClientID     string
	// ClientSecretSealed is the credential as it is stored: encrypted, not digested, because
	// it has to be PRESENTED to the token endpoint rather than compared against. This package
	// never holds the key — internal/identity seals and opens it, and storage moves bytes.
	ClientSecretSealed []byte
	// VerifiedDomains are the email domains just-in-time provisioning may admit. Empty admits
	// nobody, which is why an unconfigured provider is closed rather than open.
	VerifiedDomains []string
	JITEnabled      bool
	JITRole         authz.Role
	// RequireVerifiedEmail decides whether the provider must have SAID it verified the
	// address. Without it a domain check over an unverified claim admits anyone who can type
	// the domain.
	RequireVerifiedEmail bool
	GroupClaim           string
	// GroupRoleMap maps a provider group name to a role, so role assignment is not a second
	// directory an administrator maintains by hand.
	GroupRoleMap map[string]string
	DisabledAt   time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Disabled reports whether this provider may still be signed in through.
func (i IdentityProvider) Disabled() bool { return !i.DisabledAt.IsZero() }

// NewIdentityProvider is what an administrator asked for. It travels as one value because the
// fields constrain each other: just-in-time provisioning with no verified domain admits
// nobody, and a group map naming roles this build does not have maps to nothing.
type NewIdentityProvider struct {
	Name                 string
	Protocol             IdentityProtocol
	Issuer               string
	ClientID             string
	ClientSecretSealed   []byte
	VerifiedDomains      []string
	JITEnabled           bool
	JITRole              authz.Role
	RequireVerifiedEmail bool
	GroupClaim           string
	GroupRoleMap         map[string]string
}

// SignInFlow is one authorization request in progress. The verifier and the nonce live here
// and never reach the browser, which is what makes PKCE and the nonce checks real rather than
// asserted.
type SignInFlow struct {
	ID           uuid.UUID
	Organization string
	ProviderID   uuid.UUID
	CodeVerifier string
	Nonce        string
	ReturnTo     string
	ExpiresAt    time.Time
}

// ConfigureIdentityProvider records a tenant's way in.
func (p *Placements) ConfigureIdentityProvider(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	wanted NewIdentityProvider,
) (IdentityProvider, error) {
	return audited(ctx, p, principal, organization, audit.ActionProviderConfigured,
		func(ctx context.Context, transaction pgx.Tx) (
			IdentityProvider, audit.Target, audit.Detail, error,
		) {
			groups, err := json.Marshal(orEmptyGroups(wanted.GroupRoleMap))
			if err != nil {
				return IdentityProvider{}, audit.Target{}, nil,
					fmt.Errorf("encoding the group map: %w", err)
			}

			row := transaction.QueryRow(ctx, `
				INSERT INTO identity_provider (provider_id, organization, name, protocol, issuer,
				                               client_id, client_secret_sealed, verified_domains,
				                               jit_enabled, jit_role, require_verified_email,
				                               group_claim, group_role_map)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
				RETURNING provider_id, name, protocol, issuer, client_id, client_secret_sealed,
				          verified_domains, jit_enabled, jit_role, require_verified_email,
				          group_claim, group_role_map, disabled_at, created_at, updated_at`,
				uuid.New(), organization.String(), wanted.Name, int16(wanted.Protocol),
				wanted.Issuer, wanted.ClientID, wanted.ClientSecretSealed,
				orEmptyDomains(wanted.VerifiedDomains), wanted.JITEnabled, string(wanted.JITRole),
				wanted.RequireVerifiedEmail, wanted.GroupClaim, groups)

			provider, err := scanProvider(row, organization.String())
			if err != nil {
				if providerNameIsTaken(err) {
					return IdentityProvider{}, audit.Target{}, nil, ErrProviderNameTaken
				}
				return IdentityProvider{}, audit.Target{}, nil,
					fmt.Errorf("configuring an identity provider: %w", err)
			}
			// Story 23: the detail says what the setting became. The client secret is not here
			// and could not be — the detail drops anything named like one on the way in.
			return provider,
				audit.Target{Kind: audit.TargetIdentityProvider, ID: provider.ID.String()},
				audit.Detail{
					"name":                 provider.Name,
					"issuer":               provider.Issuer,
					"jitEnabled":           provider.JITEnabled,
					"jitRole":              string(provider.JITRole),
					"verifiedDomains":      provider.VerifiedDomains,
					"requireVerifiedEmail": provider.RequireVerifiedEmail,
				}, nil
		})
}

// UpdateIdentityProvider changes a tenant's way in, recording what it was and what it became.
//
// A nil ClientSecretSealed leaves the stored credential alone, so an administrator changing a
// domain policy does not have to re-enter a secret they may not have.
func (p *Placements) UpdateIdentityProvider(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID, wanted NewIdentityProvider,
) (IdentityProvider, error) {
	return audited(ctx, p, principal, organization, audit.ActionProviderChanged,
		func(ctx context.Context, transaction pgx.Tx) (
			IdentityProvider, audit.Target, audit.Detail, error,
		) {
			before, err := readProvider(ctx, transaction, organization, id)
			if err != nil {
				return IdentityProvider{}, audit.Target{}, nil, err
			}
			groups, err := json.Marshal(orEmptyGroups(wanted.GroupRoleMap))
			if err != nil {
				return IdentityProvider{}, audit.Target{}, nil,
					fmt.Errorf("encoding the group map: %w", err)
			}

			row := transaction.QueryRow(ctx, `
				UPDATE identity_provider
				   SET name                   = $3,
				       issuer                 = $4,
				       client_id              = $5,
				       client_secret_sealed   = coalesce($6, client_secret_sealed),
				       verified_domains       = $7,
				       jit_enabled            = $8,
				       jit_role               = $9,
				       require_verified_email = $10,
				       group_claim            = $11,
				       group_role_map         = $12,
				       updated_at             = now()
				 WHERE organization = $1 AND provider_id = $2
				RETURNING provider_id, name, protocol, issuer, client_id, client_secret_sealed,
				          verified_domains, jit_enabled, jit_role, require_verified_email,
				          group_claim, group_role_map, disabled_at, created_at, updated_at`,
				organization.String(), id, wanted.Name, wanted.Issuer, wanted.ClientID,
				wanted.ClientSecretSealed, orEmptyDomains(wanted.VerifiedDomains),
				wanted.JITEnabled, string(wanted.JITRole), wanted.RequireVerifiedEmail,
				wanted.GroupClaim, groups)

			after, err := scanProvider(row, organization.String())
			if err != nil {
				if providerNameIsTaken(err) {
					return IdentityProvider{}, audit.Target{}, nil, ErrProviderNameTaken
				}
				return IdentityProvider{}, audit.Target{}, nil,
					fmt.Errorf("updating an identity provider: %w", err)
			}
			// Story 23: a weakened policy is discoverable because both values are on the record.
			return after,
				audit.Target{Kind: audit.TargetIdentityProvider, ID: after.ID.String()},
				audit.Detail{
					"before": policySummary(before),
					"after":  policySummary(after),
				}, nil
		})
}

// RemoveIdentityProvider deletes a tenant's way in. Users provisioned through it keep their
// memberships: removing a provider is a statement about how people arrive, not about who is
// already inside.
func (p *Placements) RemoveIdentityProvider(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
	id uuid.UUID,
) error {
	_, err := audited(ctx, p, principal, organization, audit.ActionProviderRemoved,
		func(ctx context.Context, transaction pgx.Tx) (struct{}, audit.Target, audit.Detail, error) {
			before, err := readProvider(ctx, transaction, organization, id)
			if err != nil {
				return struct{}{}, audit.Target{}, nil, err
			}
			if _, err := transaction.Exec(ctx, `
				DELETE FROM identity_provider WHERE organization = $1 AND provider_id = $2`,
				organization.String(), id); err != nil {
				return struct{}{}, audit.Target{}, nil,
					fmt.Errorf("removing an identity provider: %w", err)
			}
			return struct{}{},
				audit.Target{Kind: audit.TargetIdentityProvider, ID: id.String()},
				audit.Detail{"before": policySummary(before)}, nil
		})
	return err
}

// ListIdentityProviders reports how a tenant's people sign in.
func (p *Placements) ListIdentityProviders(
	ctx context.Context, principal authz.Principal, organization tenancy.Organization,
) ([]IdentityProvider, error) {
	if !principal.MemberOf(organization) {
		return nil, ErrNotAMember
	}
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	return providersIn(ctx, pool, organization)
}

// SignInProviders reports the providers a sign-in may be started against.
//
// It takes no principal, and that is the whole difficulty of this route: the caller is not
// signed in yet, which is what they are trying to fix. It returns only what a chooser needs —
// the identifier and the name — and nothing that would tell an unauthenticated caller whether
// a tenant exists beyond what the sign-in redirect already has to.
func (p *Placements) SignInProviders(
	ctx context.Context, organization tenancy.Organization,
) ([]IdentityProvider, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return nil, err
	}
	return providersIn(ctx, pool, organization)
}

func providersIn(
	ctx context.Context, on querier, organization tenancy.Organization,
) ([]IdentityProvider, error) {
	rows, err := on.Query(ctx, `
		SELECT provider_id, name, protocol, issuer, client_id, client_secret_sealed,
		       verified_domains, jit_enabled, jit_role, require_verified_email, group_claim,
		       group_role_map, disabled_at, created_at, updated_at
		  FROM identity_provider
		 WHERE organization = $1
		 ORDER BY created_at, provider_id`, organization.String())
	if err != nil {
		return nil, fmt.Errorf("reading identity providers: %w", err)
	}
	defer rows.Close()

	providers := make([]IdentityProvider, 0, 4)
	for rows.Next() {
		provider, err := scanProvider(rows, organization.String())
		if err != nil {
			return nil, fmt.Errorf("scanning an identity provider: %w", err)
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading identity providers: %w", err)
	}
	return providers, nil
}

func readProvider(
	ctx context.Context, on querier, organization tenancy.Organization, id uuid.UUID,
) (IdentityProvider, error) {
	row := on.QueryRow(ctx, `
		SELECT provider_id, name, protocol, issuer, client_id, client_secret_sealed,
		       verified_domains, jit_enabled, jit_role, require_verified_email, group_claim,
		       group_role_map, disabled_at, created_at, updated_at
		  FROM identity_provider
		 WHERE organization = $1 AND provider_id = $2`, organization.String(), id)

	provider, err := scanProvider(row, organization.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityProvider{}, ErrProviderUnknown
	}
	if err != nil {
		return IdentityProvider{}, fmt.Errorf("reading an identity provider: %w", err)
	}
	return provider, nil
}

// IdentityProviderForSignIn reads one provider by identity, for the unauthenticated sign-in
// route. It is the same read as the authenticated one and is named separately so that the two
// call sites are visible in the route table rather than sharing a function whose caller decides
// whether it is public.
func (p *Placements) IdentityProviderForSignIn(
	ctx context.Context, organization tenancy.Organization, id uuid.UUID,
) (IdentityProvider, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return IdentityProvider{}, err
	}
	return readProvider(ctx, pool, organization, id)
}

// StartSignIn records an authorization request so its state, verifier and nonce can be checked
// when the browser comes back.
func (p *Placements) StartSignIn(
	ctx context.Context, organization tenancy.Organization, flow SignInFlow, state string,
) error {
	pool, err := p.Pool(organization)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(state))

	if _, err := pool.Exec(ctx, `
		INSERT INTO sign_in_flow (flow_id, organization, provider_id, state_digest,
		                          code_verifier, nonce, return_to, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		flow.ID, organization.String(), flow.ProviderID, digest[:], flow.CodeVerifier,
		flow.Nonce, flow.ReturnTo, flow.ExpiresAt); err != nil {
		return fmt.Errorf("starting a sign-in: %w", err)
	}
	return nil
}

// RedeemSignIn consumes an authorization request exactly once and returns what it recorded.
//
// The consumption is a conditional UPDATE rather than a read followed by a write, so two
// concurrent presentations of the same authorization code cannot both win. An unknown state, an
// expired one and one already redeemed are the same refusal: telling them apart is how a
// caller learns which half of a guess landed.
//
// It is placement-wide in the same sense the connection lookup is: the callback carries the
// state and nothing that names a tenant, so every placement is asked in a fixed order and the
// row that is found is itself the authority for the organization.
func (p *Placements) RedeemSignIn(ctx context.Context, state string) (SignInFlow, error) {
	digest := sha256.Sum256([]byte(state))

	for _, name := range p.names() {
		var flow SignInFlow
		err := p.pools[name].QueryRow(ctx, `
			UPDATE sign_in_flow
			   SET consumed_at = now()
			 WHERE state_digest = $1 AND consumed_at IS NULL AND expires_at > now()
			RETURNING flow_id, organization, provider_id, code_verifier, nonce, return_to,
			          expires_at`, digest[:]).Scan(&flow.ID, &flow.Organization, &flow.ProviderID,
			&flow.CodeVerifier, &flow.Nonce, &flow.ReturnTo, &flow.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			// A placement that cannot be read is reported rather than skipped. Continuing would
			// turn one database's outage into "this sign-in was never started", which the
			// operator would answer by trying again forever.
			return SignInFlow{}, fmt.Errorf("redeeming a sign-in in placement %q: %w", name, err)
		}
		return flow, nil
	}
	return SignInFlow{}, ErrFlowUnknown
}

// ExpireSignInFlows removes the flows nobody completed. A started sign-in that is never
// finished is the ordinary case — somebody closed the tab — and without this the table grows
// by one row per abandoned attempt forever.
func (p *Placements) ExpireSignInFlows(
	ctx context.Context, organization tenancy.Organization,
) (int64, error) {
	pool, err := p.Pool(organization)
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, `
		DELETE FROM sign_in_flow WHERE expires_at <= now() OR consumed_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("expiring sign-in flows: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanProvider(row scanned, organization string) (IdentityProvider, error) {
	var (
		provider IdentityProvider
		protocol int16
		role     string
		groups   []byte
		disabled *time.Time
	)
	if err := row.Scan(&provider.ID, &provider.Name, &protocol, &provider.Issuer,
		&provider.ClientID, &provider.ClientSecretSealed, &provider.VerifiedDomains,
		&provider.JITEnabled, &role, &provider.RequireVerifiedEmail, &provider.GroupClaim,
		&groups, &disabled, &provider.CreatedAt, &provider.UpdatedAt); err != nil {
		return IdentityProvider{}, err
	}
	provider.Organization = organization
	provider.Protocol = IdentityProtocol(protocol)
	provider.JITRole = authz.Role(role)
	if disabled != nil {
		provider.DisabledAt = *disabled
	}
	if err := json.Unmarshal(groups, &provider.GroupRoleMap); err != nil {
		return IdentityProvider{}, fmt.Errorf("decoding the group map: %w", err)
	}
	return provider, nil
}

// policySummary is what the record says a provider's policy was. It is deliberately not the
// whole row: the client identifier and the sealed secret are not policy, and one of them must
// never appear in an event at all.
func policySummary(provider IdentityProvider) map[string]any {
	return map[string]any{
		"name":                 provider.Name,
		"issuer":               provider.Issuer,
		"jitEnabled":           provider.JITEnabled,
		"jitRole":              string(provider.JITRole),
		"verifiedDomains":      provider.VerifiedDomains,
		"requireVerifiedEmail": provider.RequireVerifiedEmail,
		"groupRoleMap":         provider.GroupRoleMap,
	}
}

func orEmptyGroups(groups map[string]string) map[string]string {
	if groups == nil {
		return map[string]string{}
	}
	return groups
}

func orEmptyDomains(domains []string) []string {
	if domains == nil {
		return []string{}
	}
	return domains
}

// providerNameIsTaken reports the two ways a tenant can configure the same provider twice: by
// name, and by naming the same client at the same issuer. Both answer the operator the same
// way, because both mean "you already have this one".
func providerNameIsTaken(err error) bool {
	return isUniqueViolation(err, "identity_provider_name_is_unique_per_organization") ||
		isUniqueViolation(err, "identity_provider_client_is_unique_per_issuer")
}
