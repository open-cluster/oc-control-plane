// Package config loads and validates the control plane's process configuration from the
// environment. Configuration is non-secret with one deliberate exception: a placement's
// connection string carries a password, so placements name a FILE holding the DSN rather
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
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults for the optional settings.
const (
	defaultShutdownTimeout = 15 * time.Second
	defaultServiceName     = "oc-control-plane"
	// Five minutes places a change within an investigation window without asking a
	// customer's cluster to answer lists it would notice.
	defaultInventoryInterval = 5 * time.Minute
	// Ninety days of change history covers any window an investigation would be scoped
	// to, and is deliberately independent of evidence and audit retention.
	defaultChangeLedgerRetentionDays = 90
	// Two hours before the incident began: the change that caused an incident usually
	// landed before it fired. An evaluation-derived default, not a constant — operators
	// whose deploy cadence differs configure it.
	defaultInvestigationWindowLead = 2 * time.Hour
	// Five dollars per investigation is several times what a legitimate investigation
	// spends; the ceiling is a backstop against a runaway, not a steering wheel.
	// Re-derived from the evaluation suite's measured distributions.
	defaultModelSpendCeilingCents = 500
	// A tenant may hold four investigations at once and queue sixteen more. The first
	// number is what "one tenant cannot consume the whole deployment" means; the second
	// is what keeps overload boring — work above it is refused with a plain reason rather
	// than accumulating until something falls over.
	defaultOrgConcurrentInvestigations = 4
	defaultOrgWaitingInvestigations    = 16
	// Half the model's working budget before older turns are compacted. A soft threshold
	// rather than the ceiling itself: compacting at the edge means the very turn that
	// triggered it has no room to run.
	defaultContextThresholdPercent = 50
)

