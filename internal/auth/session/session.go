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
)

type Token string

type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
}

func NewToken() (Token, []byte, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("session: minting a token: %w", err)
	}
	token := Token(base64.RawURLEncoding.EncodeToString(raw))
	return token, Digest(token), nil
}

func Issue(userID uuid.UUID, lifetime time.Duration) (Token, []byte, Session, error) {
	token, digest, err := NewToken()
	if err != nil {
		return "", nil, Session{}, err
	}
	now := time.Now().UTC()
	return token, digest, Session{
		UserID: userID, ExpiresAt: now.Add(lifetime),
	}, nil
}

func Digest(token Token) []byte {
	// Tokens have 256 bits of generated entropy, so a slow password hash adds cost without
	// improving resistance to guessing.
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

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

func FromRequest(request *http.Request) (Token, bool) {
	cookie, err := request.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return Token(cookie.Value), true
}
