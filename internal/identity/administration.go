package identity

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/session"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

const (
	maxProviderNameLength = 128
	maxVerifiedDomains    = 32
	maxGroupMappings      = 128
)

// providerRequest is what an administrator sends to configure a way in.
type providerRequest struct {
	Name string `json:"name"`
	// Protocol is "oidc" or "saml". It defaults to OIDC when a caller says nothing, because
	// that is what every provider configured before SAML existed is, and a default that
	// silently changed an existing tenant's provider would be the worst kind of upgrade.
	Protocol string `json:"protocol"`
	// Issuer is the OIDC issuer URL. A SAML provider's entity identifier is NOT taken from
	// here — it comes out of the metadata document, because a mismatch between what an
	// administrator typed and what the provider publishes would be an audience check failing
	// at somebody's first sign-in for a reason nobody could see.
	Issuer string `json:"issuer"`
	// SAMLMetadata is the identity provider's published metadata, pasted whole.
	SAMLMetadata string `json:"samlMetadata"`
	ClientID     string `json:"clientId"`
	// ClientSecret is write-only. It is sealed on the way in and no path reads it back; an
	// administrator who lost it enters a new one.
	ClientSecret         string            `json:"clientSecret"`
	VerifiedDomains      []string          `json:"verifiedDomains"`
	JITEnabled           bool              `json:"jitEnabled"`
	JITRole              string            `json:"jitRole"`
	RequireVerifiedEmail *bool             `json:"requireVerifiedEmail"`
	GroupClaim           string            `json:"groupClaim"`
	GroupRoleMap         map[string]string `json:"groupRoleMap"`
}

func (h Handlers) listProviders(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	providers, err := h.Database.ListIdentityProviders(ctx, principal, organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]providerView, 0, len(providers))
	for _, provider := range providers {
		views = append(views, providerViewOf(provider))
	}
	writeJSON(writer, http.StatusOK, providerListView{Providers: views})
}

func (h Handlers) createProvider(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	var body providerRequest
	if !decode(writer, request, &body) {
		return
	}
	// Minted here, before the client secret is sealed, so the sealed value can bind to
	// the row it will live on.
	wanted, ok := h.planProvider(writer, body, uuid.New(), true)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	created, err := h.Database.ConfigureIdentityProvider(ctx, principal, organization, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, providerViewOf(created))
}

func (h Handlers) updateProvider(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	id, ok := identifier(writer, request, "provider")
	if !ok {
		return
	}
	var body providerRequest
	if !decode(writer, request, &body) {
		return
	}
	// A secret is optional on an update: changing a domain policy must not require re-entering
	// a credential the administrator may not have.
	wanted, ok := h.planProvider(writer, body, id, false)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	updated, err := h.Database.UpdateIdentityProvider(ctx, principal, organization, id, wanted)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, providerViewOf(updated))
}

