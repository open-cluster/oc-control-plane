// Package session owns the operator's server-side session: the record, its lifecycle, and the
// cookie that carries nothing but a pointer to it.
//
// The credential is an opaque random identifier and the row is the truth. It is deliberately
// not a JWT. A signed token that carries its own claims cannot be ended before it expires, so
// "Sign out ends my session on the server" and "revoke a departing colleague's access
// immediately" would both need a revocation list — which is a session table with extra steps
// and a second thing to keep consistent with the first.
//
// Only the digest of the identifier is stored. A disclosure of the sessions table therefore
// yields no usable credential, for the same reason sealed Integration credentials yield none.
//
// This package performs no I/O. internal/store/postgres writes and reads the rows; what a session IS,
// when it has expired, and how the cookie must be written live here.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// CookieName is what the browser holds. The prefix is not decoration: a cookie named __Host-
// is refused by the browser unless it is Secure, has Path=/ and carries no Domain, so the
// three attributes that matter are enforced by the user agent as well as by this code.
const CookieName = "__Host-oc_session"

// tokenBytes is how much entropy the opaque identifier carries. 32 bytes is well past what a
// birthday bound needs against any realistic number of live sessions, and the cost of being
// generous is a longer cookie.
const tokenBytes = 32

// Lifetime bounds. An organization sets its own within them, so a policy can be tightened past
// the default and cannot be widened past what this build is willing to serve.
const (
	MinLifetime     = 5 * time.Minute
	MaxLifetime     = 30 * 24 * time.Hour
	DefaultLifetime = 12 * time.Hour
)

// Refusals a session lookup can produce. They are separate values because the surface has to
// tell an operator WHY they were returned to sign-in — story 5 is precisely that a session
// which has expired says so rather than presenting a screen of error states.
var (
	// ErrUnknown reports a cookie naming no session. A signed-out session is this, because
	// signing out deletes the row rather than marking it.
	ErrUnknown = errors.New("session unknown")
	// ErrExpired reports a session whose lifetime has run out.
	ErrExpired = errors.New("session expired")
	// ErrRevoked reports a session an administrator ended.
	ErrRevoked = errors.New("session revoked")
)

// Token is the opaque credential a browser holds. It exists in a readable form exactly once,
// in the response that issues it, and is never stored or logged.
type Token string

// Session is one signed-in operator, as the server holds it.
type Session struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// Organization is the tenant the console was reading as when this session was issued. It
	// is a convenience for the interface rather than an authorization fact — what a session
	// may reach is decided from the user's memberships on every request, so changing it can
	// never widen access.
	Organization string
	IssuedAt     time.Time
	ExpiresAt    time.Time
	LastSeenAt   time.Time
	RevokedAt    time.Time
	// UserAgent and Address are what an administrator reads when deciding whether a live
	// session is one they recognise. Both are bounded, because both are the caller's strings.
	UserAgent string
	Address   string
}

// Bounds on what a session row records about its holder.
const (
	MaxUserAgentLength = 256
	MaxAddressLength   = 128
)

// Revoked reports whether an administrator ended this session.
func (s Session) Revoked() bool { return !s.RevokedAt.IsZero() }

// Live reports whether this session may authenticate a request at the given moment.
func (s Session) Live(now time.Time) bool {
	return !s.Revoked() && now.Before(s.ExpiresAt)
}

// Refusal reports why a session may not authenticate, or nil when it may. Revocation is
// checked before expiry so that an administrator who ended a session is told they did, rather
// than being told it timed out on its own.
func (s Session) Refusal(now time.Time) error {
	switch {
	case s.Revoked():
		return ErrRevoked
	case !now.Before(s.ExpiresAt):
		return ErrExpired
	default:
		return nil
	}
}

// NewToken mints an opaque credential and the digest that will be stored for it.
//
// The two are returned together because they must never be derived apart: a caller that
// digested the token itself could digest a different one, and a caller that stored the token
// would put a live credential in a column.
func NewToken() (Token, []byte, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("session: minting a token: %w", err)
	}
	token := Token(base64.RawURLEncoding.EncodeToString(raw))
	return token, Digest(token), nil
}

// Digest is what is stored for a token. SHA-256 rather than a password hash on purpose: the
// input is 256 bits of uniform randomness this process generated, so there is no dictionary to
// slow an attacker down against, and a slow hash on every request would be a cost paid per
// request for nothing.
func Digest(token Token) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ClampLifetime holds an organization's configured session lifetime inside what this build
// serves. Zero means the organization has configured none and takes the default.
func ClampLifetime(configured time.Duration) time.Duration {
	switch {
	case configured <= 0:
		return DefaultLifetime
	case configured < MinLifetime:
		return MinLifetime
	case configured > MaxLifetime:
		return MaxLifetime
	default:
		return configured
	}
}

// Set writes the session cookie.
//
// HttpOnly so script cannot read it, Secure so it never crosses a plaintext hop, SameSite=Lax
// so a cross-site form post does not carry it, and Path=/ so one cookie serves the whole
// surface. Lax rather than Strict because the sign-in redirect returns from the identity
// provider as a top-level navigation, and Strict would drop the cookie on exactly that hop.
func Set(writer http.ResponseWriter, token Token, expires time.Time) {
	http.SetCookie(writer, &http.Cookie{
		Name:     CookieName,
		Value:    string(token),
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// Clear removes the cookie in the same response that deleted the row, so the browser stops
// presenting a credential that is already dead rather than presenting it until it expires.
func Clear(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// FromRequest reads the token a request presents, if it presents one.
func FromRequest(request *http.Request) (Token, bool) {
	cookie, err := request.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return Token(cookie.Value), true
}
