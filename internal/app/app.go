// Package app composes and runs the OpenCluster control plane.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/genericwebhook"
	"github.com/open-cluster/oc-control-plane/internal/integrations/github"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/anthropic"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/zai"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/telemetry"
)

// readHeaderTimeout bounds how long a client may take to send its headers, which is the
// cheapest defence against a slow-loris holding connections open.
const readHeaderTimeout = 10 * time.Second

const (
	defaultServiceName             = "oc-control-plane"
	defaultShutdownTimeout         = 15 * time.Second
	defaultInventoryInterval       = 5 * time.Minute
	defaultChangeRetentionDays     = 90
	defaultInvestigationWindowLead = 2 * time.Hour
)

// Bounds on connections to the shared HTTP surface. Route owners retain their own body and
// request limits inside these server-wide bounds.
const (
	operatorReadTimeout  = 30 * time.Second
	operatorWriteTimeout = 30 * time.Second
	operatorIdleTimeout  = 60 * time.Second
)

type Options struct {
	Version      string
	OnListen     func(net.Addr)
	Agent        investigation.Agent
	Model        agent.Model
	ModelEffort  string
	ModelBaseURL string
	// InventoryInterval replaces the production cadence in composed-process tests.
	InventoryInterval time.Duration
	MaxToolRuns       int
	MaxTurns          int
	SlackAPIURL       string
	GitHubAPIURL      string
}

func Run(
	ctx context.Context,
	cfg config.Config,
	logOutput io.Writer,
	options Options,
) error {
	version := strings.TrimSpace(options.Version)
	if version == "" {
		version = "dev"
	}
	telemetry, err := observability.Start(ctx, observability.Options{
		ServiceName:    defaultServiceName,
		ServiceVersion: version,
		OTLPEndpoint:   cfg.OTLPEndpoint,
		LogOutput:      logOutput,
	})
	if err != nil {
		return err
	}
	defer func() {
		// Flushing telemetry uses a fresh context: the process context is already cancelled
		// by the time this runs, and an exporter given a dead context flushes nothing.
		if shutdownErr := telemetry.Shutdown(context.WithoutCancel(ctx)); shutdownErr != nil {
			telemetry.Logger.Warn("telemetry shutdown", slog.String("error", shutdownErr.Error()))
		}
	}()

	logger := telemetry.Logger
	logger.Info("control plane starting",
		slog.String("version", version),
		slog.String("service", defaultServiceName))

	database, err := storage.OpenDatabase(ctx, cfg.DatabaseDSN)
	if err != nil {
		return err
	}
	defer database.Close()

	applied, err := database.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	logMigrations(logger, applied)

	// The GitHub App is deployment configuration; a deployment without one still serves
	// github in the catalog — the compiled provider set and the seeded reference rows
	// must agree exactly — and connecting it fails live, with the reason. A key that
	// cannot sign refuses startup, where whoever supplied it is still reading.
	gitHubClient := github.NewClient(options.GitHubAPIURL)
	var gitHubApp *github.App
	if len(cfg.GitHubAppKey) > 0 {
		gitHubApp, err = github.NewApp(cfg.GitHubAppID, cfg.GitHubAppKey, gitHubClient)
		if err != nil {
			return fmt.Errorf("%s: %w", config.EnvGitHubAppKeyFile, err)
		}
	}
	// The installation flow is registered separately from the credential: a deployment may
	// hold an App and offer no one-click install — which is the self-hosted case — and it
	// then serves the configuration form exactly as it does today.

	// Slack's installation flow is registered separately from its credential, exactly as
	// GitHub's is: a deployment that registered no Slack app offers no connect button and
	// serves the pasted-token form, which is the air-gapped path and stays supported.
	var slackInstaller *slack.Installer
	if cfg.SlackClientID != "" {
		slackInstaller, err = slack.NewInstaller(
			cfg.SlackClientID, cfg.SlackClientSecret, options.SlackAPIURL)
		if err != nil {
			return fmt.Errorf("%s: %w", config.EnvSlackClientID, err)
		}
	}

	// The catalog is assembled HERE, and this is the only place that knows every provider.
	// A duplicate key or a definition missing its verification refuses startup, where the
	// person who caused it is still the person reading the error.
	catalog, err := integrations.NewCatalog(
		alertmanager.Definition(),
		kubernetes.Definition(kubernetes.RelayExecutor{Database: database}),
		slack.Definition(slack.NewClient(options.SlackAPIURL), slackInstaller,
			cfg.SlackSigningSecret != ""),
		github.Definition(gitHubApp, gitHubClient),
		genericwebhook.Definition(),
	)
	if err != nil {
		return fmt.Errorf("assembling the integration catalog: %w", err)
	}
	if err = database.ReconcileIntegrationTypes(ctx, catalog.Manifests()); err != nil {
		return err
	}

	// One sealer for the process: identity client secrets and integration credentials are
	// sealed under the same deployment key.
	var sealer seal.Sealer
	if len(cfg.SealingKey) > 0 {
		if sealer, err = configuredSealer(cfg); err != nil {
			return fmt.Errorf("%s: %w", config.EnvSealingKeyFile, err)
		}
	}

	investigationAgent := options.Agent
	var configuredAgent *agent.Agent
	if investigationAgent == nil && cfg.ModelProvider != "" {
		if configuredAgent, err = modelBoundary(cfg, logger, options); err != nil {
			return err
		}
	}
	if configuredAgent != nil {
		configuredAgent.Store = database
		configuredAgent.Catalog = catalog
		configuredAgent.Sealer = sealer
		configuredAgent.RuntimeTelemetry = investigation.NewTelemetry(logger)
		configuredAgent.Logger = logger
		configuredAgent.MaxToolRuns = options.MaxToolRuns
		configuredAgent.MaxTurns = options.MaxTurns
		configuredAgent.ContextWindowTokens = cfg.ModelContextWindowTokens
		investigationAgent = configuredAgent
	}

	investigations := &investigation.Runner{
		Store:      database,
		Agent:      investigationAgent,
		Workers:    cfg.InvestigationWorkers,
		MaxPending: cfg.MaxPendingInvestigationsPerOrganization,
		WindowLead: defaultInvestigationWindowLead,
		Telemetry:  investigation.NewTelemetry(logger),
		Logger:     logger,
	}
	return serve(ctx, assembled{
		config:            cfg,
		logger:            logger,
		telemetry:         telemetry,
		database:          database,
		catalog:           catalog,
		sealer:            sealer,
		investigations:    investigations,
		onListen:          options.OnListen,
		inventoryInterval: inventoryInterval(options.InventoryInterval),
		slackAPIURL:       options.SlackAPIURL,
	})
}

