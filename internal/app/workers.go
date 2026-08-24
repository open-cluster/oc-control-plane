package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"google.golang.org/grpc"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/changeledger"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/integrations/slack"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// auditPruneInterval is how often each tenant's declared retention schedule is applied.
//
// Retention is measured in days, so an hour is close enough to the horizon that the surface can
// honestly say the schedule is enforced, and far enough from it that nothing is spent looking.
const auditPruneInterval = time.Hour

const auditForwardingInterval = time.Second

const credentialRotationBatch = 50

func startAuditForwarding(process assembled) *backgroundWorker {
	if process.auditForwarder == nil {
		return nil
	}
	ctx, stop := context.WithCancel(context.Background())
	worker := audit.ForwardingWorker{
		Outbox: process.database, Forwarder: process.auditForwarder, Owner: uuid.NewString(),
		Lease: time.Minute, RetryBase: time.Second, MaxAttempts: 8, Batch: 50,
		Logger: process.logger,
	}
	running := &backgroundWorker{stopping: stop, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		worker.Run(ctx, auditForwardingInterval)
	}()
	process.logger.Info("audit forwarding worker started")
	return running
}

func startCredentialRotation(process assembled) *backgroundWorker {
	if len(process.config.PreviousSealingKeys) == 0 {
		return nil
	}
	ctx, stop := context.WithCancel(context.Background())
	running := &backgroundWorker{stopping: stop, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		for {
			changed, complete, err := process.database.RewrapIntegrationCredentials(
				ctx, process.sealer, credentialRotationBatch)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				process.logger.Error("credential rotation sweep failed")
			} else {
				if changed > 0 {
					process.logger.Info("integration credentials rewrapped", slog.Int("count", changed))
				}
				if complete {
					process.logger.Info("integration credential rotation complete")
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(auditForwardingInterval):
			}
		}
	}()
	process.logger.Info("integration credential rotation started")
	return running
}

// startAuditPruner runs the worker that applies each tenant's audit retention schedule.
//
// It is unconditional, and that is what lets the policy surface state that retention is enforced.
// A deployment that ran the control plane without it would report a schedule it does not keep,
// which is the thing that surface was written to avoid saying.
func startAuditPruner(process assembled) *backgroundWorker {
	ctx, stop := context.WithCancel(context.Background())
	pruner := audit.Pruner{
		Retentions: process.database,
		Logger:     process.logger,
		Interval:   auditPruneInterval,
	}

	running := &backgroundWorker{stopping: stop, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		pruner.Run(ctx)
	}()
	process.logger.Info("audit retention pruner started",
		slog.Duration("interval", auditPruneInterval))
	return running
}

// startChangeLedgerPruner runs the worker that ages the change ledger out on the
// deployment's schedule.
func startChangeLedgerPruner(process assembled) *backgroundWorker {
	ctx, stop := context.WithCancel(context.Background())
	pruner := changeledger.Pruner{
		Retention: process.database,
		Logger:    process.logger,
		Days:      process.config.ChangeLedgerRetentionDays,
		Interval:  auditPruneInterval,
	}

	running := &backgroundWorker{stopping: stop, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		pruner.Run(ctx)
	}()
	process.logger.Info("change ledger retention pruner started",
		slog.Int("retention_days", process.config.ChangeLedgerRetentionDays))
	return running
}

// backgroundWorker is a running background job and the handle that stops it.
type backgroundWorker struct {
	stopping context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

// stop asks the worker to finish and waits for it.
func (w *backgroundWorker) stop() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		w.stopping()
		<-w.done
	})
}

// relayEndpoint is the relay-facing listener together with the sessions it carries.
type relayEndpoint struct {
	server   *grpc.Server
	sessions *relay.SessionService
	stopping sync.Once
}

// stop drains the sessions and then stops the endpoint, within the budget.
//
// The budget is the point. A streaming call does not end because the process was asked to
// stop, so waiting for every relay to notice would let one that has stopped reading decide how
// long a deploy takes. Cutting a session short loses nothing durable: leases expire on their
// own clock and the work returns.
//
// Calling this more than once is safe, so it can be deferred as a backstop for the paths that
// leave early and still be called explicitly on the ordinary shutdown path.
//
// The nil receiver is the configured-without-relays case, so the caller can defer this
// unconditionally rather than branching around it.
func (e *relayEndpoint) stop(budget time.Duration, logger *slog.Logger) {
	if e == nil {
		return
	}
	e.stopping.Do(func() { e.drain(budget, logger) })
}

func (e *relayEndpoint) drain(budget time.Duration, logger *slog.Logger) {
	// A relay is given part of the budget to finish and flush; the rest is what the results it
	// flushes need in order to arrive and be recorded. Granting the whole budget would mean
	// cutting off a relay that used exactly the time it was told it had.
	e.sessions.Drain(budget / 2)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.server.GracefulStop()
	}()

	select {
	case <-stopped:
	case <-time.After(budget):
		logger.Warn("relay sessions did not end within the drain budget; stopping now",
			slog.Duration("budget", budget))
		e.server.Stop()
		<-stopped
	}
}

// slackAgent is what the intake listener needs to receive Slack events, or nil where this
// deployment receives none.
//
// The rollout gate is a closure over deployment configuration rather than a list handed
// down, so intake consults ONE answer to "is this live here" and cannot grow a second
// reading of an empty list.
func slackAgent(cfg config.Config) *intake.SlackAgent {
	if cfg.SlackSigningSecret == "" {
		return nil
	}
	return &intake.SlackAgent{
		SigningSecret: cfg.SlackSigningSecret,
		Enabled: func(organization tenancy.Organization) bool {
			return cfg.SlackAgentLiveFor(organization.String())
		},
		WindowLead:      cfg.InvestigationWindowLead,
		MaxWaitingTurns: cfg.OrgWaitingInvestigations,
	}
}

// startSlackReplies runs the worker that answers in Slack threads, or nothing where this
// deployment receives no Slack events.
//
// It is stopped with the process and nothing waits for it. A delivery in flight when the
// process ends resumes from its own cursor in the next instance, which is the same property
// that makes a crash mid-stream survivable — so there is nothing here worth draining for.
func startSlackReplies(process assembled) *backgroundWorker {
	if process.config.SlackSigningSecret == "" {
		return nil
	}
	ctx, stop := context.WithCancel(context.Background())
	worker := slack.Worker{
		Replies:  process.database,
		Client:   slack.NewClient(process.config.SlackAPIURL),
		Sealer:   process.sealer,
		Logger:   process.logger,
		Counters: slack.NewInstruments(process.logger),
	}

	running := &backgroundWorker{stopping: stop, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		worker.Run(ctx)
	}()
	process.logger.Info("slack delivery worker started")
	return running
}