// Environment variable names, listed once so errors and documentation cannot drift.
const (
	EnvHTTPAddress      = "OC_HTTP_ADDRESS"
	EnvPlacements       = "OC_PLACEMENTS"
	EnvAssignments      = "OC_PLACEMENT_ASSIGNMENTS"
	EnvDefaultPlacement = "OC_DEFAULT_PLACEMENT"
	EnvShutdownTimeout  = "OC_SHUTDOWN_TIMEOUT"
	EnvServiceName      = "OC_SERVICE_NAME"
	EnvOTLPEndpoint     = "OC_OTLP_ENDPOINT"
	EnvRelayAddress     = "OC_RELAY_ADDRESS"
	EnvRelaySPKIPins    = "OC_RELAY_SPKI_PINS"

	EnvOperatorAddress   = "OC_OPERATOR_ADDRESS"
	EnvOperatorTokenFile = "OC_OPERATOR_TOKEN_FILE"
	// The bootstrap credential is no longer ambient root. It names ONE organization and ONE
	// role, so a deployment that hands it to CI has handed out something with a stated blast
	// radius rather than the whole estate.
	EnvOperatorTokenOrganization = "OC_OPERATOR_TOKEN_ORGANIZATION"
	EnvOperatorTokenRole         = "OC_OPERATOR_TOKEN_ROLE"

	// Where this surface and its console are reachable from a browser. Both are configuration
	// rather than values read from a request: a caller-controlled host in a redirect URI is how
	// an authorization code is delivered somewhere else.
	EnvOperatorPublicURL  = "OC_OPERATOR_PUBLIC_URL"
	EnvOperatorConsoleURL = "OC_OPERATOR_CONSOLE_URL"
	// The browser origins a cookie-authenticated unsafe request may come from. SameSite=Lax plus
	// this check is the CSRF defence; there is no separate token. Empty permits no browser to
	// make an unsafe request, which is the right posture for a deployment that has not said
	// where its console is.
	EnvOperatorAllowedOrigins = "OC_OPERATOR_ALLOWED_ORIGINS"

	// The key every presentable credential is sealed under: an identity provider's client
	// secret, an integration's bot token. Those are the credentials this product must be
	// able to READ BACK — they are presented to the far end rather than compared against —
	// so they are encrypted rather than digested, and this names the file the key is read
	// from.
	EnvSealingKeyFile = "OC_SEALING_KEY_FILE"

	// EnvSlackAPIURL overrides where the Slack provider reaches its vendor. It exists for
	// tests and for API-compatible proxies; empty means Slack's own origin.
	EnvSlackAPIURL = "OC_SLACK_API_URL"
	// The OpenCluster Slack app is DEPLOYMENT-level configuration, exactly as the GitHub
	// App is: one app, installed by many customers. The variables name FILES for the two
	// secrets, because no environment value in this product ever carries a credential.
	//
	// The client credential and the signing secret are INDEPENDENT. The first serves the
	// one-click connect flow; the second serves the events endpoint. A deployment may
	// hold either without the other — a self-hosted install that pasted a token can still
	// receive events, and a deployment mid-registration may have a client before it has
	// published a request URL.
	EnvSlackClientID          = "OC_SLACK_CLIENT_ID"
	EnvSlackClientSecretFile  = "OC_SLACK_CLIENT_SECRET_FILE"
	EnvSlackSigningSecretFile = "OC_SLACK_SIGNING_SECRET_FILE"
	// EnvSlackAgentOrganizations is the STAGED ROLLOUT gate for the Slack agent surface:
	// a comma-separated list of the organizations it is live for, empty or unset meaning
	// none.
	//
	// Deployment configuration on purpose, and not a tenant policy column. The two
	// readings of "ships behind a per-organization switch" differ by a migration and a
	// permanent customer-facing API field: a policy column would put an internal rollout
	// decision on a customer's settings page and leave it there as a vestigial setting
	// long after the rollout ended. When the surface is generally available this variable
	// and the code reading it are deleted, and nothing is left behind.
	//
	// It is also not the only switch and does not pretend to be. An organization that has
	// not connected Slack does not have Slack, and an integration an operator disabled
	// stops reading and stops answering. This is the one that says "we are not offering
	// this yet".
	EnvSlackAgentOrganizations = "OC_SLACK_AGENT_ORGANIZATIONS"

	// The GitHub App credential is DEPLOYMENT-level configuration: one app, installed by
	// customers onto their own accounts. The id is public; the private key names a file,
	// like every credential here.
	EnvGitHubAppID      = "OC_GITHUB_APP_ID"
	EnvGitHubAppKeyFile = "OC_GITHUB_APP_PRIVATE_KEY_FILE"
	EnvGitHubAPIURL     = "OC_GITHUB_API_URL"

	// What the one-click installation flow needs beyond the App credential: the App's own
	// URL slug, and the OAuth client the return trip is proven with. The three are set
	// together or not at all; without them the connect button is not offered and the
	// configuration form remains, which is the self-hosted path.
	EnvGitHubAppSlug          = "OC_GITHUB_APP_SLUG"
	EnvGitHubClientID         = "OC_GITHUB_APP_CLIENT_ID"
	EnvGitHubClientSecretFile = "OC_GITHUB_APP_CLIENT_SECRET_FILE"
	// EnvGitHubWebURL overrides GitHub's browser origin, where an installation is started
	// and where an authorization code is exchanged. It is separate from the API origin
	// because on GitHub Enterprise Server the two differ; empty means github.com.
	EnvGitHubWebURL = "OC_GITHUB_WEB_URL"

	// The model deployment investigations reason with: DEPLOYMENT-level settings, never a
	// per-tenant concern. The key names a file; consent lists the providers evidence may
	// be sent to, and nothing listed permits nothing.
	EnvModelProvider  = "OC_MODEL_PROVIDER"
	EnvModelName      = "OC_MODEL_NAME"
	EnvModelKeyFile   = "OC_MODEL_KEY_FILE"
	EnvModelEffort    = "OC_MODEL_EFFORT"
	EnvModelConsented = "OC_MODEL_CONSENTED_PROVIDERS"
	EnvModelBaseURL   = "OC_MODEL_BASE_URL"
	// EnvModelSpendCeiling is the hard spend ceiling per investigation, in whole US
	// cents. A reached ceiling forces an honest partial conclusion labeled stopped-by-
	// spend; it cannot be turned off, only raised.
	EnvModelSpendCeiling = "OC_MODEL_SPEND_CEILING_CENTS"

	// EnvInvestigationWindowLead widens an investigation's window backwards before the
	// incident began, so the change that caused it is inside the window every read is
	// clamped to.
	EnvInvestigationWindowLead = "OC_INVESTIGATION_WINDOW_LEAD"

	// EnvInvestigationMaxToolRuns and EnvInvestigationMaxTurns are the autonomous
	// loop's safety ceilings — evaluation-derived tuning, so they are configuration
	// rather than constants. Unset means the built-in defaults.
	EnvInvestigationMaxToolRuns = "OC_INVESTIGATION_MAX_TOOL_RUNS"
	EnvInvestigationMaxTurns    = "OC_INVESTIGATION_MAX_TURNS"

	// EnvConversationsEnabled is the per-deployment switch for the conversation surface.
	// It is the existing configuration mechanism doing the job of a feature flag, because
	// a flag platform is a system to operate and this is one boolean.
	EnvConversationsEnabled = "OC_CONVERSATIONS_ENABLED"
	// EnvOrgConcurrentInvestigations and EnvOrgWaitingInvestigations are the
	// per-organization ceilings: how many turns one tenant may have executing at once,
	// and how many may wait behind them.
	EnvOrgConcurrentInvestigations = "OC_ORG_MAX_CONCURRENT_INVESTIGATIONS"
	EnvOrgWaitingInvestigations    = "OC_ORG_MAX_WAITING_INVESTIGATIONS"
	// EnvModelContextWindow is the model's working context window, in tokens. Empty means
	// the per-model default table decides, which is what a deployment that has not
	// thought about it should get. Named for the WINDOW rather than the unit, because a
	// variable whose name ends in TOKENS reads as a credential to anything scanning for
	// one — including this repository's own gate.
	EnvModelContextWindow = "OC_MODEL_CONTEXT_WINDOW"
	// EnvContextThresholdPercent is how full the estimated context may get before older
	// turns are compacted into the running summary.
	EnvContextThresholdPercent = "OC_CONTEXT_THRESHOLD_PERCENT"

	EnvIntakeAddress = "OC_INTAKE_ADDRESS"
	// EnvIntakePublicURL is the origin a customer's own alerting reaches intake at. It is
	// configured rather than derived from a request, because the delivery endpoint built from it
	// is pasted into somebody else's system.
	EnvIntakePublicURL = "OC_INTAKE_PUBLIC_URL"
	// EnvInventoryInterval is the tick interval the control plane REQUESTS from every
	// Relay's inventory synchronization. Requests, not sets: the Relay floors it at its
	// own local minimum, so this can slow a fleet down and can never speed one up past
	// what its operators allow.
	EnvInventoryInterval = "OC_INVENTORY_INTERVAL"

	// EnvChangeLedgerRetention is how many days the change ledger keeps an entry. The
	// ledger is derived operational context on its own schedule, deliberately independent
	// of evidence and audit retention.
	EnvChangeLedgerRetention = "OC_CHANGE_LEDGER_RETENTION_DAYS"

	// EnvMinimumRelayVersion is the relay version floor the fleet summary counts `outdated`
	// against. Empty means nothing is compared, and the summary says so rather than reporting
	// zero outdated as though every relay were current.
	EnvMinimumRelayVersion = "OC_MINIMUM_RELAY_VERSION"
)

