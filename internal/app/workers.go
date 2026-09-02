package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/changecontext"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
	alertwork "github.com/open-cluster/oc-control-plane/internal/webhooks/alertmanager"
	slackwork "github.com/open-cluster/oc-control-plane/internal/webhooks/slack"
)

// auditPruneInterval is how often each tenant's declared retention schedule is applied.
//
// Retention is measured in days, so an hour is close enough to the horizon that the surface can
// honestly say the schedule is enforced, and far enough from it that nothing is spent looking.
const auditPruneInterval = time.Hour

func startWorkers(ctx context.Context, group *errgroup.Group, process assembled) {
	if process.investigations.Agent != nil {
		group.Go(func() error {
			process.investigations.Run(ctx)
			return nil
		})
	}
	startWebhookWork(ctx, group, process)
	startAuditPruner(ctx, group, process)
	startChangeLedgerPruner(ctx, group, process)
	startSlackReplies(ctx, group, process)
}

func startWebhookWork(ctx context.Context, group *errgroup.Group, process assembled) {
	slackClient := slack.NewClient(process.slackAPIURL)
	worker := webhooks.Worker{
		Work: process.database,
		Handlers: webhooks.WorkHandlers{
			storage.WebhookWorkAlert: alertwork.WorkHandler{
				Database: process.database, WindowLead: defaultInvestigationWindowLead,
				MaxWaitingTurns: process.config.MaxPendingInvestigationsPerOrganization,
			},
			storage.WebhookWorkSlack: slackwork.WorkHandler{
				Work: process.database,
				References: slackwork.SlackReferenceResolver{
					Store:  slackwork.ReferenceDatabase{Database: process.database},
					Client: slackClient, Sealer: process.sealer,
				},
				WindowLead:      defaultInvestigationWindowLead,
				MaxWaitingTurns: process.config.MaxPendingInvestigationsPerOrganization,
				Logger:          process.logger,
			},
		},
		Owner: uuid.NewString(), Lease: time.Minute, RetryBase: time.Second,
		MaxAttempts: 8, Logger: process.logger,
		Counters: webhooks.NewWorkInstruments(process.logger),
	}
	group.Go(func() error {
		worker.Run(ctx)
		return nil
	})
	process.logger.Info("webhook delivery worker started")
}

// startAuditPruner runs the worker that applies each tenant's audit retention schedule.
//
// It is unconditional, and that is what lets the policy surface state that retention is enforced.
// A deployment that ran the control plane without it would report a schedule it does not keep,
// which is the thing that surface was written to avoid saying.
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

// startChangeLedgerPruner runs the worker that ages the change ledger out on the
// deployment's schedule.
func startChangeLedgerPruner(ctx context.Context, group *errgroup.Group, process assembled) {
	pruner := changeledger.Pruner{
		Retention: process.database,
		Logger:    process.logger,
		Days:      defaultChangeRetentionDays,
		Interval:  auditPruneInterval,
	}

	group.Go(func() error {
		pruner.Run(ctx)
		return nil
	})
	process.logger.Info("change ledger retention pruner started",
		slog.Int("retention_days", defaultChangeRetentionDays))
}

// slackAgent is what the intake listener needs to receive Slack events, or nil where this
// deployment receives none.
//
// The Slack surface is available wherever a signing secret is configured.
func slackAgent(cfg config.Config) *webhooks.SlackAgent {
	if cfg.SlackSigningSecret == "" {
		return nil
	}
	return &webhooks.SlackAgent{
		SigningSecret: cfg.SlackSigningSecret,
		Enabled: func(tenancy.Organization) bool {
			return true
		},
		WindowLead:      defaultInvestigationWindowLead,
		MaxWaitingTurns: cfg.MaxPendingInvestigationsPerOrganization,
	}
}

// startSlackReplies runs the worker that answers in Slack threads, or nothing where this
// deployment receives no Slack events.
//
// It is stopped with the process and nothing waits for it. A delivery in flight when the
// process ends resumes from its own cursor in the next instance, which is the same property
// that makes a crash mid-stream survivable — so there is nothing here worth draining for.
func startSlackReplies(ctx context.Context, group *errgroup.Group, process assembled) {
	if process.config.SlackSigningSecret == "" {
		return
	}
	worker := slack.Worker{
		Replies:    process.database,
		Client:     slack.NewClient(process.slackAPIURL),
		Sealer:     process.sealer,
		Logger:     process.logger,
		Counters:   slack.NewInstruments(process.logger),
		ConsoleURL: process.config.OperatorPublicURL,
	}

	group.Go(func() error {
		worker.Run(ctx)
		return nil
	})
	process.logger.Info("slack delivery worker started")
}
