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
	"time"
)

const (
	defaultAuthenticationMode                     = "local"
	defaultInvestigationWorkers                   = 8
	defaultInvestigationMaxPendingPerOrganization = 100
	defaultSessionLifetime                        = 12 * time.Hour
	minimumSessionLifetime                        = 5 * time.Minute
	maximumSessionLifetime                        = 30 * 24 * time.Hour
)

var SupportedEnvironmentKeys = []string{
	// ========= Required ENVs =========
	EnvDatabaseDSN,
	EnvDatabaseDSNFile,

	// ========= Optional ENVs =========
	EnvHTTPAddress,
	EnvOperatorPublicURL,
	EnvLogLevel,
	EnvOTLPEndpoint,

	EnvAuthenticationMode,
	EnvSessionLifetimeSeconds,
	EnvOperatorToken,
	EnvOperatorTokenFile,
	EnvSealingKey,
	EnvSealingKeyFile,
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
	// ========= Required ENVs =========
	EnvDatabaseDSN     = "OC_DATABASE_DSN"
	EnvDatabaseDSNFile = "OC_DATABASE_DSN_FILE"

	// ========= Optional ENVs =========
	EnvHTTPAddress       = "OC_SERVER_ADDRESS"
	EnvOperatorPublicURL = "OC_PUBLIC_URL"
	EnvLogLevel          = "OC_LOG_LEVEL"
	EnvOTLPEndpoint      = "OC_OTLP_ENDPOINT"

	EnvAuthenticationMode     = "OC_AUTH_MODE"
	EnvSessionLifetimeSeconds = "OC_SESSION_LIFETIME_SECONDS"
	EnvOperatorToken          = "OC_BOOTSTRAP_TOKEN"
	EnvOperatorTokenFile      = "OC_BOOTSTRAP_TOKEN_FILE"
	EnvSealingKey             = "OC_ENCRYPTION_KEY"
	EnvSealingKeyFile         = "OC_ENCRYPTION_KEY_FILE"
	EnvOIDCIssuer             = "OC_OIDC_ISSUER"
	EnvOIDCClientID           = "OC_OIDC_CLIENT_ID"
	EnvOIDCClientSecret       = "OC_OIDC_CLIENT_SECRET"
	EnvOIDCClientSecretFile   = "OC_OIDC_CLIENT_SECRET_FILE"

	EnvRelayAddress  = "OC_RELAY_ADDRESS"
	EnvRelaySPKIPins = "OC_RELAY_SPKI_PINS"

	EnvModelProvider                          = "OC_AI_PROVIDER"
	EnvModelName                              = "OC_AI_MODEL"
	EnvModelKey                               = "OC_AI_API_KEY"
	EnvModelKeyFile                           = "OC_AI_API_KEY_FILE"
	EnvModelContextWindowSize                 = "OC_AI_CONTEXT_WINDOW_SIZE"
	EnvModelMaxOutputTokens                   = "OC_AI_MAX_OUTPUT_SIZE"
	EnvInvestigationWorkers                   = "OC_INVESTIGATION_WORKERS"
	EnvInvestigationMaxPendingPerOrganization = "OC_MAX_PENDING_INVESTIGATIONS_PER_ORGANIZATION"

	EnvSlackClientID          = "OC_SLACK_CLIENT_ID"
	EnvSlackClientSecret      = "OC_SLACK_CLIENT_SECRET"
	EnvSlackClientSecretFile  = "OC_SLACK_CLIENT_SECRET_FILE"
	EnvSlackSigningSecret     = "OC_SLACK_SIGNING_SECRET"
	EnvSlackSigningSecretFile = "OC_SLACK_SIGNING_SECRET_FILE"

	EnvGitHubAppID      = "OC_GITHUB_APP_ID"
	EnvGitHubAppKey     = "OC_GITHUB_APP_PRIVATE_KEY"
	EnvGitHubAppKeyFile = "OC_GITHUB_APP_PRIVATE_KEY_FILE"
)