// Config is the validated process configuration.
type Config struct {
	// HTTPAddress is the listen address for health, readiness, and metrics.
	HTTPAddress string

	// Placements maps a placement name to its resolved connection string. The DSN is read
	// from the file the operator named; it is never carried in an environment value.
	Placements map[string]string

	// Assignments maps an organization to its placement name. An organization listed here
	// overrides the default, which is how the Business and Enterprise tiers put a tenant on
	// a dedicated database.
	Assignments map[string]string

	// DefaultPlacement serves organizations with no explicit assignment. It is the shared
	// tier: enumerating five thousand organizations in an environment variable, and
	// restarting every instance to onboard one, is not a deployment.
	//
	// It is OPTIONAL and, when set, is an explicit operator declaration — not an implicit
	// fallback. With no default configured an unassigned organization is a hard error,
	// because silently serving an unrecognised caller from someone else's connection is the
	// failure this design exists to prevent.
	DefaultPlacement string

	// ShutdownTimeout bounds the drain of in-flight requests on SIGTERM.
	ShutdownTimeout time.Duration

	// ServiceName identifies this process in telemetry.
	ServiceName string

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

	// OperatorAddress is the listen address for the operator surface, separate from the health
	// surface because it reads across tenants and belongs on an interface that health and
	// metrics do not. Empty disables it, which is correct for a deployment that has nowhere
	// private to put it.
	OperatorAddress string

	// OperatorTokenDigest is the SHA-256 of the bootstrap token. The token is read from the file
	// the operator named, reduced to this, and discarded: the process holds no copy of it, so
	// there is nothing here to log or echo by accident.
	//
	// It is what the old shared operator token became. The difference is the whole point: it is
	// bound to the organization and role below rather than reaching every tenant. Its limits are
	// worth stating rather than implying — it has no expiry and no revocation row, because it
	// exists to bootstrap a deployment that has no members yet, and revoking it means changing
	// the file and restarting. Every token issued after that comes from the api_token table,
	// where both exist.
	OperatorTokenDigest []byte

	// OperatorTokenOrganization is the one tenant the bootstrap credential reaches.
	OperatorTokenOrganization string

	// OperatorTokenRole is the one role it holds there. It defaults to admin, because a
	// deployment with no members yet needs a credential that can create the first one.
	OperatorTokenRole string

	// OperatorPublicURL is where this surface is reachable from a browser, and what the redirect
	// URI registered with an identity provider is built from.
	OperatorPublicURL string

	// OperatorConsoleURL is where a browser is sent once it has signed in.
	//
	// It must share a registrable domain with OperatorPublicURL. That follows from the session
	// cookie being SameSite=Lax with no separate CSRF token: a cross-SITE console would never
	// send the cookie at all, so the deployment would authenticate nobody.
	OperatorConsoleURL string

	// OperatorAllowedOrigins are the browser origins a cookie-authenticated unsafe request may
	// come from.
	OperatorAllowedOrigins []string

	// SealingKey seals presentable credentials at rest: an identity provider's client
	// secret, an integration's outbound token. Empty means this deployment cannot hold
	// one, and submitting one is refused with that reason rather than stored in the clear.
	SealingKey []byte

	// SlackAPIURL is where the Slack provider reaches its vendor; empty means Slack's own
	// origin. It exists so a test can stand a fake where slack.com would be.
	SlackAPIURL string
	// SlackClientID and SlackClientSecret are the OpenCluster Slack app's OAuth client.
	// Both empty means this deployment offers no one-click Slack install and serves the
	// pasted-token form instead. SlackSigningSecret is what inbound events are verified
	// against; empty means the events endpoint is not served at all, and the integration
	// truthfully reports its inbound capabilities as unavailable.
	SlackClientID      string
	SlackClientSecret  string
	SlackSigningSecret string
	// SlackAgentOrganizations are the organizations the Slack agent surface is live for.
	// Empty means none, which is the default: an inbound event for an organization outside
	// it is acknowledged and dropped, and reads through the existing Slack tools are
	// untouched either way.
	SlackAgentOrganizations []string

	// GitHubAppID and GitHubAppKey are the deployment's GitHub App credential; both empty
	// means this deployment cannot reach GitHub, and connecting it is refused live with
	// that reason. GitHubAPIURL overrides the vendor origin, for tests and GitHub
	// Enterprise hosts.
	GitHubAppID  string
	GitHubAppKey []byte
	GitHubAPIURL string

	// GitHubAppSlug, GitHubClientID and GitHubClientSecret are what the one-click
	// installation flow needs: where to send a browser, and the OAuth client the return
	// trip is proven with. All empty means this deployment offers no installation flow
	// for GitHub and serves the configuration form instead. GitHubWebURL overrides
	// GitHub's browser origin, for tests and GitHub Enterprise hosts.
	GitHubAppSlug      string
	GitHubClientID     string
	GitHubClientSecret string
	GitHubWebURL       string

	// The model deployment. ModelProvider empty means this deployment cannot investigate,
	// and opening one is refused with that reason. The credential travels as a file's
	// contents, never as an environment value.
	ModelProvider  string
	ModelName      string
	ModelKey       string
	ModelEffort    string
	ModelConsented []string
	ModelBaseURL   string
	// ModelSpendCeilingCents is the hard spend ceiling per investigation, in whole US
	// cents. Always positive: the ceiling can be raised, never removed.
	ModelSpendCeilingCents int

	// InvestigationWindowLead is how far before the incident began an investigation's
	// window reaches back.
	InvestigationWindowLead time.Duration

	// ConversationsEnabled turns the conversation surface on. Off by default: the
	// single-shot investigation path is untouched either way, so a deployment that has
	// not opted in behaves exactly as it did.
	ConversationsEnabled bool
	// OrgConcurrentInvestigations and OrgWaitingInvestigations bound one organization's
	// executing and queued turns.
	OrgConcurrentInvestigations int
	OrgWaitingInvestigations    int
	// ModelContextWindow is the configured context window in tokens; zero means the
	// per-model default table decides. ContextThresholdPercent is how full the estimate
	// may get before compaction.
	ModelContextWindow      int
	ContextThresholdPercent int
	// InvestigationMaxToolRuns and InvestigationMaxTurns are the autonomous loop's
	// ceilings; zero means the built-in defaults.
	InvestigationMaxToolRuns int
	InvestigationMaxTurns    int

	// IntakeAddress is the listen address for alert intake. It is separate from every other
	// surface because it is the only one a customer's own infrastructure connects to inbound,
	// so a deployment can expose it and expose nothing else. Empty disables it, which is
	// correct for an instance that serves relays but takes no alerts.
	//
	// It carries no credential of its own: each configured source authenticates with its own
	// secret, so there is nothing here that would be shared across tenants.
	IntakeAddress string

	// IntakePublicURL is the public origin a customer's own system reaches intake at, for
	// example https://intake.opencluster.example. It is what an Integration's webhook
	// endpoint is built from.
	//
	// Empty is supported and means the endpoint is served as an absence rather than as a guess:
	// a URL assembled from the operator surface's own Host header would be one that works from
	// wherever the console is served and not from the customer's alerting, which is the one
	// place it has to work.
	IntakePublicURL string

	// InventoryInterval is the tick interval requested from every Relay's inventory
	// synchronization; each Relay floors it locally.
	InventoryInterval time.Duration

	// ChangeLedgerRetentionDays is how long a ledger entry is kept.
	ChangeLedgerRetentionDays int

	// MinimumRelayVersion is the relay version floor the fleet summary counts `outdated`
	// against. Empty means this deployment states no floor, in which case nothing is counted
	// outdated because nothing was compared — a different fact from every relay being current,
	// and one the summary reports rather than hides.
	MinimumRelayVersion string
}

