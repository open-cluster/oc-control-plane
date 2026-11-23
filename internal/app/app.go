package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/model"

	"github.com/open-cluster/oc-control-plane/internal/ai"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/genericwebhook"
	"github.com/open-cluster/oc-control-plane/internal/integrations/github"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/integrations/loki"
	"github.com/open-cluster/oc-control-plane/internal/integrations/prometheus"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/integrations/tempo"
	"github.com/open-cluster/oc-control-plane/internal/seal"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/telemetry"
)

const readHeaderTimeout = 10 * time.Second

const (
	defaultServiceName             = "oc-control-plane"
	defaultShutdownTimeout         = 15 * time.Second
	defaultInventoryInterval       = 5 * time.Minute
	defaultChangeRetentionDays     = 90
	defaultInvestigationWindowLead = conversation.DefaultIncidentWindowLead
)

const (
	serverReadTimeout  = 30 * time.Second
	serverWriteTimeout = 30 * time.Second
	serverIdleTimeout  = 60 * time.Second
)

type Options struct {
	Version           string
	OnListen          func(net.Addr)
	PrepareExecution  PrepareExecution
	ExecutionWake     <-chan struct{}
	Model             model.ChatModel
	ModelBaseURL      string
	InventoryInterval time.Duration
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
		LogLevel:       cfg.LogLevel,
	})
	if err != nil {
		return err
	}
	defer func() {
		if shutdownErr := telemetry.Shutdown(context.WithoutCancel(ctx)); shutdownErr != nil {
			telemetry.Logger.Warn("Telemetry shutdown failed", slog.String("error", "telemetry could not be flushed"))
		}
	}()

	logger := telemetry.Logger
	logger.Info("OpenCluster starting",
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
	logMigrationSummary(logger, applied)

	gitHubClient := github.NewClient(options.GitHubAPIURL)
	var gitHubApp *github.App
	if len(cfg.GitHubAppPrivateKey) > 0 {
		gitHubApp, err = github.NewApp(cfg.GitHubAppID, cfg.GitHubAppPrivateKey, gitHubClient)
		if err != nil {
			return fmt.Errorf("%s: %w", config.EnvGitHubAppKeyFile, err)
		}
	}

	var slackInstaller *slack.Installer
	if cfg.SlackClientID != "" {
		slackInstaller, err = slack.NewInstaller(
			cfg.SlackClientID, cfg.SlackClientSecret, options.SlackAPIURL)
		if err != nil {
			return fmt.Errorf("%s: %w", config.EnvSlackClientID, err)
		}
	}

	// integrations catalog ------>
	catalog, err := integrations.NewCatalog(
		alertmanager.Definition(),
		kubernetes.Definition(kubernetes.RelayExecutor{Database: database}),
		slack.Definition(slack.NewClient(options.SlackAPIURL), slackInstaller),
		github.Definition(gitHubApp, gitHubClient, cfg.GitHubClientID, cfg.GitHubClientSecret),
		genericwebhook.Definition(),
		prometheus.Definition(),
		loki.Definition(),
		tempo.Definition(),
	)
	if err != nil {
		return fmt.Errorf("assembling the integration catalog: %w", err)
	}

	var sealer seal.Sealer
	if len(cfg.SealingKey) > 0 {
		if sealer, err = configuredSealer(cfg); err != nil {
			return fmt.Errorf("%s: %w", config.EnvSealingKeyFile, err)
		}
	}

	prepare := options.PrepareExecution
	chatModel := options.Model
	if prepare == nil && chatModel == nil && cfg.ModelProvider != "" {
		chatModel, err = ai.NewChatModel(cfg, options.ModelBaseURL, ollamaContextSize)
		if err != nil {
			return err
		}
	}
	if prepare == nil && chatModel != nil {
		prepare = prepareInvestigationExecution(database, catalog, sealer, chatModel)
	}

	return serve(ctx, assembled{
		config:            cfg,
		logger:            logger,
		telemetry:         telemetry,
		database:          database,
		catalog:           catalog,
		sealer:            sealer,
		prepareExecution:  prepare,
		executionWake:     options.ExecutionWake,
		onListen:          options.OnListen,
		inventoryInterval: inventoryInterval(options.InventoryInterval),
		slackAPIURL:       options.SlackAPIURL,
	})
}

func configuredSealer(cfg config.Config) (seal.Sealer, error) {
	return seal.New(cfg.SealingKey)
}

func inventoryInterval(replacement time.Duration) time.Duration {
	if replacement > 0 {
		return replacement
	}
	return defaultInventoryInterval
}

type assembled struct {
	streamContext     context.Context
	config            config.Config
	logger            *slog.Logger
	telemetry         *observability.Telemetry
	database          *storage.Database
	catalog           integrations.Catalog
	sealer            seal.Sealer
	prepareExecution  PrepareExecution
	executionWake     <-chan struct{}
	onListen          func(net.Addr)
	inventoryInterval time.Duration
	slackAPIURL       string
}

func (p assembled) agentAvailable() bool {
	return p.prepareExecution != nil
}
