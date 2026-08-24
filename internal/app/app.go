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
	"github.com/open-cluster/oc-control-plane/internal/integrations/github"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/observability"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
	"github.com/open-cluster/oc-control-plane/internal/reasoning/providers"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// readHeaderTimeout bounds how long a client may take to send its headers, which is the
// cheapest defence against a slow-loris holding connections open.
const readHeaderTimeout = 10 * time.Second

// Bounds on the operator surface's connections. They are longer than a request needs and
// shorter than an idle connection may be held, which is the whole job.
const (
	operatorReadTimeout  = 30 * time.Second
	operatorWriteTimeout = 30 * time.Second
	operatorIdleTimeout  = 60 * time.Second
)

// Bounds on intake's connections. Shorter than the operator surface's, because a webhook
// delivery is one bounded body from a machine rather than a person reading a page, and this is
// the surface reachable from outside.
const (
	intakeReadTimeout  = 20 * time.Second
	intakeWriteTimeout = 20 * time.Second
	intakeIdleTimeout  = 30 * time.Second
)

// Options carries the process facts and boundary replacements supplied by the command or a
// composed-process test. Production supplies Version and leaves the replacements empty.
//
// OnListen lets a test address an ephemeral port without racing the listener. Investigator
// lets a test use a scripted model boundary instead of a paid provider.
type Options struct {
	Version      string
	OnListen     func(net.Addr)
	Investigator investigation.Investigator
}