// Load reads configuration through lookup (os.LookupEnv in production) and validates every
// value, failing on the first problem and naming the offending variable.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		ShutdownTimeout:           defaultShutdownTimeout,
		ServiceName:               defaultServiceName,
		InventoryInterval:         defaultInventoryInterval,
		ChangeLedgerRetentionDays: defaultChangeLedgerRetentionDays,
		InvestigationWindowLead:   defaultInvestigationWindowLead,
		ModelSpendCeilingCents:    defaultModelSpendCeilingCents,

		OrgConcurrentInvestigations: defaultOrgConcurrentInvestigations,
		OrgWaitingInvestigations:    defaultOrgWaitingInvestigations,
		ContextThresholdPercent:     defaultContextThresholdPercent,
	}

	var err error
	if cfg.HTTPAddress, err = requiredListenAddress(lookup, EnvHTTPAddress); err != nil {
		return Config{}, err
	}
	if cfg.Placements, err = placements(lookup); err != nil {
		return Config{}, err
	}
	if cfg.Assignments, err = assignments(lookup, cfg.Placements); err != nil {
		return Config{}, err
	}
	if cfg.DefaultPlacement, err = defaultPlacement(lookup, cfg.Placements); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = optionalDuration(lookup, EnvShutdownTimeout, cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.InventoryInterval, err = optionalDuration(
		lookup, EnvInventoryInterval, cfg.InventoryInterval); err != nil {
		return Config{}, err
	}
	if cfg.ChangeLedgerRetentionDays, err = optionalDays(
		lookup, EnvChangeLedgerRetention, cfg.ChangeLedgerRetentionDays); err != nil {
		return Config{}, err
	}
	if cfg.ServiceName, err = optionalName(lookup, EnvServiceName, cfg.ServiceName); err != nil {
		return Config{}, err
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
	if cfg.OperatorAddress, err = optionalHostPort(lookup, EnvOperatorAddress); err != nil {
		return Config{}, err
	}
	if cfg.OperatorTokenDigest, err = operatorTokenDigest(lookup, cfg.OperatorAddress); err != nil {
		return Config{}, err
	}
	if err = operatorCredentialScope(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.OperatorPublicURL, err = optionalBrowserURL(lookup, EnvOperatorPublicURL); err != nil {
		return Config{}, err
	}
	if cfg.OperatorConsoleURL, err = optionalBrowserURL(lookup, EnvOperatorConsoleURL); err != nil {
		return Config{}, err
	}
	if cfg.OperatorAllowedOrigins, err = allowedOrigins(lookup); err != nil {
		return Config{}, err
	}
	if cfg.SealingKey, err = sealingKey(lookup); err != nil {
		return Config{}, err
	}
	if cfg.IntakeAddress, err = optionalHostPort(lookup, EnvIntakeAddress); err != nil {
		return Config{}, err
	}
	if cfg.IntakePublicURL, err = optionalIntakeURL(lookup, EnvIntakePublicURL); err != nil {
		return Config{}, err
	}
	if cfg.SlackAPIURL, err = optionalVendorURL(lookup, EnvSlackAPIURL); err != nil {
		return Config{}, err
	}
	if cfg.GitHubAppID, cfg.GitHubAppKey, err = gitHubApp(lookup); err != nil {
		return Config{}, err
	}
	if cfg.GitHubAPIURL, err = optionalVendorURL(lookup, EnvGitHubAPIURL); err != nil {
		return Config{}, err
	}
	if err = gitHubInstallFlow(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if err = slackApp(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if err = slackAgentRollout(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.GitHubWebURL, err = optionalVendorURL(lookup, EnvGitHubWebURL); err != nil {
		return Config{}, err
	}
	if err = modelDeployment(lookup, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.InvestigationMaxToolRuns, err = optionalPositive(
		lookup, EnvInvestigationMaxToolRuns); err != nil {
		return Config{}, err
	}
	if cfg.InvestigationMaxTurns, err = optionalPositive(
		lookup, EnvInvestigationMaxTurns); err != nil {
		return Config{}, err
	}
	if cfg.InvestigationWindowLead, err = optionalDuration(
		lookup, EnvInvestigationWindowLead, cfg.InvestigationWindowLead); err != nil {
		return Config{}, err
	}
	if cfg.ConversationsEnabled, err = optionalFlag(
		lookup, EnvConversationsEnabled); err != nil {
		return Config{}, err
	}
	if cfg.OrgConcurrentInvestigations, err = optionalPositiveOr(
		lookup, EnvOrgConcurrentInvestigations,
		cfg.OrgConcurrentInvestigations); err != nil {
		return Config{}, err
	}
	if cfg.OrgWaitingInvestigations, err = optionalPositiveOr(
		lookup, EnvOrgWaitingInvestigations, cfg.OrgWaitingInvestigations); err != nil {
		return Config{}, err
	}
	if cfg.ModelContextWindow, err = optionalPositive(
		lookup, EnvModelContextWindow); err != nil {
		return Config{}, err
	}
	if cfg.ContextThresholdPercent, err = optionalPercent(
		lookup, EnvContextThresholdPercent, cfg.ContextThresholdPercent); err != nil {
		return Config{}, err
	}
	if cfg.ModelSpendCeilingCents, err = optionalCents(
		lookup, EnvModelSpendCeiling, cfg.ModelSpendCeilingCents); err != nil {
		return Config{}, err
	}
	minimumRelay, _ := lookup(EnvMinimumRelayVersion)
	cfg.MinimumRelayVersion = strings.TrimSpace(minimumRelay)

	// A deployment with neither explicit assignments nor a default could resolve no
	// organization at all, which is a misconfiguration rather than a strict posture.
	if len(cfg.Assignments) == 0 && cfg.DefaultPlacement == "" {
		return Config{}, fmt.Errorf(
			"%s or %s is required: with neither, no organization can be resolved",
			EnvAssignments, EnvDefaultPlacement)
	}
	return cfg, nil
}

// placements parses "name=/path/to/dsn" pairs and resolves each DSN from its file. The
// file's contents are trimmed and never appear in an error.
func placements(lookup func(string) (string, bool)) (map[string]string, error) {
	raw, err := required(lookup, EnvPlacements)
	if err != nil {
		return nil, err
	}

	resolved := make(map[string]string)
	entries, listErr := decodeList(raw)
	if listErr != nil {
		return nil, fmt.Errorf("%s: invalid list: %w", EnvPlacements, listErr)
	}
	for _, entry := range entries {
		name, path, found := strings.Cut(strings.TrimSpace(entry), "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !found || name == "" || path == "" {
			return nil, fmt.Errorf("%s: each entry must be name=path-to-dsn-file", EnvPlacements)
		}
		if _, duplicate := resolved[name]; duplicate {
			return nil, fmt.Errorf("%s: placement %q is defined more than once", EnvPlacements, name)
		}

		dsn, readErr := readDSN(path)
		if readErr != nil {
			return nil, fmt.Errorf("%s: placement %q: %w", EnvPlacements, name, readErr)
		}
		resolved[name] = dsn
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("%s: no usable placements", EnvPlacements)
	}
	return resolved, nil
}

// readDSN reads a placement's connection string from disk. Every error is classified
// rather than wrapped, because the underlying error can quote file contents — which for
// this file is a database password.
func readDSN(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("dsn file does not exist")
		}
		if errors.Is(err, os.ErrPermission) {
			return "", errors.New("dsn file is not readable")
		}
		return "", errors.New("dsn file could not be read")
	}
	dsn := strings.TrimSpace(string(raw))
	if dsn == "" {
		return "", errors.New("dsn file is empty")
	}
	return dsn, nil
}

// assignments parses "org=placement" pairs. Every named placement must exist, so a typo
// fails at startup rather than becoming an unresolvable organization at request time.
//
// The variable is optional: a deployment with a default placement puts every organization
// on it and names only the exceptions. A deployment with neither is refused by Load, since
// it could resolve nothing.
func assignments(lookup func(string) (string, bool), known map[string]string) (map[string]string, error) {
	raw, ok := lookup(EnvAssignments)
	if !ok || strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	raw = strings.TrimSpace(raw)

	resolved := make(map[string]string)
	entries, listErr := decodeList(raw)
	if listErr != nil {
		return nil, fmt.Errorf("%s: invalid list: %w", EnvAssignments, listErr)
	}
	for _, entry := range entries {
		organization, placement, found := strings.Cut(strings.TrimSpace(entry), "=")
		organization, placement = strings.TrimSpace(organization), strings.TrimSpace(placement)
		if !found || organization == "" || placement == "" {
			return nil, fmt.Errorf("%s: each entry must be organization=placement", EnvAssignments)
		}
		if _, ok := known[placement]; !ok {
			return nil, fmt.Errorf("%s: organization %q names unknown placement %q",
				EnvAssignments, organization, placement)
		}
		resolved[organization] = placement
	}
	return resolved, nil
}

// defaultPlacement resolves the optional shared-tier placement. Naming a placement that
// does not exist is refused here so the mistake is a startup failure rather than an
// unresolvable organization at request time.
func defaultPlacement(lookup func(string) (string, bool), known map[string]string) (string, error) {
	value, ok := lookup(EnvDefaultPlacement)
	if !ok || strings.TrimSpace(value) == "" {
		return "", nil
	}
	name := strings.TrimSpace(value)
	if _, defined := known[name]; !defined {
		return "", fmt.Errorf("%s names unknown placement %q", EnvDefaultPlacement, name)
	}
	return name, nil
}

// readSecretFile reads a credential from disk. Every error is classified rather than wrapped,
// because the underlying error can quote the file's contents — and for this file those
// contents are the secret.
func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("file does not exist")
		}
		if errors.Is(err, os.ErrPermission) {
			return "", errors.New("file is not readable")
		}
		return "", errors.New("file could not be read")
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", errors.New("file is empty")
	}
	return value, nil
}

// optionalDays reads a positive whole number of days, or the fallback when absent.
func optionalDays(lookup func(string) (string, bool), key string, fallback int) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive whole number of days", key)
	}
	return parsed, nil
}

