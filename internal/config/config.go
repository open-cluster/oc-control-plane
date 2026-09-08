package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultAuthenticationMode                     = "local"
	defaultInvestigationWorkers                   = 8
	defaultInvestigationMaxPendingPerOrganization = 100
)

var SupportedEnvironmentKeys = []string{
	EnvHTTPAddress,
	EnvOperatorPublicURL,
	EnvDatabaseDSN,
	EnvDatabaseDSNFile,
	EnvAuthenticationMode,
	EnvOperatorToken,
	EnvOperatorTokenFile,
	EnvOIDCIssuer,
	EnvOIDCClientID,
	EnvOIDCClientSecret,
	EnvOIDCClientSecretFile,
	EnvRelayAddress,
	EnvRelaySPKIPins,
	EnvModelProvider,
	EnvModelName,
	EnvModelKey,
	EnvModelKeyFile,
	EnvModelContextWindowSize,
	EnvModelMaxOutputTokens,
	EnvInvestigationWorkers,
	EnvInvestigationMaxPendingPerOrganization,
	EnvSealingKey,
	EnvSealingKeyFile,
	EnvLogLevel,
	EnvOTLPEndpoint,
	EnvSlackClientID,
	EnvSlackClientSecret,
	EnvSlackClientSecretFile,
	EnvSlackSigningSecret,
	EnvSlackSigningSecretFile,
	EnvGitHubAppID,
	EnvGitHubAppKey,
	EnvGitHubAppKeyFile,
}

const (
	EnvHTTPAddress                            = "OC_SERVER_ADDRESS"
	EnvOperatorPublicURL                      = "OC_PUBLIC_URL"
	EnvDatabaseDSN                            = "OC_DATABASE_DSN"
	EnvDatabaseDSNFile                        = "OC_DATABASE_DSN_FILE"
	EnvAuthenticationMode                     = "OC_AUTH_MODE"
	EnvOperatorToken                          = "OC_BOOTSTRAP_TOKEN"
	EnvOperatorTokenFile                      = "OC_BOOTSTRAP_TOKEN_FILE"
	EnvOIDCIssuer                             = "OC_OIDC_ISSUER"
	EnvOIDCClientID                           = "OC_OIDC_CLIENT_ID"
	EnvOIDCClientSecret                       = "OC_OIDC_CLIENT_SECRET"
	EnvOIDCClientSecretFile                   = "OC_OIDC_CLIENT_SECRET_FILE"
	EnvRelayAddress                           = "OC_RELAY_ADDRESS"
	EnvRelaySPKIPins                          = "OC_RELAY_SPKI_PINS"
	EnvModelProvider                          = "OC_AI_PROVIDER"
	EnvModelName                              = "OC_AI_MODEL"
	EnvModelKey                               = "OC_AI_API_KEY"
	EnvModelKeyFile                           = "OC_AI_API_KEY_FILE"
	EnvModelContextWindowSize                 = "OC_AI_CONTEXT_WINDOW_SIZE"
	EnvModelMaxOutputTokens                   = "OC_AI_MAX_OUTPUT_SIZE"
	EnvInvestigationWorkers                   = "OC_INVESTIGATION_WORKERS"
	EnvInvestigationMaxPendingPerOrganization = "OC_MAX_PENDING_INVESTIGATIONS_PER_ORGANIZATION"
	EnvSealingKey                             = "OC_ENCRYPTION_KEY"
	EnvSealingKeyFile                         = "OC_ENCRYPTION_KEY_FILE"
	EnvLogLevel                               = "OC_LOG_LEVEL"
	EnvOTLPEndpoint                           = "OC_OTLP_ENDPOINT"
	EnvSlackClientID                          = "OC_SLACK_CLIENT_ID"
	EnvSlackClientSecret                      = "OC_SLACK_CLIENT_SECRET"
	EnvSlackClientSecretFile                  = "OC_SLACK_CLIENT_SECRET_FILE"
	EnvSlackSigningSecret                     = "OC_SLACK_SIGNING_SECRET"
	EnvSlackSigningSecretFile                 = "OC_SLACK_SIGNING_SECRET_FILE"
	EnvGitHubAppID                            = "OC_GITHUB_APP_ID"
	EnvGitHubAppKey                           = "OC_GITHUB_APP_PRIVATE_KEY"
	EnvGitHubAppKeyFile                       = "OC_GITHUB_APP_PRIVATE_KEY_FILE"
)