func configuredSealer(cfg config.Config) (seal.Sealer, error) {
	return seal.NewKeyring(seal.Key{ID: "primary", Material: cfg.SealingKey})
}

// modelBoundary validates and builds the configured model-backed agent.
func modelBoundary(cfg config.Config, logger *slog.Logger, options Options) (*agent.Agent, error) {
	deployment := agent.Deployment{
		Provider:   cfg.ModelProvider,
		Model:      cfg.ModelName,
		Effort:     agent.Effort(options.ModelEffort),
		BaseURL:    options.ModelBaseURL,
		Credential: agent.Secret(cfg.ModelKey),
	}.WithDefaults()
	if err := deployment.Validate(); err != nil {
		return nil, err
	}
	model := options.Model

	if model == nil {
		var openErr error
		switch deployment.Provider {
		case anthropic.Name:
			model, openErr = anthropic.New(deployment, anthropic.Options{})
		case zai.Name:
			model, openErr = zai.New(deployment, zai.Options{})
		default:
			return nil, fmt.Errorf("%q is not a model provider this build serves; it serves [%s, %s]",
				deployment.Provider, anthropic.Name, zai.Name)
		}
		if openErr != nil {
			return nil, openErr
		}
	}
	built, err := agent.NewAgent(deployment, model)
	if err != nil {
		return nil, err
	}
	built.Instrument(agent.NewTelemetry(logger))
	logger.Info("model boundary configured",
		slog.String("deployment", deployment.String()))
	return built, nil
}

func inventoryInterval(replacement time.Duration) time.Duration {
	if replacement > 0 {
		return replacement
	}
	return defaultInventoryInterval
}

// assembled is the constructed process: the pieces serve needs, which are meaningless
// apart and always travel together.
type assembled struct {
	config            config.Config
	logger            *slog.Logger
	telemetry         *observability.Telemetry
	database          *storage.Database
	catalog           integrations.Catalog
	sealer            seal.Sealer
	investigations    *investigation.Runner
	onListen          func(net.Addr)
	inventoryInterval time.Duration
	slackAPIURL       string
}