// optionalPositive reads a positive whole number, or zero when absent — zero meaning
// the built-in default, so a ceiling cannot be configured off.
func optionalPositive(lookup func(string) (string, bool), key string) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive whole number", key)
	}
	return parsed, nil
}

// optionalFlag reads a boolean switch. Only the words Go itself accepts are accepted, and
// anything else is refused rather than read as false: a deployment that meant to turn
// something on and typed "yes" should learn that, not run with it off.
func optionalFlag(lookup func(string) (string, bool), key string) (bool, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return parsed, nil
}

// optionalPositiveOr is optionalPositive with a default that is not zero.
func optionalPositiveOr(
	lookup func(string) (string, bool), key string, fallback int,
) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive whole number", key)
	}
	return parsed, nil
}

// optionalPercent reads a threshold as whole percent, refusing anything outside 1-99. A
// hundred would compact at the ceiling, which is where there is no room left to compact.
func optionalPercent(
	lookup func(string) (string, bool), key string, fallback int,
) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 || parsed > 99 {
		return 0, fmt.Errorf("%s must be a whole percentage between 1 and 99", key)
	}
	return parsed, nil
}

// optionalCents refuses zero and negatives: a spend ceiling that is off would make a
// runaway investigation possible again, so it can only be raised, never removed.
func optionalCents(lookup func(string) (string, bool), key string, fallback int) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive whole number of cents", key)
	}
	return parsed, nil
}

