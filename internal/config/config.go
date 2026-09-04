// Package config loads and validates the control plane's process configuration from the
// environment. Configuration is non-secret with one deliberate exception: the database
// connection string carries a password, so configuration names a file holding the DSN rather
// than the DSN itself. No environment value ever carries a credential, and no error ever
// quotes a DSN file's contents — a failed start must not write a password into a log.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Defaults for the optional settings.
const (
	defaultAuthenticationMode                     = "local"
	defaultInvestigationWorkers                   = 8
	defaultInvestigationMaxPendingPerOrganization = 100
	defaultModelContextWindowTokens               = 128_000
)

// Environment variable names, listed once so errors and documentation cannot drift.
var SupportedEnvironmentKeys = []string{
	EnvConfigFile,
	EnvHTTPAddress, EnvOperatorPublicURL,
	EnvDatabaseDSNFile,
	EnvAuthenticationMode, EnvOperatorTokenFile,
	EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecretFile,
	EnvRelayAddress, EnvRelaySPKIPins,
	EnvModelProvider, EnvModelName, EnvModelKeyFile, EnvModelContextWindowSize,
	EnvInvestigationWorkers, EnvInvestigationMaxPendingPerOrganization,
	EnvSealingKeyFile,
	EnvLogLevel, EnvOTLPEndpoint,
	EnvSlackClientID, EnvSlackClientSecretFile, EnvSlackSigningSecretFile,
	EnvGitHubAppID, EnvGitHubAppKeyFile,
}

const (
	EnvHTTPAddress                            = "OC_SERVER_ADDRESS"
	EnvOperatorPublicURL                      = "OC_PUBLIC_URL"
	EnvDatabaseDSNFile                        = "OC_DATABASE_DSN_FILE"
	EnvAuthenticationMode                     = "OC_AUTH_MODE"
	EnvOperatorTokenFile                      = "OC_BOOTSTRAP_TOKEN_FILE"
	EnvOIDCIssuer                             = "OC_OIDC_ISSUER"
	EnvOIDCClientID                           = "OC_OIDC_CLIENT_ID"
	EnvOIDCClientSecretFile                   = "OC_OIDC_CLIENT_SECRET_FILE"
	EnvRelayAddress                           = "OC_RELAY_ADDRESS"
	EnvRelaySPKIPins                          = "OC_RELAY_SPKI_PINS"
	EnvModelProvider                          = "OC_AI_PROVIDER"
	EnvModelName                              = "OC_AI_MODEL"
	EnvModelKeyFile                           = "OC_AI_API_KEY_FILE"
	EnvModelContextWindowSize                 = "OC_AI_CONTEXT_WINDOW_SIZE"
	EnvInvestigationWorkers                   = "OC_INVESTIGATION_WORKERS"
	EnvInvestigationMaxPendingPerOrganization = "OC_MAX_PENDING_INVESTIGATIONS_PER_ORGANIZATION"
	EnvSealingKeyFile                         = "OC_ENCRYPTION_KEY_FILE"
	EnvLogLevel                               = "OC_LOG_LEVEL"
	EnvOTLPEndpoint                           = "OC_OTLP_ENDPOINT"
	EnvSlackClientID                          = "OC_SLACK_CLIENT_ID"
	EnvSlackClientSecretFile                  = "OC_SLACK_CLIENT_SECRET_FILE"
	EnvSlackSigningSecretFile                 = "OC_SLACK_SIGNING_SECRET_FILE"
	EnvGitHubAppID                            = "OC_GITHUB_APP_ID"
	EnvGitHubAppKeyFile                       = "OC_GITHUB_APP_PRIVATE_KEY_FILE"
)

