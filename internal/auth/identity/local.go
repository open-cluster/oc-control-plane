package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const maxConcurrentPasswordChecks = 8

var passwordCheckSlots = make(chan struct{}, maxConcurrentPasswordChecks)

type localSignInRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type localBootstrapRequest struct {
	OrganizationName string `json:"organizationName"`
	Email            string `json:"email"`
	DisplayName      string `json:"displayName,omitempty"`
	Password         string `json:"password"`
}

type memberCreationRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
	Password    string `json:"password"`
	Role        string `json:"role"`
}

func (h Handlers) bootstrapLocalAdmin(writer http.ResponseWriter, request *http.Request) {
	var body localBootstrapRequest
	if !decode(writer, request, &body) {
		return
	}
	presented, present := bearerToken(request.Header.Get("Authorization"))
	if !present || !h.Bootstrap.accepts(presented) {
		writeJSON(writer, http.StatusUnauthorized, errorView{Error: "credential rejected"})
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
	organizationName := strings.TrimSpace(body.OrganizationName)
	if organizationName == "" || len(organizationName) > 256 {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "organizationName must be between 1 and 256 characters"})
		return
	}
	encoded, err := hashPassword(body.Password)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}

	token, digest, issued, _, err := h.prepareSession(
		request, uuid.UUID{}, uuid.Nil)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	_, _, err = h.Database.BootstrapLocalUser(
		ctx, organizationName, email, displayName, encoded, issued, digest)
	if errors.Is(err, storage.ErrLocalBootstrapComplete) {
		writeJSON(writer, http.StatusConflict,
			errorView{Error: "a local administrator already exists"})
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	session.Set(writer, token, issued.ExpiresAt)
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
	var body localSignInRequest
	if !decode(writer, request, &body) {
		return
	}
	email, ok := localEmail(writer, body.Email)
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	found, err := h.Database.LocalIdentityByEmail(ctx, email)
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
		if hashErr = h.Database.RehashLocalPassword(ctx, found.User.ID,
			found.PasswordHash, replacement); hashErr != nil {
			h.fail(writer, request, hashErr)
			return
		}
		found.PasswordHash = replacement
	}
	organization := found.Membership.Organization
	if err := h.issueSession(writer, request, organization, found.User,
		found.PasswordHash); err != nil {
		if errors.Is(err, storage.ErrLocalCredentialUnknown) {
			writeJSON(writer, http.StatusForbidden, errorView{Error: "this sign-in cannot be completed"})
			return
		}
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"signedIn": true})
}

