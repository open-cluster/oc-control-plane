package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Bounds on an API token. The maximum lifetime is the point of the whole feature: a token with
// no deadline is the ambient root credential this slice exists to replace, so there is no way
// to ask for one.
const (
	maxAccountNameLength = 128
	minTokenLifetime     = time.Hour
	maxTokenLifetime     = 365 * 24 * time.Hour
	defaultTokenLifetime = 90 * 24 * time.Hour
	// tokenPrefixLength is how much of a token is stored in the clear so an operator can tell
	// their tokens apart. Short enough that it is not a meaningful head start on guessing the
	// rest, which is 256 bits of randomness.
	tokenPrefixLength = 8
)

// tokenLabel prefixes every issued token. It exists so that a token pasted into a repository
// is recognisable as one by a secret scanner, which is the difference between a leak found in
// minutes and a leak found in a breach report.
const tokenLabel = "occp_"

type serviceAccountRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type tokenRequest struct {
	ServiceAccountID string `json:"serviceAccountId"`
	Role             string `json:"role"`
	// ExpiresInSeconds is bounded rather than optional-and-unbounded. Zero takes the default.
	ExpiresInSeconds int `json:"expiresInSeconds"`
}

func (h Handlers) listServiceAccounts(writer http.ResponseWriter, request *http.Request) {
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

	accounts, err := h.Placements.ListServiceAccounts(ctx, principal, organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]serviceAccountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, serviceAccountViewOf(account))
	}
	writeJSON(writer, http.StatusOK, serviceAccountListView{ServiceAccounts: views})
}

func (h Handlers) createServiceAccount(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	var body serviceAccountRequest
	if !decode(writer, request, &body) {
		return
	}
	name, ok := validName(writer, body.Name, maxAccountNameLength)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	created, err := h.Placements.CreateServiceAccount(
		ctx, principal, organization, name, strings.TrimSpace(body.Description))
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, serviceAccountViewOf(created))
}

func (h Handlers) removeServiceAccount(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	id, ok := identifier(writer, request, "account")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Placements.RemoveServiceAccount(ctx, principal, organization, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (h Handlers) listTokens(writer http.ResponseWriter, request *http.Request) {
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

	tokens, err := h.Placements.ListAPITokens(ctx, principal, organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]tokenView, 0, len(tokens))
	for _, token := range tokens {
		views = append(views, tokenViewOf(token))
	}
	writeJSON(writer, http.StatusOK, tokenListView{Tokens: views})
}

// issueToken mints a scoped credential and shows it exactly once.
//
// This is the only response in this package that carries a secret. It is not logged, not
// returned again, and not recoverable: what is stored is a digest, and an operator who lost
// the token issues another and revokes this one.
func (h Handlers) issueToken(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	var body tokenRequest
	if !decode(writer, request, &body) {
		return
	}

	account, ok := identifierIn(writer, body.ServiceAccountID, "serviceAccountId")
	if !ok {
		return
	}
	role, known := authz.ParseRole(body.Role)
	if !known {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "role is not one this build has"})
		return
	}
	// An owner token would be a credential that can appoint owners, sitting in a CI
	// environment variable. A person may hold that role; a token may not.
	if role == authz.OrganizationOwner {
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "a token may not hold the owner role; automation runs as something with a " +
				"stated job"})
		return
	}
	lifetime, ok := tokenLifetime(writer, body.ExpiresInSeconds)
	if !ok {
		return
	}

	secret, digest, err := newAPIToken()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError,
			errorView{Error: "a token could not be generated"})
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	issued, err := h.Placements.IssueAPIToken(ctx, principal, organization, storage.NewAPIToken{
		ServiceAccountID: account,
		Digest:           digest,
		Prefix:           secret[:len(tokenLabel)+tokenPrefixLength],
		Role:             role,
		ExpiresAt:        time.Now().UTC().Add(lifetime),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	writeJSON(writer, http.StatusCreated, issuedTokenView{
		Token:  tokenViewOf(issued),
		Secret: secret,
		SecretNotice: "This token is shown once. Store it now; it cannot be read back. " +
			"It reaches one organization with one role and stops working at its expiry.",
	})
}

func (h Handlers) revokeToken(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	id, ok := identifier(writer, request, "token")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	if err := h.Placements.RevokeAPIToken(ctx, principal, organization, id); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// newAPIToken mints a credential and the digest that will be stored for it. The two are
// returned together for the reason a session's are: a caller that derived one from the other
// could derive it from something else.
func newAPIToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	secret := tokenLabel + base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(secret))
	return secret, digest[:], nil
}

func tokenLifetime(writer http.ResponseWriter, seconds int) (time.Duration, bool) {
	if seconds == 0 {
		return defaultTokenLifetime, true
	}
	lifetime := time.Duration(seconds) * time.Second
	if lifetime < minTokenLifetime || lifetime > maxTokenLifetime {
		writeJSON(writer, http.StatusBadRequest, errorView{
			Error: "expiresInSeconds must be between " + secondsIn(minTokenLifetime) +
				" and " + secondsIn(maxTokenLifetime) + ", or 0 for the default; a token " +
				"without a deadline is the credential this replaces"})
		return 0, false
	}
	return lifetime, true
}
