package config

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const EnvConfigFile = "OC_CONFIG_FILE"

func LoadProcess(args []string, lookup func(string) (string, bool)) (Config, error) {
	path, _ := lookup(EnvConfigFile)
	flags := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&path, "config", strings.TrimSpace(path), "path to the YAML configuration file")
	serverAddress := flags.String("server-address", "", "local server listen address")
	if err := flags.Parse(args); err != nil {
		return Config{}, fmt.Errorf("command line: %w", err)
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("command line: unexpected argument %q", flags.Arg(0))
	}
	fileValues, err := loadFile(strings.TrimSpace(path))
	if err != nil {
		return Config{}, err
	}
	return Load(func(key string) (string, bool) {
		if key == EnvHTTPAddress && strings.TrimSpace(*serverAddress) != "" {
			return *serverAddress, true
		}
		if value, ok := lookup(key); ok {
			return value, true
		}
		value, ok := fileValues[key]
		return value, ok
	})
}

type fileDocument struct {
	Server         fileServer         `yaml:"server"`
	Database       fileDatabase       `yaml:"database"`
	Authentication fileAuthentication `yaml:"authentication"`
	Relay          fileRelay          `yaml:"relay"`
	AI             fileAI             `yaml:"ai"`
	Telemetry      fileTelemetry      `yaml:"telemetry"`
	Slack          fileSlack          `yaml:"slack"`
	GitHub         fileGitHub         `yaml:"github"`
}
type fileServer struct {
	Address   string `yaml:"address"`
	PublicURL string `yaml:"public_url"`
}
type fileDatabase struct {
	DSNFile string `yaml:"dsn_file"`
}
type fileAuthentication struct {
	Mode               string   `yaml:"mode"`
	BootstrapTokenFile string   `yaml:"bootstrap_token_file"`
	EncryptionKeyFile  string   `yaml:"encryption_key_file"`
	OIDC               fileOIDC `yaml:"oidc"`
}
type fileOIDC struct {
	Issuer           string `yaml:"issuer"`
	ClientID         string `yaml:"client_id"`
	ClientSecretFile string `yaml:"client_secret_file"`
}
type fileRelay struct {
	Address  string   `yaml:"address"`
	SPKIPins []string `yaml:"spki_pins"`
}
type fileAI struct {
	Provider   string `yaml:"provider"`
	Model      string `yaml:"model"`
	APIKeyFile string `yaml:"api_key_file"`
}
type fileTelemetry struct {
	LogLevel     string `yaml:"log_level"`
	OTLPEndpoint string `yaml:"otlp_endpoint"`
}
type fileSlack struct {
	ClientID          string `yaml:"client_id"`
	ClientSecretFile  string `yaml:"client_secret_file"`
	SigningSecretFile string `yaml:"signing_secret_file"`
}
type fileGitHub struct {
	AppID             string `yaml:"app_id"`
	AppPrivateKeyFile string `yaml:"app_private_key_file"`
}

func loadFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: read configuration: %w", EnvConfigFile, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var document fileDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("%s: decode YAML: %w", EnvConfigFile, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("%s: decode YAML: %w", EnvConfigFile, err)
		}
		return nil, fmt.Errorf("%s: configuration must contain exactly one YAML document", EnvConfigFile)
	}
	return document.environment(), nil
}

func (d fileDocument) environment() map[string]string {
	values := map[string]string{}
	set(values, EnvHTTPAddress, d.Server.Address)
	set(values, EnvOperatorPublicURL, d.Server.PublicURL)
	set(values, EnvDatabaseDSNFile, d.Database.DSNFile)
	set(values, EnvAuthenticationMode, d.Authentication.Mode)
	set(values, EnvOperatorTokenFile, d.Authentication.BootstrapTokenFile)
	set(values, EnvSealingKeyFile, d.Authentication.EncryptionKeyFile)
	set(values, EnvOIDCIssuer, d.Authentication.OIDC.Issuer)
	set(values, EnvOIDCClientID, d.Authentication.OIDC.ClientID)
	set(values, EnvOIDCClientSecretFile, d.Authentication.OIDC.ClientSecretFile)
	set(values, EnvRelayAddress, d.Relay.Address)
	if len(d.Relay.SPKIPins) > 0 {
		values[EnvRelaySPKIPins] = encodeList(d.Relay.SPKIPins)
	}
	set(values, EnvModelProvider, d.AI.Provider)
	set(values, EnvModelName, d.AI.Model)
	set(values, EnvModelKeyFile, d.AI.APIKeyFile)
	set(values, EnvLogLevel, d.Telemetry.LogLevel)
	set(values, EnvOTLPEndpoint, d.Telemetry.OTLPEndpoint)
	set(values, EnvSlackClientID, d.Slack.ClientID)
	set(values, EnvSlackClientSecretFile, d.Slack.ClientSecretFile)
	set(values, EnvSlackSigningSecretFile, d.Slack.SigningSecretFile)
	set(values, EnvGitHubAppID, d.GitHub.AppID)
	set(values, EnvGitHubAppKeyFile, d.GitHub.AppPrivateKeyFile)
	return values
}
func set(values map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		values[key] = value
	}
}