// Config is the validated process configuration.
type Config struct {
	// HTTPAddress is the shared listen address for every HTTP route group.
	HTTPAddress string

	// DatabaseDSN is the single deployment database connection string, resolved from
	// the file named by configuration. It never appears in an environment value.
	DatabaseDSN string

	// OTLPEndpoint is the trace collector, host:port. Empty disables trace export, which
	// is the correct default for a process with no collector configured.
	OTLPEndpoint string

	// RelayAddress is the listen address for the Relay endpoint, which is deliberately
	// separate from the HTTP surface: it speaks a different protocol to a different kind of
	// caller, and sharing a port would put the two behind one set of middleware. Empty
	// disables it, which is correct for a deployment that serves no relays.
	RelayAddress string

	// RelaySPKIPins are this control plane's own public key digests, handed to a Relay at
	// enrolment so every later connection is pinned to a key rather than trusting a
	// certificate authority. More than one exists so a rotation can overlap.
	RelaySPKIPins []string

	// OperatorTokenDigest is the SHA-256 of the bootstrap token. The token is read from the file
	// the operator named, reduced to this, and discarded: the process holds no copy of it, so
	// there is nothing here to log or echo by accident.
	//
	// It is what the old shared operator token became. The difference is the whole point: it is
	// bound to the organization and role below rather than reaching every tenant. Its limits are
	// worth stating rather than implying — it has no expiry and no revocation row, because it
	// exists only to bootstrap a deployment. Revoking it means changing the mounted file and
	// restarting.
	OperatorTokenDigest []byte

	// OperatorTokenOrganization is the one tenant the bootstrap credential reaches.
	OperatorTokenOrganization string

	// OperatorTokenRole is the one role it holds there. It defaults to admin, because a
	// deployment with no members yet needs a credential that can create the first one.
	OperatorTokenRole string

	// OperatorPublicURL is where this surface is reachable from a browser, and what the redirect
	// URI registered with an identity provider is built from.
	OperatorPublicURL string

	// AuthenticationMode is local by default. local+oidc keeps local recovery available and
	// adds one deployment-configured generic OIDC adapter.
	AuthenticationMode string
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string

	// SealingKey seals presentable credentials at rest: an identity provider's client
	// secret, an integration's outbound token. Empty means this deployment cannot hold
	// one, and submitting one is refused with that reason rather than stored in the clear.
	SealingKey []byte

	// SlackClientID and SlackClientSecret are the OpenCluster Slack app's OAuth client.
	// Both empty means this deployment offers no one-click Slack install and serves the
	// pasted-token form instead. SlackSigningSecret is what inbound events are verified
	// against; empty means the events endpoint is not served at all, and the integration
	// truthfully reports its inbound capabilities as unavailable.
	SlackClientID      string
	SlackClientSecret  string
	SlackSigningSecret string
	// GitHubAppID and GitHubAppKey are the deployment's GitHub App credential; both empty
	// means this deployment cannot reach GitHub, and connecting it is refused live with
	// that reason.
	GitHubAppID  string
	GitHubAppKey []byte

	// The model deployment. ModelProvider empty means this deployment cannot investigate,
	// and opening one is refused with that reason. The credential travels as a file's
	// contents, never as an environment value.
	ModelProvider            string
	ModelName                string
	ModelKey                 string
	ModelContextWindowTokens int

	InvestigationWorkers                    int
	MaxPendingInvestigationsPerOrganization int
}

