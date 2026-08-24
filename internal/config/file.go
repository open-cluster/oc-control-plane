package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvConfigFile names the optional YAML configuration file. The command-line flag may
// override its location; values in the process environment override values inside it.
const EnvConfigFile = "OC_CONFIG_FILE"

// LoadProcess reads the process's configuration sources in precedence order: YAML,
// environment, then the deliberately small local-development command-line surface.
func LoadProcess(args []string, lookup func(string) (string, bool)) (Config, error) {
	path, _ := lookup(EnvConfigFile)
	path = strings.TrimSpace(path)

	flags := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&path, "config", path, "path to the YAML configuration file")
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
	overrides := map[string]string{}
	if strings.TrimSpace(*serverAddress) != "" {
		overrides[EnvHTTPAddress] = *serverAddress
	}

	return Load(func(key string) (string, bool) {
		if value, ok := overrides[key]; ok {
			return value, true
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
	Telemetry      fileTelemetry      `yaml:"telemetry"`
	Providers      fileProviders      `yaml:"providers"`
	Model          fileModel          `yaml:"model"`
	Investigations fileInvestigations `yaml:"investigations"`
	Conversations  fileConversations  `yaml:"conversations"`
	ChangeLedger   fileChangeLedger   `yaml:"change_ledger"`
}

type fileServer struct {
	Address         string   `yaml:"address"`
	OperatorAddress string   `yaml:"operator_address"`
	IntakeAddress   string   `yaml:"intake_address"`
	PublicURL       string   `yaml:"public_url"`
	ConsoleURL      string   `yaml:"console_url"`
	IntakePublicURL string   `yaml:"intake_public_url"`
	AllowedOrigins  []string `yaml:"allowed_origins"`
	ShutdownTimeout string   `yaml:"shutdown_timeout"`
}

type fileDatabase struct {
	DSNFile          string                   `yaml:"dsn_file"`
	Placements       map[string]filePlacement `yaml:"placements"`
	Assignments      map[string]string        `yaml:"assignments"`
	DefaultPlacement string                   `yaml:"default_placement"`
}

type filePlacement struct {
	DSNFile string `yaml:"dsn_file"`
}

type fileAuthentication struct {
	BootstrapTokenFile    string `yaml:"bootstrap_token_file"`
	BootstrapOrganization string `yaml:"bootstrap_organization"`
	BootstrapRole         string `yaml:"bootstrap_role"`
	SealingKeyFile        string `yaml:"sealing_key_file"`
}

type fileRelay struct {
	Address           string   `yaml:"address"`
	SPKIPins          []string `yaml:"spki_pins"`
	InventoryInterval string   `yaml:"inventory_interval"`
	MinimumVersion    string   `yaml:"minimum_version"`
}

type fileTelemetry struct {
	ServiceName  string `yaml:"service_name"`
	OTLPEndpoint string `yaml:"otlp_endpoint"`
}

type fileProviders struct {
	Slack  fileSlack  `yaml:"slack"`
	GitHub fileGitHub `yaml:"github"`
}

type fileSlack struct {
	APIURL             string   `yaml:"api_url"`
	ClientID           string   `yaml:"client_id"`
	ClientSecretFile   string   `yaml:"client_secret_file"`
	SigningSecretFile  string   `yaml:"signing_secret_file"`
	AgentOrganizations []string `yaml:"agent_organizations"`
}

type fileGitHub struct {
	AppID             string `yaml:"app_id"`
	AppPrivateKeyFile string `yaml:"app_private_key_file"`
	APIURL            string `yaml:"api_url"`
	AppSlug           string `yaml:"app_slug"`
	ClientID          string `yaml:"client_id"`
	ClientSecretFile  string `yaml:"client_secret_file"`
	WebURL            string `yaml:"web_url"`
}

type fileModel struct {
	Provider           string   `yaml:"provider"`
	Name               string   `yaml:"name"`
	APIKeyFile         string   `yaml:"api_key_file"`
	Effort             string   `yaml:"effort"`
	ConsentedProviders []string `yaml:"consented_providers"`
	BaseURL            string   `yaml:"base_url"`
	SpendCeilingCents  *int     `yaml:"spend_ceiling_cents"`
	ContextWindow      *int     `yaml:"context_window"`
}

type fileInvestigations struct {
	WindowLead  string `yaml:"window_lead"`
	MaxToolRuns *int   `yaml:"max_tool_runs"`
	MaxTurns    *int   `yaml:"max_turns"`
}

type fileConversations struct {
	Enabled                     *bool `yaml:"enabled"`
	MaxConcurrentInvestigations *int  `yaml:"max_concurrent_investigations"`
	MaxWaitingInvestigations    *int  `yaml:"max_waiting_investigations"`
	ContextThresholdPercent     *int  `yaml:"context_threshold_percent"`
}

type fileChangeLedger struct {
	RetentionDays *int `yaml:"retention_days"`
}

func loadFile(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	opened, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: configuration file could not be opened", EnvConfigFile)
	}
	defer func() { _ = opened.Close() }()

	var document fileDocument
	decoder := yaml.NewDecoder(opened)
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("%s: invalid YAML: %w", EnvConfigFile, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("%s: invalid YAML: %w", EnvConfigFile, err)
		}
		return nil, fmt.Errorf("%s must contain exactly one YAML document", EnvConfigFile)
	}
	return document.environment(), nil
}

