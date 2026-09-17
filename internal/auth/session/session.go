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

const CookieName = "__Host-oc_session"
const tokenBytes = 32

var (
	ErrUnknown = errors.New("session unknown")
	ErrExpired = errors.New("session expired")
	ErrRevoked = errors.New("session revoked")
)

type Token string

type Session struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Organization    string
	IssuedAt        time.Time
	ExpiresAt       time.Time
	LastSeenAt      time.Time
	RevokedAt       time.Time
	ClientUserAgent string
	RemoteAddr      string
}

const (
	MaxClientUserAgentLength = 512
	MaxRemoteAddrLength      = 128
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

// Issue creates the stored session facts and the one-time credential presented to the client.
// The lifetime is deployment policy that configuration has already validated.
func Issue(userID uuid.UUID, organization string, lifetime time.Duration) (Token, []byte, Session, error) {
	token, digest, err := NewToken()
	if err != nil {
		return "", nil, Session{}, err
	}
	now := time.Now().UTC()
	return token, digest, Session{
		ID:           uuid.New(),
		UserID:       userID,
		Organization: organization,
		IssuedAt:     now,
		ExpiresAt:    now.Add(lifetime),
	}, nil
}

// Digest is what is stored for a token. SHA-256 rather than a password hash on purpose: the
// input is 256 bits of uniform randomness this process generated, so there is no dictionary to
// slow an attacker down against, and a slow hash on every request would be a cost paid per
// request for nothing.
func Digest(token Token) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Set writes the session cookie.
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