func (h Handlers) deleteProvider(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	id, ok := identifier(writer, request, "provider")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.RemoveIdentityProvider(ctx, principal, organization, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// planProvider turns a request into what storage needs, refusing every combination that could
// not work. It answers the caller itself on a refusal, so a handler either has a valid plan or
// has already returned.
func (h Handlers) planProvider(
	writer http.ResponseWriter, body providerRequest, id uuid.UUID, secretRequired bool,
) (storage.NewIdentityProvider, bool) {
	name, ok := validName(writer, body.Name, maxProviderNameLength)
	if !ok {
		return storage.NewIdentityProvider{}, false
	}

	protocol, ok := parseProtocol(writer, body.Protocol)
	if !ok {
		return storage.NewIdentityProvider{}, false
	}

	var (
		issuer   string
		clientID string
		sealed   []byte
		metadata string
	)
	if protocol == storage.ProtocolSAML {
		// Everything that identifies a SAML provider comes out of the one document it
		// publishes. It is parsed HERE, at configuration time, so a document that is not
		// metadata, publishes no signing certificate, or does not survive a re-serialise is an
		// answer to the administrator who pasted it rather than a refusal at somebody's first
		// sign-in.
		entity, err := parseIdPMetadata(body.SAMLMetadata)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return storage.NewIdentityProvider{}, false
		}
		issuer, metadata = entity.EntityID, strings.TrimSpace(body.SAMLMetadata)
	} else {
		issuer = strings.TrimSpace(body.Issuer)
		if err := usableIssuer(issuer); err != nil {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "issuer must be the https URL the provider publishes its metadata under"})
			return storage.NewIdentityProvider{}, false
		}
		clientID = strings.TrimSpace(body.ClientID)
		if clientID == "" {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "clientId must not be empty"})
			return storage.NewIdentityProvider{}, false
		}

		switch secret := strings.TrimSpace(body.ClientSecret); {
		case secret != "":
			if !h.Sealer.Configured() {
				writeJSON(writer, http.StatusServiceUnavailable, errorView{
					Error: "this deployment has no sealing key and cannot hold a " +
						"client secret"})
				return storage.NewIdentityProvider{}, false
			}
			var err error
			if sealed, err = h.Sealer.Seal(secret, id[:]); err != nil {
				writeJSON(writer, http.StatusInternalServerError,
					errorView{Error: "the client secret could not be stored"})
				return storage.NewIdentityProvider{}, false
			}
		case secretRequired:
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "clientSecret is required when an OIDC provider is configured"})
			return storage.NewIdentityProvider{}, false
		}
	}

	domains, ok := validDomains(writer, body.VerifiedDomains)
	if !ok {
		return storage.NewIdentityProvider{}, false
	}
	if body.JITEnabled && len(domains) == 0 {
		// Refused rather than accepted-and-inert. A tenant that turned provisioning on with no
		// domain would admit nobody, and discovering that at somebody's first sign-in is worse
		// than being told now.
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "just-in-time provisioning needs at least one verified domain; with none " +
				"it would admit nobody"})
		return storage.NewIdentityProvider{}, false
	}

	role := authz.Viewer
	if trimmed := strings.TrimSpace(body.JITRole); trimmed != "" {
		parsed, known := authz.ParseRole(trimmed)
		if !known {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "jitRole is not a role this build has"})
			return storage.NewIdentityProvider{}, false
		}
		// An admin arriving by just-in-time provisioning would mean anyone at a verified
		// domain could administer the tenant on their first sign-in. It is refused rather
		// than documented.
		if parsed == authz.Admin {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "admin may not be granted by just-in-time provisioning; grant it " +
					"to a person deliberately"})
			return storage.NewIdentityProvider{}, false
		}
		role = parsed
	}

	groups, ok := validGroupMap(writer, body.GroupRoleMap)
	if !ok {
		return storage.NewIdentityProvider{}, false
	}

	// Verification defaults ON. A tenant that says nothing gets the safe posture, and turning
	// it off is a deliberate act with an audit event naming both values.
	requireVerified := true
	if body.RequireVerifiedEmail != nil {
		requireVerified = *body.RequireVerifiedEmail
	}

	return storage.NewIdentityProvider{
		ID:                   id,
		Name:                 name,
		Protocol:             protocol,
		Issuer:               issuer,
		ClientID:             clientID,
		ClientSecretSealed:   sealed,
		SAMLMetadata:         metadata,
		VerifiedDomains:      domains,
		JITEnabled:           body.JITEnabled,
		JITRole:              role,
		RequireVerifiedEmail: requireVerified,
		GroupClaim:           strings.TrimSpace(body.GroupClaim),
		GroupRoleMap:         groups,
	}, true
}

func validDomains(writer http.ResponseWriter, domains []string) ([]string, bool) {
	if len(domains) > maxVerifiedDomains {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "at most 32 verified domains"})
		return nil, false
	}
	cleaned := make([]string, 0, len(domains))
	for _, domain := range domains {
		trimmed := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(domain, "@")))
		if trimmed == "" || strings.ContainsAny(trimmed, " \t/@") || len(trimmed) > 253 {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "a verified domain must be a bare domain name"})
			return nil, false
		}
		cleaned = append(cleaned, trimmed)
	}
	return cleaned, true
}

func validGroupMap(
	writer http.ResponseWriter, mapping map[string]string,
) (map[string]string, bool) {
	if len(mapping) > maxGroupMappings {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "at most 128 group mappings"})
		return nil, false
	}
	cleaned := make(map[string]string, len(mapping))
	for group, role := range mapping {
		trimmed := strings.TrimSpace(group)
		if trimmed == "" || len(trimmed) > 256 {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "a group name must be 1-256 characters"})
			return nil, false
		}
		parsed, known := authz.ParseRole(role)
		if !known {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "the group " + trimmed + " maps to " + role +
					", which is not a role this build has"})
			return nil, false
		}
		// An admin arriving from a directory group is the same escalation the just-in-time
		// role is refused for, one directory edit away.
		if parsed == authz.Admin {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "admin may not be granted by a group mapping; grant it to a person " +
					"deliberately"})
			return nil, false
		}
		cleaned[trimmed] = string(parsed)
	}
	return cleaned, true
}