// Run assembles and serves the control plane until ctx is cancelled, then drains within
// the configured timeout. It returns nil on a clean shutdown.
//
// Options carries what a test may put in place of the real thing: the address callback, so an
// ephemeral port can be addressed without racing the listener, and the model boundary,
// so investigations run against a scripted reasoner instead of a paid provider.
// Production supplies nothing here.
func Run(
	ctx context.Context, cfg config.Config, logOutput io.Writer, options Options,
) error {
	version := strings.TrimSpace(options.Version)
	if version == "" {
		version = "dev"
	}
	telemetry, err := observability.Start(ctx, observability.Options{
		ServiceName:    cfg.ServiceName,
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
		slog.String("service", cfg.ServiceName),
		slog.Int("placements", len(cfg.Placements)))

	placements, err := storage.OpenPlacements(ctx, storage.Layout{
		Placements:       cfg.Placements,
		Assignments:      cfg.Assignments,
		DefaultPlacement: cfg.DefaultPlacement,
	})
	if err != nil {
		return err
	}
	defer placements.Close()

	applied, err := placements.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	logMigrations(logger, applied)

	// The GitHub App is deployment configuration; a deployment without one still serves
	// github in the catalog — the compiled provider set and the seeded reference rows
	// must agree exactly — and connecting it fails live, with the reason. A key that
	// cannot sign refuses startup, where whoever supplied it is still reading.
	gitHubClient := github.NewClient(cfg.GitHubAPIURL)
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
	var gitHubInstaller *github.Installer
	if cfg.GitHubAppSlug != "" {
		gitHubInstaller, err = github.NewInstaller(cfg.GitHubAppSlug, cfg.GitHubClientID,
			cfg.GitHubClientSecret, cfg.GitHubWebURL)
		if err != nil {
			return fmt.Errorf("%s: %w", config.EnvGitHubAppSlug, err)
		}
	}

	// Slack's installation flow is registered separately from its credential, exactly as
	// GitHub's is: a deployment that registered no Slack app offers no connect button and
	// serves the pasted-token form, which is the air-gapped path and stays supported.
	var slackInstaller *slack.Installer
	if cfg.SlackClientID != "" {
		slackInstaller, err = slack.NewInstaller(
			cfg.SlackClientID, cfg.SlackClientSecret, cfg.SlackAPIURL)
		if err != nil {
			return fmt.Errorf("%s: %w", config.EnvSlackClientID, err)
		}
	}

	// The catalog is assembled HERE, and this is the only place that knows every provider.
	// A duplicate key or a definition missing its verification refuses startup, where the
	// person who caused it is still the person reading the error.
	catalog, err := integrations.NewCatalog(
		alertmanager.Definition(),
		kubernetes.Definition(),
		slack.Definition(slack.NewClient(cfg.SlackAPIURL), slackInstaller,
			cfg.SlackSigningSecret != ""),
		github.Definition(gitHubInstaller, gitHubApp, gitHubClient, cfg.GitHubWebURL),
	)
	if err != nil {
		return fmt.Errorf("assembling the integration catalog: %w", err)
	}

	// One sealer for the process: identity client secrets and integration credentials are
	// sealed under the same deployment key.
	var sealer seal.Sealer
	if len(cfg.SealingKey) > 0 {
		if sealer, err = seal.New(cfg.SealingKey); err != nil {
			return fmt.Errorf("%s: %w", config.EnvSealingKeyFile, err)
		}
	}

	// The model boundary: a test's scripted investigator outranks configuration, and a
	// deployment that configured no model provider cannot investigate — the surface
	// says so per request rather than the process refusing to serve everything else it
	// can do.
	investigator := options.Investigator
	if investigator == nil && cfg.ModelProvider != "" {
		if investigator, err = modelBoundary(cfg, catalog, logger); err != nil {
			return err
		}
	}

	investigations := &investigation.Runner{
		Events:        placements,
		Store:         placements,
		Leases:        placements,
		Catalog:       catalog,
		Sealer:        sealer,
		Investigator:  investigator,
		MaxToolRuns:   cfg.InvestigationMaxToolRuns,
		MaxTurns:      cfg.InvestigationMaxTurns,
		OrgConcurrent: cfg.OrgConcurrentInvestigations,
		WindowLead:    cfg.InvestigationWindowLead,
		// The context budget is computed HERE, where the model is known, and handed to the
		// domain as a number. internal/investigation must never learn what a vendor is,
		// and "how big is this model's window" is a vendor fact.
		ContextBudget: reasoning.ContextBudget(cfg.ModelName, cfg.ModelContextWindow,
			cfg.ContextThresholdPercent),
		// The ceiling is the same window without the soft threshold applied, so it always
		// sits above the budget. The distance between them is the room a compaction buys
		// the turn that performed it.
		ContextCeiling:         reasoning.ContextCeiling(cfg.ModelName, cfg.ModelContextWindow),
		ModelName:              cfg.ModelName,
		Telemetry:              investigation.NewTelemetry(logger),
		Logger:                 logger,
		SpendCeilingMicroCents: microCentsOf(cfg.ModelSpendCeilingCents),
	}
	// Drained on the way out: an investigation mid-flight is failed with the reason
	// recorded rather than orphaned into a record that says running forever.
	defer investigations.Drain()

	// The claiming and recovery loops. They live as long as the process: the claimer is
	// what makes a Conversation turn actually happen — a message opens an investigation
	// with no lease and answers the person immediately — and the sweeper is what turns a
	// worker that died into a stated failure rather than a spinner nobody ever stops
	// watching. Both end with the run context, so shutdown stops looking for work before
	// Drain waits for what it already holds.
	//
	// Only a deployment with a model provider runs them. Claiming work this process could
	// not investigate would take a lease, fail for the one reason the operator surface
	// already reports per request, and do it again for every turn.
	if investigator != nil {
		go investigations.Claim(ctx)
		go investigations.Sweep(ctx)
	}

	return serve(ctx, assembled{
		config:         cfg,
		logger:         logger,
		telemetry:      telemetry,
		placements:     placements,
		catalog:        catalog,
		sealer:         sealer,
		investigations: investigations,
		onListen:       options.OnListen,
	})
}

// modelBoundary builds the configured deployment's Exchange driver. Everything
// that could be wrong with the model configuration is refused HERE, at startup: an
// unimplemented provider, an unpriced model, an effort level nothing recognises, a
// provider nobody consented to.
func modelBoundary(
	cfg config.Config, catalog integrations.Catalog, logger *slog.Logger,
) (investigation.Investigator, error) {
	deployment := reasoning.Deployment{
		Provider:               cfg.ModelProvider,
		Model:                  cfg.ModelName,
		Effort:                 reasoning.Effort(cfg.ModelEffort),
		BaseURL:                cfg.ModelBaseURL,
		Credential:             reasoning.Secret(cfg.ModelKey),
		SpendCeilingMicroCents: microCentsOf(cfg.ModelSpendCeilingCents),
	}.WithDefaults()
	if err := deployment.Validate(); err != nil {
		return nil, err
	}
	provider, err := providers.Open(deployment, providers.Options{})
	if err != nil {
		return nil, err
	}
	agent, err := reasoning.NewAgent(deployment, provider,
		reasoning.DefaultTariff(), reasoning.ConsentTo(cfg.ModelConsented...))
	if err != nil {
		return nil, err
	}
	revision := reasoning.AgentRevision(catalog.Tools())
	agent.Instrument(reasoning.NewTelemetry(logger, revision))
	logger.Info("model boundary configured",
		slog.String("deployment", deployment.String()),
		slog.String("agent_revision", revision))
	return agent, nil
}

// microCentsOf converts a configured whole-cent figure into the integer micro-cents
// every spend record counts in.
func microCentsOf(cents int) int64 { return int64(cents) * 1_000_000 }

// assembled is the constructed process: the pieces serve needs, which are meaningless
// apart and always travel together.
type assembled struct {
	config         config.Config
	logger         *slog.Logger
	telemetry      *observability.Telemetry
	placements     *storage.Placements
	catalog        integrations.Catalog
	sealer         seal.Sealer
	investigations *investigation.Runner
	onListen       func(net.Addr)
}
