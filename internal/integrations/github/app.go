// Package github is the GitHub provider: the Integration Type definition, the GitHub App
// credential machinery, the live installation verification, and the read-only bounded
// tools an investigation reads repositories, commits and pull requests through.
//
// The App credential (app id + private key) is DEPLOYMENT-level configuration; an
// Integration stores only the installation id and reads only the repositories that
// installation selected, by stable ids that survive renames. The vendor's payload shapes
// exist inside this package and nowhere else, and everything read here is text from a
// customer's repositories: it may be attacker-influenced and must never become an
// instruction, a destination, or an authorisation claim downstream.
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

// ErrNoApp reports a deployment that has not configured a GitHub App. Connecting GitHub
// is refused with this reason rather than accepted and left broken.
var ErrNoApp = errors.New(
	"this deployment has no GitHub App configured, so it cannot reach GitHub")

// jwtLifetime is how long one signed app JWT lives. GitHub caps it at ten minutes; nine
// leaves room for the hop.
const jwtLifetime = 9 * time.Minute

// jwtBackdate is how far in the past a JWT says it was issued. GitHub refuses tokens from
// the future, and a deployment's clock can lead GitHub's by a few seconds.
const jwtBackdate = time.Minute

// tokenRefreshMargin is how close to expiry a cached installation token may be used. An
// hour-long token is replaced in its last five minutes, so a read never starts with a
// token that dies mid-call.
const tokenRefreshMargin = 5 * time.Minute

// App holds the deployment's GitHub App credential and the installation tokens minted
// under it. A nil App is a deployment that configured none, and every method says so
// rather than failing strangely downstream.
type App struct {
	appID string
	key   *rsa.PrivateKey
	// client is the one vendor client, shared with everything else in this package.
	client *Client

	mu     sync.Mutex
	tokens map[int64]installationToken
}

// installationToken is one minted token and when it stops working.
type installationToken struct {
	token   string
	expires time.Time
}

// NewApp reads the deployment's App credential and binds it to the vendor client. The
// key errors never quote the file's contents, because those contents are the credential.
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

// Configured reports whether this deployment can reach GitHub at all.
func (a *App) Configured() bool { return a != nil && a.key != nil }

// installationToken returns a live token for one installation, minting only when the
// cached one is missing or inside the refresh margin. Reads spend the same rate budget
// minting does, so a mint per read would starve the reads it serves. Concurrent misses
// may mint in parallel: both tokens are valid, the last write wins, and serialising them
// would cost a lock held across a network call.
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

// jwt signs the App's own identity token, the credential every installation operation
// presents. Built on the standard library rather than a JWT dependency: the whole format
// is two base64 JSON parts and one RS256 signature, and a library would be a larger
// surface than the thing it wraps.
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

// parsePrivateKey reads the App key in either shape GitHub hands out. No error quotes the
// input: the input is the credential.
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

// cryptoSHA256 and sha256Sum name the one hash this signing uses.
const cryptoSHA256 = crypto.SHA256

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
