package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type fileConfiguration struct {
	Server struct {
		Address   *string `yaml:"address"`
		PublicURL *string `yaml:"publicURL"`
	} `yaml:"server"`
	Telemetry struct {
		LogLevel     *string `yaml:"logLevel"`
		OTLPEndpoint *string `yaml:"otlpEndpoint"`
	} `yaml:"telemetry"`
	Database struct {
		DSNFile *string `yaml:"dsnFile"`
	} `yaml:"database"`
	Session struct {
		Lifetime *durationValue `yaml:"lifetime"`
	} `yaml:"session"`
	Relay struct {
		Address  *string   `yaml:"address"`
		SPKIPins *[]string `yaml:"spkiPins"`
	} `yaml:"relay"`
	AI struct {
		Provider        *string `yaml:"provider"`
		Model           *string `yaml:"model"`
		APIKeyFile      *string `yaml:"apiKeyFile"`
		ContextWindow   *int    `yaml:"contextWindow"`
		MaxOutputTokens *int64  `yaml:"maxOutputTokens"`
	} `yaml:"ai"`
	Investigations struct {
		Workers                   *int `yaml:"workers"`
		MaxPendingPerOrganization *int `yaml:"maxPendingPerOrganization"`
	} `yaml:"investigations"`
	OIDC struct {
		Issuer           *string `yaml:"issuer"`
		ClientID         *string `yaml:"clientID"`
		ClientSecretFile *string `yaml:"clientSecretFile"`
	} `yaml:"oidc"`
	Slack struct {
		ClientID          *string `yaml:"clientID"`
		ClientSecretFile  *string `yaml:"clientSecretFile"`
		SigningSecretFile *string `yaml:"signingSecretFile"`
	} `yaml:"slack"`
	GitHub struct {
		AppID          *string `yaml:"appID"`
		PrivateKeyFile *string `yaml:"privateKeyFile"`
	} `yaml:"github"`
	Security struct {
		EncryptionKeyFile  *string `yaml:"encryptionKeyFile"`
		BootstrapTokenFile *string `yaml:"bootstrapTokenFile"`
	} `yaml:"security"`
}

type durationValue time.Duration

func (value *durationValue) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return errors.New("must be a duration such as 12h")
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return errors.New("must be a duration such as 12h")
	}
	*value = durationValue(parsed)
	return nil
}

func effectiveLookup(lookup func(string) (string, bool)) (func(string) (string, bool), error) {
	path, _ := lookup(EnvConfigFile)
	path = strings.TrimSpace(path)
	if path == "" {
		return lookup, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: configuration file cannot be read", EnvConfigFile)
	}
	defer func() { _ = file.Close() }()

	var document fileConfiguration
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err = decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("%s: invalid YAML: %w", EnvConfigFile, err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: exactly one YAML document is required", EnvConfigFile)
	}

	values := document.environment(filepath.Dir(path))
	return func(key string) (string, bool) {
		if value, present := lookup(key); present {
			return value, true
		}
		if strings.HasSuffix(key, "_FILE") {
			if _, direct := lookup(strings.TrimSuffix(key, "_FILE")); direct {
				return "", false
			}
		}
		value, present := values[key]
		return value, present
	}, nil
}

func (document fileConfiguration) environment(directory string) map[string]string {
	values := make(map[string]string)
	set := func(key string, value *string) {
		if value != nil {
			values[key] = *value
		}
	}
	secretFile := func(key string, value *string) {
		if value == nil {
			return
		}
		path := strings.TrimSpace(*value)
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(directory, path)
		}
		values[key] = path
	}
	integer := func(key string, value *int) {
		if value != nil {
			values[key] = strconv.Itoa(*value)
		}
	}

	set(EnvHTTPAddress, document.Server.Address)
	set(EnvPublicURL, document.Server.PublicURL)
	set(EnvLogLevel, document.Telemetry.LogLevel)
	set(EnvOTLPEndpoint, document.Telemetry.OTLPEndpoint)
	secretFile(EnvDatabaseDSNFile, document.Database.DSNFile)
	if document.Session.Lifetime != nil {
		values[EnvSessionLifetimeSeconds] = strconv.FormatInt(
			int64(time.Duration(*document.Session.Lifetime)/time.Second), 10)
	}
	set(EnvRelayAddress, document.Relay.Address)
	if document.Relay.SPKIPins != nil {
		values[EnvRelaySPKIPins] = strings.Join(*document.Relay.SPKIPins, ",")
	}
	set(EnvModelProvider, document.AI.Provider)
	set(EnvModelName, document.AI.Model)
	secretFile(EnvModelKeyFile, document.AI.APIKeyFile)
	integer(EnvModelContextWindowSize, document.AI.ContextWindow)
	if document.AI.MaxOutputTokens != nil {
		values[EnvModelMaxOutputTokens] = strconv.FormatInt(*document.AI.MaxOutputTokens, 10)
	}
	integer(EnvInvestigationWorkers, document.Investigations.Workers)
	integer(EnvInvestigationMaxPendingPerOrganization,
		document.Investigations.MaxPendingPerOrganization)
	set(EnvOIDCIssuer, document.OIDC.Issuer)
	set(EnvOIDCClientID, document.OIDC.ClientID)
	secretFile(EnvOIDCClientSecretFile, document.OIDC.ClientSecretFile)
	set(EnvSlackClientID, document.Slack.ClientID)
	secretFile(EnvSlackClientSecretFile, document.Slack.ClientSecretFile)
	secretFile(EnvSlackSigningSecretFile, document.Slack.SigningSecretFile)
	set(EnvGitHubAppID, document.GitHub.AppID)
	secretFile(EnvGitHubAppKeyFile, document.GitHub.PrivateKeyFile)
	secretFile(EnvSealingKeyFile, document.Security.EncryptionKeyFile)
	secretFile(EnvBootstrapTokenFile, document.Security.BootstrapTokenFile)
	return values
}