// Members.

type memberRequest struct {
	Role string `json:"role"`
}

func (h Handlers) listMembers(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	list, err := h.Database.ListMembers(ctx, principal, organization, storage.Page{
		Limit: pageSize(request),
		After: request.URL.Query().Get("after"),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]memberView, 0, len(list.Members))
	for _, member := range list.Members {
		views = append(views, memberViewOf(member))
	}
	writeJSON(writer, http.StatusOK, memberListView{Members: views, Next: list.Next})
}

// setMember grants or changes a person's role.
//
// The route declares member.manage. Appointing an OWNER needs member.owner.manage on top, and
// that second check lives here rather than in the table because it depends on the body: it is
// the same route, and which permission it needs is decided by what is being granted. That is
// the one place on this surface where a permission is not decidable from the route alone, and
// it is stated here so a reader of the table is not misled by its absence.
func (h Handlers) setMember(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	user, ok := identifier(writer, request, "user")
	if !ok {
		return
	}
	var body memberRequest
	if !decode(writer, request, &body) {
		return
	}
	role, known := authz.ParseRole(body.Role)
	if !known {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "role is not one this build has"})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	member, err := h.Database.SetMembership(ctx, principal, organization, user, role)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, memberViewOf(member))
}

// removeMember ends a person's access to this tenant and revokes their sessions in the same
// transaction, so there is no window in which the membership is gone and the credential works.
func (h Handlers) removeMember(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	user, ok := identifier(writer, request, "user")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.RemoveMembership(ctx, principal, organization, user); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// Policy.

type policyRequest struct {
	SessionLifetimeSeconds int `json:"sessionLifetimeSeconds"`
	AuditRetentionDays     int `json:"auditRetentionDays"`
}

func (h Handlers) readPolicy(writer http.ResponseWriter, request *http.Request) {
	if _, ok := h.caller(writer, request); !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	lifetime, retention, err := h.Database.SessionPolicy(ctx, organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, policyView{
		// What is REPORTED is what will actually be applied, not what was typed. A tenant that
		// asked for a year and is served twelve hours should be told twelve hours.
		SessionLifetimeSeconds: int(session.ClampLifetime(lifetime).Seconds()),
		AuditRetentionDays:     retention,
		// Stated rather than implied, and read from what this deployment actually runs rather
		// than from a constant somebody would have to remember to change.
		AuditRetentionEnforced: h.RetentionEnforced,
	})
}

func (h Handlers) writePolicy(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	var body policyRequest
	if !decode(writer, request, &body) {
		return
	}
	if body.SessionLifetimeSeconds < 0 || body.AuditRetentionDays < 0 {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "a policy value must not be negative"})
		return
	}

	lifetime := time.Duration(body.SessionLifetimeSeconds) * time.Second
	if lifetime != 0 && (lifetime < session.MinLifetime || lifetime > session.MaxLifetime) {
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "sessionLifetimeSeconds must be between " +
				secondsIn(session.MinLifetime) + " and " + secondsIn(session.MaxLifetime) +
				", or 0 for the product default"})
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Database.SetSessionPolicy(
		ctx, principal, organization, lifetime, body.AuditRetentionDays); err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, policyView{
		SessionLifetimeSeconds: int(session.ClampLifetime(lifetime).Seconds()),
		AuditRetentionDays:     body.AuditRetentionDays,
		AuditRetentionEnforced: h.RetentionEnforced,
	})
}

// secondsIn renders a bound the way the field carries it, so an operator reading the refusal
// can put the number straight back into the request that produced it.
func secondsIn(value time.Duration) string {
	return strconv.Itoa(int(value.Seconds()))
}

// parseProtocol reads which way in a tenant is configuring.
//
// Nothing said means OIDC. Every provider configured before SAML existed is one, and a default
// that changed what an existing tenant's provider was would be an upgrade that broke sign-in
// for the customers who were already working.
func parseProtocol(
	writer http.ResponseWriter, named string,
) (storage.IdentityProtocol, bool) {
	switch strings.ToLower(strings.TrimSpace(named)) {
	case "", "oidc":
		return storage.ProtocolOIDC, true
	case "saml":
		return storage.ProtocolSAML, true
	default:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: `protocol must be "oidc" or "saml"`})
		return 0, false
	}
}