// Load reads configuration through lookup (os.LookupEnv in production) and validates every
// value, failing on the first problem and naming the offending variable.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		HTTPAddress:                             ":8080",
		AuthenticationMode:                      defaultAuthenticationMode,
		InvestigationWorkers:                    defaultInvestigationWorkers,
		MaxPendingInvestigationsPerOrganization: defaultInvestigationMaxPendingPerOrganization,
		ModelContextWindowTokens:                defaultModelContextWindowTokens,
	}

	var err error
	if raw, ok := lookup(EnvHTTPAddress); ok && strings.TrimSpace(raw) != "" {
		cfg.HTTPAddress = strings.TrimSpace(raw)
	}
	if err = validateHostPort(cfg.HTTPAddress); err != nil {
		return Config{}, fmt.Errorf("%s must be a host:port listen address: %w", EnvHTTPAddress, err)
	}
	if cfg.DatabaseDSN, err = databaseDSN(lookup); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseDSN == "" {
		return Config{}, fmt.Errorf("%s is required", EnvDatabaseDSNFile)
	}
	if cfg.OTLPEndpoint, err = optionalHostPort(lookup, EnvOTLPEndpoint); err != nil {
		return Config{}, err
	}
	if cfg.RelayAddress, err = optionalHostPort(lookup, EnvRelayAddress); err != nil {
		return Config{}, err
	}
	if cfg.RelaySPKIPins, err = relaySPKIPins(lookup, cfg.RelayAddress); err != nil {
		return Config{}, err
	}
	if cfg.OperatorTokenDigest, err = operatorTokenDigest(lookup, cfg.HTTPAddress); err != nil {
		return Config{}, err
	}
	cfg.OperatorTokenOrganization, cfg.OperatorTokenRole = "local", defaultOperatorTokenRole
	if cfg.OperatorPublicURL, err = optionalBrowserURL(lookup, EnvOperatorPublicURL); err != nil {
		return Config{}, err
	}
	if cfg.OperatorPublicURL == "" {
		cfg.OperatorPublicURL = "http://localhost:8080"
	}
	if err = authentication(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.SealingKey, err = sealingKey(lookup); err != nil {
		return Config{}, err
	}
	if cfg.GitHubAppID, cfg.GitHubAppKey, err = gitHubApp(lookup); err != nil {
		return Config{}, err
	}
	if err = slackApp(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if err = modelDeployment(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.ModelContextWindowTokens, err = positiveInteger(
		lookup, EnvModelContextWindowSize, defaultModelContextWindowTokens); err != nil {
		return Config{}, err
	}
	if cfg.InvestigationWorkers, err = positiveInteger(
		lookup, EnvInvestigationWorkers, defaultInvestigationWorkers); err != nil {
		return Config{}, err
	}
	if cfg.MaxPendingInvestigationsPerOrganization, err = positiveInteger(
		lookup, EnvInvestigationMaxPendingPerOrganization,
		defaultInvestigationMaxPendingPerOrganization); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func positiveInteger(
	lookup func(string) (string, bool), key string, fallback int,
) (int, error) {
	raw, ok := lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func authentication(lookup func(string) (string, bool), cfg *Config) error {
	mode := defaultAuthenticationMode
	if raw, ok := lookup(EnvAuthenticationMode); ok && strings.TrimSpace(raw) != "" {
		mode = strings.ToLower(strings.TrimSpace(raw))
	}
	if mode == "oidc" {
		mode = "local+oidc"
	}
	if mode != "local" && mode != "local+oidc" {
		return fmt.Errorf("%s must be local or oidc", EnvAuthenticationMode)
	}
	cfg.AuthenticationMode = mode
	issuer, _ := lookup(EnvOIDCIssuer)
	clientID, _ := lookup(EnvOIDCClientID)
	secretFile, _ := lookup(EnvOIDCClientSecretFile)
	issuer, clientID, secretFile = strings.TrimSpace(issuer), strings.TrimSpace(clientID),
		strings.TrimSpace(secretFile)
	configured := issuer != "" || clientID != "" || secretFile != ""
	if mode == "local" {
		if configured {
			return fmt.Errorf("%s must be local+oidc when OIDC settings are present",
				EnvAuthenticationMode)
		}
		return nil
	}
	if issuer == "" || clientID == "" || secretFile == "" {
		return fmt.Errorf("%s, %s, and %s are all required in local+oidc mode",
			EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecretFile)
	}
	parsed, err := url.Parse(issuer)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "https" && (parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1")) {
		return fmt.Errorf("%s must be an HTTPS issuer URL", EnvOIDCIssuer)
	}
	secret, err := readSecretFile(secretFile)
	if err != nil {
		return fmt.Errorf("%s: client secret file cannot be read", EnvOIDCClientSecretFile)
	}
	if secret == "" {
		return fmt.Errorf("%s: client secret file is empty", EnvOIDCClientSecretFile)
	}
	cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret = issuer, clientID, secret
	return nil
}

func databaseDSN(lookup func(string) (string, bool)) (string, error) {
	path, _ := lookup(EnvDatabaseDSNFile)
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	dsn, err := readSecretFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvDatabaseDSNFile, err)
	}
	return dsn, nil
}

func readSecretFile(path string) (string, error) {
	raw, err := (MountedSecretSource{}).Read("", path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", errors.New("file is empty")
	}
	return value, nil
}

func optionalHostPort(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", nil
	}
	value = strings.TrimSpace(value)
	if err := validateHostPort(value); err != nil {
		return "", fmt.Errorf("%s must be host:port: %w", key, err)
	}
	return value, nil
}

// validateHostPort accepts only a bare listen or dial address: an optional host with no
// scheme and no path, plus a usable port. net.SplitHostPort alone would accept
// "http://host:8080" by splitting on the scheme's colon.
func validateHostPort(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if strings.ContainsAny(host, "/\\") {
		return fmt.Errorf("invalid host %q", host)
	}
	if strings.Contains(port, "/") {
		return fmt.Errorf("invalid port %q", port)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65_535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

// modelDeployment reads the model settings. A provider set demands a model name and a
// key: half a deployment would serve an investigations surface that fails on first use,
// and whoever set one variable is still reading when this refuses. What the values MEAN —
// a provider this build implements, an effort level it recognises — is judged at the
// composition root, which owns that vocabulary.
func modelDeployment(lookup func(string) (string, bool), cfg *Config) error {
	provider, _ := lookup(EnvModelProvider)
	cfg.ModelProvider = strings.TrimSpace(provider)
	name, _ := lookup(EnvModelName)
	cfg.ModelName = strings.TrimSpace(name)

	path, _ := lookup(EnvModelKeyFile)
	path = strings.TrimSpace(path)
	if cfg.ModelProvider == "" {
		if path != "" || cfg.ModelName != "" {
			return fmt.Errorf("%s is required when a model is configured", EnvModelProvider)
		}
		return nil
	}
	if cfg.ModelName == "" {
		return fmt.Errorf("%s is required when %s is set: a constructed model identifier "+
			"is a 404 at best", EnvModelName, EnvModelProvider)
	}
	if path == "" {
		return fmt.Errorf("%s is required when %s is set", EnvModelKeyFile, EnvModelProvider)
	}
	key, err := readSecretFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvModelKeyFile, err)
	}
	cfg.ModelKey = key
	return nil
}

// gitHubApp reads the deployment's GitHub App credential: both halves or neither. Half a
// credential would serve a catalog whose GitHub entry can never work, and the person who
// set one variable is still reading when this refuses. The key file's contents never
// appear in an error.
func gitHubApp(lookup func(string) (string, bool)) (string, []byte, error) {
	id, _ := lookup(EnvGitHubAppID)
	id = strings.TrimSpace(id)
	path, _ := lookup(EnvGitHubAppKeyFile)
	path = strings.TrimSpace(path)

	switch {
	case id == "" && path == "":
		return "", nil, nil
	case id == "":
		return "", nil, fmt.Errorf("%s is required when %s is set",
			EnvGitHubAppID, EnvGitHubAppKeyFile)
	case path == "":
		return "", nil, fmt.Errorf("%s is required when %s is set",
			EnvGitHubAppKeyFile, EnvGitHubAppID)
	}
	raw, err := (MountedSecretSource{}).Read(EnvGitHubAppKeyFile, path)
	if err != nil {
		return "", nil, err
	}
	if len(raw) == 0 {
		return "", nil, fmt.Errorf("%s: the key file is empty", EnvGitHubAppKeyFile)
	}
	return id, raw, nil
}

// slackApp reads the deployment's Slack app registration.
//
// Two independent halves. The OAuth client is both parts or neither, for the reason the
// GitHub one is: half of it offers a connect button that cannot finish, and the person who
// set one variable is still reading when this refuses. The signing secret stands alone —
// it serves the events endpoint rather than the connect flow, and a deployment may
// legitimately have one without the other in either direction.
//
// Neither secret's contents ever appear in an error.
func slackApp(lookup func(string) (string, bool), cfg *Config) error {
	clientID, _ := lookup(EnvSlackClientID)
	clientID = strings.TrimSpace(clientID)
	secretPath, _ := lookup(EnvSlackClientSecretFile)
	secretPath = strings.TrimSpace(secretPath)

	switch {
	case clientID == "" && secretPath != "":
		return fmt.Errorf("%s is required when %s is set",
			EnvSlackClientID, EnvSlackClientSecretFile)
	case clientID != "" && secretPath == "":
		return fmt.Errorf("%s is required when %s is set",
			EnvSlackClientSecretFile, EnvSlackClientID)
	case clientID != "":
		secret, err := readSecretFile(secretPath)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvSlackClientSecretFile, err)
		}
		cfg.SlackClientID, cfg.SlackClientSecret = clientID, secret
	}

	signingPath, _ := lookup(EnvSlackSigningSecretFile)
	if signingPath = strings.TrimSpace(signingPath); signingPath == "" {
		return nil
	}
	signing, err := readSecretFile(signingPath)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvSlackSigningSecretFile, err)
	}
	cfg.SlackSigningSecret = signing
	return nil
}