func (h Handlers) createMember(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	organization := h.organization(request)
	var body memberCreationRequest
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

const (
	passwordMemory      = 64 * 1024
	passwordIterations  = 3
	passwordParallelism = 4
	passwordSaltBytes   = 16
	passwordKeyBytes    = 32
	minPasswordBytes    = 12
	maxPasswordBytes    = 1024
)

var errPasswordFormat = errors.New("local password verifier has an unusable format")

func hashPassword(password string) (string, error) {
	if len(password) < minPasswordBytes || len(password) > maxPasswordBytes {
		return "", fmt.Errorf("password must be between %d and %d bytes",
			minPasswordBytes, maxPasswordBytes)
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("minting a password salt: %w", err)
	}
	return encodePassword(password, salt, passwordMemory, passwordIterations,
		passwordParallelism), nil
}

func encodePassword(password string, salt []byte, memory, iterations uint32, parallelism uint8) string {
	key := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, passwordKeyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func verifyPassword(encoded, password string) (bool, bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false, errPasswordFormat
	}
	version, err := parsePasswordParameter(parts[2], "v")
	if err != nil || version != uint64(argon2.Version) {
		return false, false, errPasswordFormat
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return false, false, errPasswordFormat
	}
	memory, err := parsePasswordParameter(parameters[0], "m")
	if err != nil {
		return false, false, errPasswordFormat
	}
	iterations, err := parsePasswordParameter(parameters[1], "t")
	if err != nil {
		return false, false, errPasswordFormat
	}
	parallel, err := parsePasswordParameter(parameters[2], "p")
	if err != nil || parallel > 255 || memory == 0 || iterations == 0 {
		return false, false, errPasswordFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false, false, errPasswordFormat
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != passwordKeyBytes {
		return false, false, errPasswordFormat
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory),
		uint8(parallel), uint32(len(want)))
	valid := subtle.ConstantTimeCompare(got, want) == 1
	needsRehash := memory != passwordMemory || iterations != passwordIterations ||
		parallel != passwordParallelism
	return valid, needsRehash, nil
}

func parsePasswordParameter(raw, name string) (uint64, error) {
	prefix := name + "="
	if !strings.HasPrefix(raw, prefix) {
		return 0, errPasswordFormat
	}
	value, err := strconv.ParseUint(strings.TrimPrefix(raw, prefix), 10, 32)
	if err != nil {
		return 0, errPasswordFormat
	}
	return value, nil
}

var dummyPasswordHash = encodePassword("password that is never accepted",
	[]byte("fixed timing salt"), passwordMemory, passwordIterations, passwordParallelism)

type localPasswordChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (h Handlers) changeLocalPassword(writer http.ResponseWriter, request *http.Request) {
	principal := h.caller(request)
	var body localPasswordChangeRequest
	if !decode(writer, request, &body) {
		return
	}
	if body.CurrentPassword == "" || len(body.CurrentPassword) > maxPasswordBytes || len(body.NewPassword) < minPasswordBytes || len(body.NewPassword) > maxPasswordBytes {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "invalid password length"})
		return
	}
	select {
	case passwordCheckSlots <- struct{}{}:
		defer func() { <-passwordCheckSlots }()
	default:
		writeJSON(writer, http.StatusTooManyRequests, errorView{Error: "password check capacity reached"})
		return
	}
	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()
	previous, err := h.Database.LocalPasswordHash(ctx, principal)
	if errors.Is(err, storage.ErrLocalCredentialUnknown) {
		writeJSON(writer, http.StatusForbidden, errorView{Error: "local password change refused"})
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	valid, _, err := verifyPassword(previous, body.CurrentPassword)
	if err != nil || !valid {
		writeJSON(writer, http.StatusForbidden, errorView{Error: "local password change refused"})
		return
	}
	replacement, err := hashPassword(body.NewPassword)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if err = h.Database.ChangeLocalPassword(ctx, principal, previous, replacement); err != nil {
		if errors.Is(err, storage.ErrLocalCredentialUnknown) {
			writeJSON(writer, http.StatusForbidden, errorView{Error: "local password change refused"})
			return
		}
		h.fail(writer, request, err)
		return
	}
	session.Clear(writer)
	writer.WriteHeader(http.StatusNoContent)
}

// RecoverLocalPassword accepts bounded stdin and uses deployment database authority.
func RecoverLocalPassword(ctx context.Context, database *storage.Database, user uuid.UUID, input io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(input, maxPasswordBytes+3))
	if err != nil {
		return errors.New("cannot read recovery password")
	}
	defer clear(raw)
	password := bytes.TrimSuffix(raw, []byte("\n"))
	password = bytes.TrimSuffix(password, []byte("\r"))
	if len(password) < minPasswordBytes || len(password) > maxPasswordBytes {
		return errors.New("recovery password must be between 12 and 1024 bytes")
	}
	encoded, err := hashPassword(string(password))
	if err != nil {
		return errors.New("cannot prepare recovery password")
	}
	if err = database.RecoverLocalPassword(ctx, user, encoded); err != nil {
		if errors.Is(err, storage.ErrLocalCredentialUnknown) {
			return errors.New("recovery requires an existing enabled local User")
		}
		return errors.New("password recovery failed; no credential change was confirmed")
	}
	return nil
}
