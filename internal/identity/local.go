package identity

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

const maxConcurrentPasswordChecks = 8

var passwordCheckSlots = make(chan struct{}, maxConcurrentPasswordChecks)

type localCredentialRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
	Password    string `json:"password"`
}

type localMemberRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
	Password    string `json:"password"`
	Role        string `json:"role"`
}

type localPasswordRequest struct {
	Password string `json:"password"`
}

func (h Handlers) bootstrapLocalAdmin(writer http.ResponseWriter, request *http.Request) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	presented, present := bearerToken(request.Header.Get("Authorization"))
	if !present || !h.Bootstrap.accepts(presented) {
		writeJSON(writer, http.StatusUnauthorized, errorView{Error: "credential rejected"})
		return
	}
	if organization.String() != h.Bootstrap.Organization.String() {
		writeJSON(writer, http.StatusUnauthorized, errorView{Error: "credential rejected"})
		return
	}
	if h.Bootstrap.Role != authz.Admin {
		writeJSON(writer, http.StatusUnauthorized, errorView{Error: "credential rejected"})
		return
	}
	var body localCredentialRequest
	if !decode(writer, request, &body) {
		return
	}
	email, ok := localEmail(writer, body.Email)
	if !ok {
		return
	}
	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" {
		displayName = email
	}
	if len(displayName) > 256 {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "displayName must be at most 256 characters"})
		return
	}
	encoded, err := hashPassword(body.Password)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}

	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	user, memberships, err := h.Database.BootstrapLocalAdmin(
		ctx, organization, email, displayName, encoded)
	if errors.Is(err, storage.ErrLocalBootstrapComplete) {
		writeJSON(writer, http.StatusConflict,
			errorView{Error: "a local administrator already exists"})
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if err := h.issueSession(writer, request, organization, user, memberships,
		admission{Role: authz.Admin}); err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]bool{"bootstrapped": true})
}

func (h Handlers) localSignIn(writer http.ResponseWriter, request *http.Request) {
	select {
	case passwordCheckSlots <- struct{}{}:
		defer func() { <-passwordCheckSlots }()
	default:
		writeJSON(writer, http.StatusTooManyRequests,
			errorView{Error: "this sign-in cannot be completed"})
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	var body localCredentialRequest
	if !decode(writer, request, &body) {
		return
	}
	email, ok := localEmail(writer, body.Email)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	found, err := h.Database.LocalIdentityByEmail(ctx, organization, email)
	encoded := dummyPasswordHash
	if err == nil {
		encoded = found.PasswordHash
	} else if !errors.Is(err, storage.ErrLocalCredentialUnknown) &&
		!errors.Is(err, storage.ErrUserDisabled) {
		h.fail(writer, request, err)
		return
	}
	valid, rehash, verifyErr := verifyPassword(encoded, body.Password)
	if verifyErr != nil || err != nil || !valid {
		writeJSON(writer, http.StatusForbidden,
			errorView{Error: "this sign-in cannot be completed"})
		return
	}
	if rehash {
		replacement, hashErr := hashPassword(body.Password)
		if hashErr != nil {
			h.fail(writer, request, hashErr)
			return
		}
		if hashErr = h.Database.RehashLocalPassword(ctx, organization, found.User.ID,
			found.PasswordHash, replacement); hashErr != nil {
			h.fail(writer, request, hashErr)
			return
		}
	}
	if err := h.issueSession(writer, request, organization, found.User, found.Memberships,
		admission{}); err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"signedIn": true})
}

func (h Handlers) createLocalMember(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	var body localMemberRequest
	if !decode(writer, request, &body) {
		return
	}
	email, ok := localEmail(writer, body.Email)
	if !ok {
		return
	}
	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" {
		displayName = email
	}
	if len(displayName) > 256 {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "displayName must be at most 256 characters"})
		return
	}
	role, known := authz.ParseRole(body.Role)
	if !known {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "role must be admin, editor, or viewer"})
		return
	}
	encoded, err := hashPassword(body.Password)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	member, err := h.Database.CreateLocalMember(ctx, principal, organization,
		email, displayName, encoded, role)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, memberViewOf(member))
}

func (h Handlers) resetLocalPassword(writer http.ResponseWriter, request *http.Request) {
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
	var body localPasswordRequest
	if !decode(writer, request, &body) {
		return
	}
	encoded, err := hashPassword(body.Password)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	if err := h.Database.ResetLocalPassword(ctx, principal, organization, user, encoded); err != nil {
		h.fail(writer, request, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func localEmail(writer http.ResponseWriter, raw string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	parsed, err := mail.ParseAddress(normalized)
	if err != nil || parsed.Address != normalized || len(normalized) > 320 {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "email is not an address"})
		return "", false
	}
	return normalized, true
}

func (b Bootstrap) accepts(presented string) bool {
	if !b.Configured() {
		return false
	}
	digest := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(digest[:], b.Digest) == 1
}
