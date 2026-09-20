package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/changes"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
	alertwork "github.com/open-cluster/oc-control-plane/internal/webhooks/alertmanager"
	slackwork "github.com/open-cluster/oc-control-plane/internal/webhooks/slack"
)

const auditPruneInterval = time.Hour

func startWorkers(ctx context.Context, group *errgroup.Group, process assembled) {
	if process.investigations.Agent != nil {
		group.Go(func() error {
			process.investigations.Run(ctx)
			return nil
		})
	}
	startWebhookJob(ctx, group, process)
	startAuditPruner(ctx, group, process)
	startSessionPruner(ctx, group, process)
	startChangesPruner(ctx, group, process)
	startSlackReplyWorker(ctx, group, process)
}

func startWebhookJob(ctx context.Context, group *errgroup.Group, process assembled) {
	slackClient := slack.NewClient(process.slackAPIURL)
	worker := webhooks.Worker{
		Jobs: process.database,
		Handlers: webhooks.JobHandlers{
			storage.WebhookJobAlert: alertwork.JobHandler{
				Database: process.database, WindowLead: defaultInvestigationWindowLead,
				MaxWaitingTurns: process.config.MaxPendingInvestigations,
			},
			storage.WebhookJobSlack: slackwork.JobHandler{
				Jobs: process.database,
				References: slackwork.SlackReferenceResolver{
					Store:  slackwork.ReferenceDatabase{Database: process.database},
					Client: slackClient, Sealer: process.sealer,
				},
				WindowLead:      defaultInvestigationWindowLead,
				MaxWaitingTurns: process.config.MaxPendingInvestigations,
				Logger:          process.logger,
			},
		},
		Owner: uuid.NewString(), Lease: time.Minute, RetryBase: time.Second,
		MaxAttempts: 8, Logger: process.logger,
		Counters: webhooks.NewJobInstruments(process.logger),
	}
	group.Go(func() error {
		worker.Run(ctx)
		return nil
	})
	process.logger.Info("webhook delivery worker started")
}

// startAuditPruner runs the worker that applies each tenant's audit retention schedule.
func startAuditPruner(ctx context.Context, group *errgroup.Group, process assembled) {
	pruner := audit.Pruner{
		Retentions: process.database,
		Logger:     process.logger,
		Interval:   auditPruneInterval,
	}

	group.Go(func() error {
		pruner.Run(ctx)
		return nil
	})
	process.logger.Info("audit retention pruner started",
		slog.Duration("interval", auditPruneInterval))
}

// startChangesPruner runs the worker that ages captured changes out on the deployment's schedule.
func startChangesPruner(ctx context.Context, group *errgroup.Group, process assembled) {
	pruner := changes.Pruner{
		Retention: process.database,
		Logger:    process.logger,
		Days:      defaultChangeRetentionDays,
		Interval:  auditPruneInterval,
	}

	group.Go(func() error {
		pruner.Run(ctx)
		return nil
	})
	process.logger.Info("change retention pruner started",
		slog.Int("retention_days", defaultChangeRetentionDays))
}

// newSlackAgent returns a Slack webhook agent when Slack integration is configured.
// It returns nil when Slack event handling is disabled.
func newSlackAgent(cfg config.Config) *webhooks.SlackAgent {
	isSlackConfigured(cfg)
	return &webhooks.SlackAgent{
		SigningSecret:   cfg.SlackSigningSecret,
		Enabled:         func(tenancy.Organization) bool { return true },
		WindowLead:      defaultInvestigationWindowLead,
		MaxWaitingTurns: cfg.MaxPendingInvestigations,
	}
}

// startSlackReplyWorker starts the background worker that delivers replies to
// Slack threads. It does nothing when Slack integration is disabled.
func startSlackReplyWorker(
	ctx context.Context,
	group *errgroup.Group,
	process assembled,
) {
	isSlackConfigured(process.config)
	worker := slack.Worker{
		Replies:   process.database,
		Client:    slack.NewClient(process.slackAPIURL),
		Sealer:    process.sealer,
		Logger:    process.logger,
		Counters:  slack.NewInstruments(process.logger),
		PublicURL: process.config.PublicURL,
	}

	group.Go(func() error {
		worker.Run(ctx)
		return nil
	})

	process.logger.Info("slack delivery worker started")
}

func isSlackConfigured(cfg config.Config) bool {
	return cfg.SlackSigningSecret != ""
}