func (document fileDocument) environment() map[string]string {
	values := map[string]string{}
	set(values, EnvHTTPAddress, document.Server.Address)
	set(values, EnvOperatorAddress, document.Server.OperatorAddress)
	set(values, EnvIntakeAddress, document.Server.IntakeAddress)
	set(values, EnvOperatorPublicURL, document.Server.PublicURL)
	set(values, EnvOperatorConsoleURL, document.Server.ConsoleURL)
	set(values, EnvIntakePublicURL, document.Server.IntakePublicURL)
	setList(values, EnvOperatorAllowedOrigins, document.Server.AllowedOrigins)
	set(values, EnvShutdownTimeout, document.Server.ShutdownTimeout)
	set(values, EnvServiceName, document.Telemetry.ServiceName)
	set(values, EnvOTLPEndpoint, document.Telemetry.OTLPEndpoint)
	set(values, EnvDefaultPlacement, document.Database.DefaultPlacement)
	set(values, EnvDatabaseDSNFile, document.Database.DSNFile)
	set(values, EnvOperatorTokenFile, document.Authentication.BootstrapTokenFile)
	set(values, EnvOperatorTokenOrganization, document.Authentication.BootstrapOrganization)
	set(values, EnvOperatorTokenRole, document.Authentication.BootstrapRole)
	set(values, EnvSealingKeyFile, document.Authentication.SealingKeyFile)
	set(values, EnvRelayAddress, document.Relay.Address)
	setList(values, EnvRelaySPKIPins, document.Relay.SPKIPins)
	set(values, EnvInventoryInterval, document.Relay.InventoryInterval)
	set(values, EnvMinimumRelayVersion, document.Relay.MinimumVersion)
	set(values, EnvSlackAPIURL, document.Providers.Slack.APIURL)
	set(values, EnvSlackClientID, document.Providers.Slack.ClientID)
	set(values, EnvSlackClientSecretFile, document.Providers.Slack.ClientSecretFile)
	set(values, EnvSlackSigningSecretFile, document.Providers.Slack.SigningSecretFile)
	setList(values, EnvSlackAgentOrganizations, document.Providers.Slack.AgentOrganizations)
	set(values, EnvGitHubAppID, document.Providers.GitHub.AppID)
	set(values, EnvGitHubAppKeyFile, document.Providers.GitHub.AppPrivateKeyFile)
	set(values, EnvGitHubAPIURL, document.Providers.GitHub.APIURL)
	set(values, EnvGitHubAppSlug, document.Providers.GitHub.AppSlug)
	set(values, EnvGitHubClientID, document.Providers.GitHub.ClientID)
	set(values, EnvGitHubClientSecretFile, document.Providers.GitHub.ClientSecretFile)
	set(values, EnvGitHubWebURL, document.Providers.GitHub.WebURL)
	set(values, EnvModelProvider, document.Model.Provider)
	set(values, EnvModelName, document.Model.Name)
	set(values, EnvModelKeyFile, document.Model.APIKeyFile)
	set(values, EnvModelEffort, document.Model.Effort)
	setList(values, EnvModelConsented, document.Model.ConsentedProviders)
	set(values, EnvModelBaseURL, document.Model.BaseURL)
	setInt(values, EnvModelSpendCeiling, document.Model.SpendCeilingCents)
	setInt(values, EnvModelContextWindow, document.Model.ContextWindow)
	set(values, EnvInvestigationWindowLead, document.Investigations.WindowLead)
	setInt(values, EnvInvestigationMaxToolRuns, document.Investigations.MaxToolRuns)
	setInt(values, EnvInvestigationMaxTurns, document.Investigations.MaxTurns)
	setBool(values, EnvConversationsEnabled, document.Conversations.Enabled)
	setInt(values, EnvOrgConcurrentInvestigations,
		document.Conversations.MaxConcurrentInvestigations)
	setInt(values, EnvOrgWaitingInvestigations,
		document.Conversations.MaxWaitingInvestigations)
	setInt(values, EnvContextThresholdPercent,
		document.Conversations.ContextThresholdPercent)
	setInt(values, EnvChangeLedgerRetention, document.ChangeLedger.RetentionDays)

	names := make([]string, 0, len(document.Database.Placements))
	for name := range document.Database.Placements {
		names = append(names, name)
	}
	sort.Strings(names)
	placements := make([]string, 0, len(names))
	for _, name := range names {
		placements = append(placements, name+"="+document.Database.Placements[name].DSNFile)
	}
	set(values, EnvPlacements, encodeList(placements))

	organizations := make([]string, 0, len(document.Database.Assignments))
	for organization := range document.Database.Assignments {
		organizations = append(organizations, organization)
	}
	sort.Strings(organizations)
	assignments := make([]string, 0, len(organizations))
	for _, organization := range organizations {
		assignments = append(assignments,
			organization+"="+document.Database.Assignments[organization])
	}
	set(values, EnvAssignments, encodeList(assignments))
	return values
}

func set(values map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		values[key] = value
	}
}

func setList(values map[string]string, key string, entries []string) {
	if len(entries) != 0 {
		values[key] = encodeList(entries)
	}
}

func setInt(values map[string]string, key string, value *int) {
	if value != nil {
		values[key] = strconv.Itoa(*value)
	}
}

func setBool(values map[string]string, key string, value *bool) {
	if value != nil {
		values[key] = strconv.FormatBool(*value)
	}
}
