package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/changes"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
	slackwork "github.com/open-cluster/oc-control-plane/internal/webhooks/slack"
)

const auditPruneInterval = time.Hour

func startWorkers(ctx context.Context, group *errgroup.Group, process assembled) {
	if process.agentAvailable {
		group.Go(func() error {
			process.investigations.Run(ctx)
			return nil
		})
		startSlackMessageWorker(ctx, group, process)
	}
	startAuditPruner(ctx, group, process)
	startSessionPruner(ctx, group, process)
	startChangesPruner(ctx, group, process)
	startSlackReplyWorker(ctx, group, process)
}

func startSlackMessageWorker(ctx context.Context, group *errgroup.Group, process assembled) {
	slackClient := slack.NewClient(process.slackAPIURL)
	worker := slackwork.MessageWorker{
		Database: process.database,
		References: &slackwork.SlackReferenceResolver{
			Database: process.database,
			Client:   slackClient,
			Sealer:   process.sealer,
		},
		WindowLead:      defaultInvestigationWindowLead,
		MaxWaitingTurns: process.config.MaxPendingInvestigations,
		Owner:           uuid.NewString(),
		Lease:           time.Minute,
		RetryBase:       time.Second,
		MaxAttempts:     8, Logger: process.logger,
		Counters: slackwork.NewMessageInstruments(process.logger),
	}
	group.Go(func() error {
		worker.Run(ctx)
		return nil
	})
	process.logger.Info("slack message worker started")
}

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

func newSlackAgent(cfg config.Config, agentAvailable bool) *webhooks.SlackAgent {
	isSlackConfigured(cfg)
	return &webhooks.SlackAgent{
		SigningSecret:   cfg.SlackSigningSecret,
		Enabled:         func(uuid.UUID) bool { return true },
		AgentAvailable:  agentAvailable,
		WindowLead:      defaultInvestigationWindowLead,
		MaxWaitingTurns: cfg.MaxPendingInvestigations,
	}
}

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