// Config is the validated process configuration.
type Config struct {
	// ========= Server =========
	LogLevel          slog.Level
	HTTPAddress       string
	OperatorPublicURL string
	DatabaseDSN       string
	OTLPEndpoint      string

	// ========= Authentication =========
	AuthenticationMode string
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	SessionLifetime    time.Duration
	// Only the digest is retained; the bootstrap token is discarded after loading.
	OperatorTokenDigest []byte
	// SealingKey encrypts credentials that must be presented again.
	SealingKey []byte

	// ========= Relay =========
	RelayAddress  string
	RelaySPKIPins []string

	// ========= Integrations =========
	SlackClientID      string
	SlackClientSecret  string
	SlackSigningSecret string
	GitHubAppID        string
	GitHubAppKey       []byte

	// ========= Model runtime =========
	ModelProvider            string
	ModelName                string
	ModelKey                 string
	ModelContextWindowTokens int
	ModelMaxOutputTokens     int64

	// ========= Investigation =========
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
		SessionLifetime:                         defaultSessionLifetime,
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
		return Config{}, fmt.Errorf("%s or %s is required", EnvDatabaseDSN, EnvDatabaseDSNFile)
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
	if err = modelConfiguration(lookup, &cfg); err != nil {
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
	var sessionLifetimeSeconds int
	if sessionLifetimeSeconds, err = boundedInteger(lookup, EnvSessionLifetimeSeconds,
		int(cfg.SessionLifetime.Seconds()), int(minimumSessionLifetime.Seconds()),
		int(maximumSessionLifetime.Seconds())); err != nil {
		return Config{}, err
	}
	cfg.SessionLifetime = time.Duration(sessionLifetimeSeconds) * time.Second

	return cfg, nil
}

func boundedInteger(
	lookup func(string) (string, bool), key string, fallback, minimum, maximum int,
) (int, error) {
	value, err := positiveInteger(lookup, key, fallback)
	if err != nil {
		return 0, err
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", key, minimum, maximum)
	}
	return value, nil
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
		return fmt.Errorf("%s, %s, and either %s or %s are required in local+oidc mode",
			EnvOIDCIssuer, EnvOIDCClientID, EnvOIDCClientSecret, EnvOIDCClientSecretFile)
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

func modelConfiguration(lookup func(string) (string, bool), cfg *Config) error {
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
		return fmt.Errorf("%s or %s is required when %s is set",
			EnvModelKey, EnvModelKeyFile, EnvModelProvider)
	}
	cfg.ModelKey = key
	return nil
}

func gitHubApp(lookup func(string) (string, bool)) (string, []byte, error) {
	id, _ := lookup(EnvGitHubAppID)
	id = strings.TrimSpace(id)
	key, err := readSecret(lookup, EnvGitHubAppKeyFile)
	if err != nil {
		return "", nil, err
	}
	if (id == "") != (len(key) == 0) {
		return "", nil, fmt.Errorf("%s and either %s or %s must be configured together",
			EnvGitHubAppID, EnvGitHubAppKey, EnvGitHubAppKeyFile)
	}
	return id, key, nil
}

func slackApp(lookup func(string) (string, bool), cfg *Config) error {
	clientID, _ := lookup(EnvSlackClientID)
	clientID = strings.TrimSpace(clientID)
	secret, err := readSecretText(lookup, EnvSlackClientSecretFile)
	if err != nil {
		return err
	}
	if (clientID == "") != (secret == "") {
		return fmt.Errorf("%s and either %s or %s must be configured together",
			EnvSlackClientID, EnvSlackClientSecret, EnvSlackClientSecretFile)
	}
	signing, err := readSecretText(lookup, EnvSlackSigningSecretFile)
	if err != nil {
		return err
	}
	cfg.SlackClientID, cfg.SlackClientSecret, cfg.SlackSigningSecret = clientID, secret, signing
	return nil
}

// Relay pins are required when the Relay listener is enabled.
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