// Config is the validated process configuration.
type Config struct {
	LogLevel slog.Level
	// HTTPAddress is the shared listen address for every HTTP route group.
	HTTPAddress string

	// DatabaseDSN is the single deployment database connection string, resolved from
	// a direct environment value or the configured file.
	DatabaseDSN string

	// OTLPEndpoint is the trace collector, host:port. Empty disables trace export, which
	// is the correct default for a process with no collector configured.
	OTLPEndpoint string

	// RelayAddress is the listen address for the Relay endpoint,
	// which is deliberately separate from the HTTP surface;
	RelayAddress string

	// RelaySPKIPins are this control plane's own public key digests, handed to a Relay at
	// enrolment so every later connection is pinned to a key rather than trusting a
	// certificate authority. More than one exists so a rotation can overlap.
	RelaySPKIPins []string

	// OperatorTokenDigest is the SHA-256 of the bootstrap token. The token is read from the file
	// the operator named, reduced to this, and discarded: the process holds no copy of it, so
	// there is nothing here to log or echo by accident.
	OperatorTokenDigest []byte

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
	ModelMaxOutputTokens     int64

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
	}

	var err error
	if raw, _ := lookup(EnvLogLevel); strings.TrimSpace(raw) != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(strings.TrimSpace(raw))); err != nil {
			return Config{}, fmt.Errorf("%s must be debug, info, warn, or error", EnvLogLevel)
		}
	}
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

	if cfg.ModelContextWindowTokens, err = positiveInteger(lookup,
		EnvModelContextWindowSize,
		0); err != nil {
		return Config{}, err
	}
	modelOutput, outputErr := positiveInteger(lookup, EnvModelMaxOutputTokens, 0)
	if outputErr != nil {
		return Config{}, outputErr
	}
	cfg.ModelMaxOutputTokens = int64(modelOutput)
	if cfg.InvestigationWorkers, err = positiveInteger(lookup,
		EnvInvestigationWorkers,
		defaultInvestigationWorkers); err != nil {
		return Config{}, err
	}
	if cfg.MaxPendingInvestigationsPerOrganization, err = positiveInteger(lookup,
		EnvInvestigationMaxPendingPerOrganization,
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
	secret, err := readSecretText(lookup, EnvOIDCClientSecretFile)
	if err != nil {
		return err
	}
	issuer, clientID = strings.TrimSpace(issuer), strings.TrimSpace(clientID)
	configured := issuer != "" || clientID != "" || secret != ""
	if mode == "local" {
		if configured {
			return fmt.Errorf("%s must be local+oidc when OIDC settings are present",
				EnvAuthenticationMode)
		}
		return nil
	}
	if issuer == "" || clientID == "" || secret == "" {
		return fmt.Errorf("%s, %s, and %s are all required in local+oidc mode",
			EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecretFile)
	}
	parsed, err := url.Parse(issuer)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
		(parsed.Scheme != "https" && (parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1")) {
		return fmt.Errorf("%s must be an HTTPS issuer URL", EnvOIDCIssuer)
	}
	cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret = issuer, clientID, secret
	return nil
}

func databaseDSN(lookup func(string) (string, bool)) (string, error) {
	return readSecretText(lookup, EnvDatabaseDSNFile)
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
// key: half a deployment would serve an investigations surface that fails on first use
func modelDeployment(lookup func(string) (string, bool), cfg *Config) error {
	provider, _ := lookup(EnvModelProvider)
	cfg.ModelProvider = strings.TrimSpace(provider)
	name, _ := lookup(EnvModelName)
	cfg.ModelName = strings.TrimSpace(name)
	key, err := readSecretText(lookup, EnvModelKeyFile)
	if err != nil {
		return err
	}
	if cfg.ModelProvider == "" {
		if key != "" || cfg.ModelName != "" {
			return fmt.Errorf("%s is required when a model is configured", EnvModelProvider)
		}
		return nil
	}
	if cfg.ModelName == "" {
		return fmt.Errorf("%s is required when %s is set: a constructed model identifier "+
			"is a 404 at best", EnvModelName, EnvModelProvider)
	}
	if key == "" {
		return fmt.Errorf("%s is required when %s is set", EnvModelKeyFile, EnvModelProvider)
	}
	cfg.ModelKey = key
	return nil
}

// gitHubApp reads the deployment's GitHub App credential;
func gitHubApp(lookup func(string) (string, bool)) (string, []byte, error) {
	id, _ := lookup(EnvGitHubAppID)
	id = strings.TrimSpace(id)
	key, err := readSecret(lookup, EnvGitHubAppKeyFile)
	if err != nil {
		return "", nil, err
	}
	if (id == "") != (len(key) == 0) {
		return "", nil, fmt.Errorf("%s and a GitHub App private key must be configured together", EnvGitHubAppID)
	}
	return id, key, nil
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
	secret, err := readSecretText(lookup, EnvSlackClientSecretFile)
	if err != nil {
		return err
	}
	if (clientID == "") != (secret == "") {
		return fmt.Errorf("%s and %s or its direct value must be configured together", EnvSlackClientID, EnvSlackClientSecretFile)
	}
	signing, err := readSecretText(lookup, EnvSlackSigningSecretFile)
	if err != nil {
		return err
	}
	cfg.SlackClientID, cfg.SlackClientSecret, cfg.SlackSigningSecret = clientID, secret, signing
	return nil
}

// relaySPKIPins reads the pin set the Relay endpoint advertises at enrolment. Pins are
// required whenever the endpoint is enabled: a Relay handed no pin has no trust anchor for
// its next connection and would have to fall back to trusting a certificate authority,
// which is the property key pinning exists to remove.
func relaySPKIPins(lookup func(string) (string, bool), relayAddress string) ([]string, error) {
	raw, _ := lookup(EnvRelaySPKIPins)
	fields := strings.Split(raw, ",")
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
