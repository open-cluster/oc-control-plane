package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrNoApp = errors.New(
	"this deployment has no GitHub App configured, so it cannot reach GitHub")

const jwtLifetime = 9 * time.Minute

// GitHub rejects future-issued JWTs, so backdate them to tolerate clock skew.
const jwtBackdate = time.Minute

const tokenRefreshMargin = 5 * time.Minute

type App struct {
	appID  string
	key    *rsa.PrivateKey
	client *Client
	mu     sync.Mutex
	tokens map[int64]installationToken
}

type installationToken struct {
	token   string
	expires time.Time
}

func NewApp(appID string, privateKeyPEM []byte, client *Client) (*App, error) {
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	app := &App{
		appID:  strings.TrimSpace(appID),
		key:    key,
		client: client,
		tokens: map[int64]installationToken{},
	}
	if app.appID == "" {
		return nil, errors.New("the GitHub App id must not be empty")
	}
	return app, nil
}

func (a *App) Configured() bool { return a != nil && a.key != nil }

func (a *App) installationToken(ctx context.Context, installation int64) (string, error) {
	if !a.Configured() {
		return "", ErrNoApp
	}

	a.mu.Lock()
	held, cached := a.tokens[installation]
	a.mu.Unlock()
	if cached && time.Until(held.expires) > tokenRefreshMargin {
		return held.token, nil
	}

	signed, err := a.jwt(time.Now())
	if err != nil {
		return "", err
	}
	minted, err := a.client.mintInstallationToken(ctx, signed, installation)
	if err != nil {
		return "", err
	}

	a.mu.Lock()
	a.tokens[installation] = minted
	a.mu.Unlock()
	return minted.token, nil
}

func (a *App) jwt(now time.Time) (string, error) {
	if !a.Configured() {
		return "", ErrNoApp
	}

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("encoding the JWT header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss": a.appID,
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("encoding the JWT claims: %w", err)
	}

	signed := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, cryptoSHA256, sha256Sum([]byte(signed)))
	if err != nil {
		return "", fmt.Errorf("signing the app JWT: %w", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("the GitHub App key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the GitHub App key could not be parsed as PKCS#1 or PKCS#8")
	}
	key, isRSA := parsed.(*rsa.PrivateKey)
	if !isRSA {
		return nil, errors.New("the GitHub App key is not an RSA key")
	}
	return key, nil
}

const cryptoSHA256 = crypto.SHA256

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