// relaySPKIPins reads the pin set the Relay endpoint advertises at enrolment. Pins are
// required whenever the endpoint is enabled: a Relay handed no pin has no trust anchor for
// its next connection and would have to fall back to trusting a certificate authority,
// which is the property key pinning exists to remove.
func relaySPKIPins(lookup func(string) (string, bool), relayAddress string) ([]string, error) {
	raw, _ := lookup(EnvRelaySPKIPins)
	fields, listErr := decodeList(raw)
	if listErr != nil {
		return nil, fmt.Errorf("%s: invalid list: %w", EnvRelaySPKIPins, listErr)
	}
	pins := make([]string, 0, len(fields))
	for _, field := range fields {
		pin := strings.TrimSpace(field)
		if pin == "" {
			continue
		}
		digest, err := base64.StdEncoding.DecodeString(pin)
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf(
				"%s: each pin must be a base64-encoded SHA-256 digest of a SubjectPublicKeyInfo",
				EnvRelaySPKIPins)
		}
		pins = append(pins, pin)
	}

	if relayAddress == "" {
		return nil, nil
	}
	if len(pins) == 0 {
		return nil, fmt.Errorf("%s is required when %s is set", EnvRelaySPKIPins, EnvRelayAddress)
	}
	return pins, nil
}