func required(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return strings.TrimSpace(value), nil
}

func requiredListenAddress(lookup func(string) (string, bool), key string) (string, error) {
	value, err := required(lookup, key)
	if err != nil {
		return "", err
	}
	if err := validateHostPort(value); err != nil {
		return "", fmt.Errorf("%s must be host:port: %w", key, err)
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
	effort, _ := lookup(EnvModelEffort)
	cfg.ModelEffort = strings.TrimSpace(effort)

	consented, _ := lookup(EnvModelConsented)
	consentedEntries, err := decodeList(consented)
	if err != nil {
		return fmt.Errorf("%s: invalid list: %w", EnvModelConsented, err)
	}
	for _, entry := range consentedEntries {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			cfg.ModelConsented = append(cfg.ModelConsented, trimmed)
		}
	}

	baseURL, err := optionalVendorURL(lookup, EnvModelBaseURL)
	if err != nil {
		return err
	}
	cfg.ModelBaseURL = baseURL

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
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("%s: the key file could not be read", EnvGitHubAppKeyFile)
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

// slackAgentRollout reads which organizations the Slack agent surface is live for.
//
// Names are carried as text, exactly as every other organization name this package reads is.
// This package deliberately depends on nothing else in the product, so what a valid
// organization name is stays the tenancy package's answer and is not restated here — and a
// name that is not one simply matches nothing, which is the same outcome as leaving it out.
func slackAgentRollout(lookup func(string) (string, bool), cfg *Config) error {
	raw, _ := lookup(EnvSlackAgentOrganizations)
	entries, err := decodeList(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid list: %w", EnvSlackAgentOrganizations, err)
	}
	for _, entry := range entries {
		if name := strings.TrimSpace(entry); name != "" {
			cfg.SlackAgentOrganizations = append(cfg.SlackAgentOrganizations, name)
		}
	}
	return nil
}

// SlackAgentLiveFor reports whether the Slack agent surface is live for one organization.
//
// A method rather than a bare list, so the surfaces consulting it cannot each invent their
// own idea of what an empty list means. It means NO organization, which is the safe reading
// of an unset rollout gate and the default this ships with.
func (c Config) SlackAgentLiveFor(organization string) bool {
	for _, name := range c.SlackAgentOrganizations {
		if name == organization {
			return true
		}
	}
	return false
}

// gitHubInstallFlow reads what the one-click installation flow needs: all three of the
// slug, the client id and the client secret, or none of them. Two of three would offer a
// connect button that cannot complete, and the person who set two is still reading when
// this refuses. The secret's contents never appear in an error.
func gitHubInstallFlow(lookup func(string) (string, bool), cfg *Config) error {
	slug, _ := lookup(EnvGitHubAppSlug)
	slug = strings.TrimSpace(slug)
	clientID, _ := lookup(EnvGitHubClientID)
	clientID = strings.TrimSpace(clientID)
	path, _ := lookup(EnvGitHubClientSecretFile)
	path = strings.TrimSpace(path)

	set := 0
	for _, value := range []string{slug, clientID, path} {
		if value != "" {
			set++
		}
	}
	switch {
	case set == 0:
		return nil
	case set < 3:
		return fmt.Errorf("%s, %s and %s are set together or not at all; a partial "+
			"installation flow offers a button that cannot finish",
			EnvGitHubAppSlug, EnvGitHubClientID, EnvGitHubClientSecretFile)
	case cfg.GitHubAppID == "":
		return fmt.Errorf("%s needs %s: an installation flow with no app credential "+
			"cannot verify what it installed", EnvGitHubAppSlug, EnvGitHubAppID)
	}
	secret, err := readSecretFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvGitHubClientSecretFile, err)
	}
	cfg.GitHubAppSlug, cfg.GitHubClientID, cfg.GitHubClientSecret = slug, clientID, secret
	return nil
}

// optionalVendorURL reads a base URL a provider reaches its vendor at. Unlike an origin,
// a path is allowed — vendor APIs live under one — and https is required except on
// loopback, because a credential is presented to whatever answers here.
func optionalVendorURL(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", nil
	}
	trimmed := strings.TrimSuffix(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "", fmt.Errorf("%s must be an absolute URL such as https://vendor.example.com/api", key)
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" &&
		parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		return "", fmt.Errorf("%s must be https; a credential is presented to this URL", key)
	}
	return trimmed, nil
}

func optionalDuration(
	lookup func(string) (string, bool), key string, fallback time.Duration,
) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}

func optionalName(lookup func(string) (string, bool), key, fallback string) (string, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s must not be blank", key)
	}
	return trimmed, nil
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
