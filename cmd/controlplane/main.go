// Command controlplane is the OpenCluster control plane's composition root: it loads and
// validates configuration, assembles telemetry, opens one connection pool per placement,
// applies the embedded migrations, serves liveness, readiness, and metrics, and drains
// in-flight requests when the process is asked to stop.
//
// There is no domain logic here yet. The value of this package is that placement
// resolution, organization scoping, observability, migration discipline, and shutdown
// semantics are all in place before any domain depends on them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/changeledger"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/identity"
	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/observability"
	"github.com/open-cluster/oc-control-plane/internal/operator"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// version is stamped at release build time via -ldflags; "dev" otherwise.
var version = "dev"

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

// main is the only place that exits, so every deferred cleanup in start runs first.
func main() {
	if err := start(); err != nil {
		// The process failed before or after a logger existed; either way this write is the
		// last word. The message names the offending variable and never carries secret
		// material, because configuration refuses to put a credential in an environment
		// value in the first place.
		fmt.Fprintf(os.Stderr, "control plane exiting: %v\n", err)
		os.Exit(1)
	}
}

// start installs signal handling, loads configuration, and runs the control plane.
func start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	return run(ctx, cfg, os.Stderr, wiring{})
}

// wiring is what a test may put in place of the real thing. The address callback lets a
// test address an ephemeral port without racing the listener. Production supplies nothing
// here.
type wiring struct {
	onListen func(net.Addr)
}

// run assembles and serves the control plane until ctx is cancelled, then drains within
// the configured timeout. It returns nil on a clean shutdown.
//
// replace carries what a test puts in place of the real thing: the address callback, so an
// ephemeral port can be addressed without racing the listener, and the model boundary. Production
// passes an empty value.
func run(
	ctx context.Context, cfg config.Config, logOutput io.Writer, replace wiring,
) error {
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

	// The catalog is assembled HERE, and this is the only place that knows every provider.
	// A duplicate key or a definition missing its verification refuses startup, where the
	// person who caused it is still the person reading the error.
	catalog, err := integrations.NewCatalog(
		alertmanager.Definition(),
		kubernetes.Definition(),
	)
	if err != nil {
		return fmt.Errorf("assembling the integration catalog: %w", err)
	}

	return serve(ctx, assembled{
		config:     cfg,
		logger:     logger,
		telemetry:  telemetry,
		placements: placements,
		catalog:    catalog,
		onListen:   replace.onListen,
	})
}

// assembled is the constructed process: the pieces serve needs, which are meaningless
// apart and always travel together.
type assembled struct {
	config     config.Config
	logger     *slog.Logger
	telemetry  *observability.Telemetry
	placements *storage.Placements
	catalog    integrations.Catalog
	onListen   func(net.Addr)
}

// serve opens the listener and runs the HTTP surface until ctx is cancelled, then drains.
func serve(ctx context.Context, process assembled) error {
	cfg, logger := process.config, process.logger

	handlers := health.Handlers{
		Ready:   process.placements.Ping,
		Metrics: process.telemetry.MetricsHandler,
		Logger:  logger,
	}
	server := &http.Server{
		Handler:           handlers.Router(),
		ReadHeaderTimeout: readHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	listener, err := net.Listen("tcp", cfg.HTTPAddress)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.HTTPAddress, err)
	}
	if process.onListen != nil {
		process.onListen(listener.Addr())
	}
	logger.Info("listening", slog.String("address", listener.Addr().String()))

	// One slot per surface that can report a failure. Too few would leave the last goroutines
	// blocked forever on a send nobody is left to receive.
	failed := make(chan error, 4)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			failed <- serveErr
			return
		}
		failed <- nil
	}()

	// The Relay endpoint is a second listener on purpose. It speaks a different protocol to
	// a different kind of caller, and putting it on the HTTP port would place it behind that
	// surface's middleware and expose the health surface to relays.
	relays, err := startRelayEndpoint(process, failed)
	if err != nil {
		return err
	}
	defer relays.stop(cfg.ShutdownTimeout, logger)

	// The operator surface is a third listener for the same reason the second one exists: it
	// reads across tenants and belongs on an interface that health and metrics do not.
	operators, err := startOperatorEndpoint(process, failed)
	if err != nil {
		return err
	}
	defer operators.stop(cfg.ShutdownTimeout, logger)

	// Intake is a fourth, and the only one a customer's own infrastructure reaches inbound.
	// Separating it is what lets a deployment publish alert intake without publishing health,
	// metrics, the operator surface, or the relay endpoint alongside it.
	intake, err := startIntakeEndpoint(process, failed)
	if err != nil {
		return err
	}
	defer intake.stop(cfg.ShutdownTimeout, logger)

	// The retention pruner is not a listener: it applies the schedule each tenant declared
	// for its own record. It is stopped with the process and nothing waits for it, because
	// the work it does is bounded per batch and the next instance simply continues.
	pruners := startAuditPruner(process)
	defer pruners.stop()

	// The change ledger ages out on its own schedule, independent of the audit record's:
	// it is derived operational context, and what bounds it is the deployment's retention
	// rather than a tenant's declaration.
	ledgerPruners := startChangeLedgerPruner(process)
	defer ledgerPruners.stop()

	select {
	case serveErr := <-failed:
		return serveErr
	case <-ctx.Done():
	}

	// Drain: stop accepting, let in-flight requests finish within the budget, then exit.
	// The shutdown context is detached from the already-cancelled process context, or the
	// drain would end instantly and defeat the point.
	logger.Info("draining", slog.Duration("timeout", cfg.ShutdownTimeout))
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	// Both surfaces drain at once, under one budget. Draining them in sequence would let a slow
	// HTTP drain spend the whole budget and leave relay sessions none of it, and would make a
	// shutdown take twice as long as it was configured to.
	var stopped sync.WaitGroup
	stopped.Add(3)
	go func() {
		defer stopped.Done()
		relays.stop(cfg.ShutdownTimeout, logger)
	}()
	go func() {
		defer stopped.Done()
		operators.stop(cfg.ShutdownTimeout, logger)
	}()
	go func() {
		defer stopped.Done()
		intake.stop(cfg.ShutdownTimeout, logger)
	}()

	err = server.Shutdown(drainCtx)
	stopped.Wait()
	if err != nil {
		return fmt.Errorf("draining: %w", err)
	}
	<-failed
	logger.Info("stopped")
	return nil
}

// logMigrations reports the schema effect of this start, so a deployment's schema change
// is visible without querying the database. Placements are reported in a stable order;
// ranging a map directly would scramble the output that storage deliberately orders.
func logMigrations(logger *slog.Logger, applied map[string][]string) {
	placements := make([]string, 0, len(applied))
	for placement := range applied {
		placements = append(placements, placement)
	}
	sort.Strings(placements)

	for _, placement := range placements {
		versions := applied[placement]
		if len(versions) == 0 {
			logger.Info("schema current", slog.String("placement", placement))
			continue
		}
		logger.Info("migrations applied",
			slog.String("placement", placement),
			slog.Any("versions", versions))
	}
}

// startRelayEndpoint listens for relays when one is configured. A configuration with no relay
// address returns nothing to stop, which is correct for a deployment that serves no relays yet
// rather than a reason to fail.
//
// A serve error goes to the same channel the HTTP server uses, so either surface failing
// ends the process rather than leaving it half-serving.
func startRelayEndpoint(process assembled, failed chan<- error) (*relayEndpoint, error) {
	cfg := process.config
	if cfg.RelayAddress == "" {
		return nil, nil
	}

	listener, err := net.Listen("tcp", cfg.RelayAddress)
	if err != nil {
		return nil, fmt.Errorf("listening for relays on %s: %w", cfg.RelayAddress, err)
	}

	endpoint := &relayEndpoint{
		server:   grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler())),
		sessions: relay.NewSessionService(process.placements, process.logger, cfg.InventoryInterval),
	}
	relayv1.RegisterRelayRegistrationServiceServer(endpoint.server,
		relay.NewRegistrationService(process.placements, cfg.RelaySPKIPins, process.logger))
	relayv1.RegisterRelaySessionServiceServer(endpoint.server, endpoint.sessions)

	process.logger.Info("listening for relays",
		slog.String("address", listener.Addr().String()))

	go func() {
		if serveErr := endpoint.server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, grpc.ErrServerStopped) {
			failed <- serveErr
		}
	}()
	return endpoint, nil
}

// startOperatorEndpoint listens for operators when one is configured. A deployment with no
// operator address exposes nothing, which is the right default for a surface that reads across
// tenants: it has to be put somewhere deliberately.
func startOperatorEndpoint(process assembled, failed chan<- error) (*operatorEndpoint, error) {
	cfg := process.config
	if cfg.OperatorAddress == "" {
		return nil, nil
	}

	identities, err := operatorIdentity(process)
	if err != nil {
		return nil, err
	}
	// The route table becomes a mux HERE, before the listener opens, so a route that cannot be
	// authorized correctly is a process that refuses to start rather than a route that is
	// served open. That is the runtime half of "a new route without a declared permission
	// cannot ship"; the compile-time half is that authz.Privileged takes the permission
	// positionally, and the gate in internal/gates is the third.
	router, err := operator.Handlers{
		Placements:          process.placements,
		Logger:              process.logger,
		Identity:            identities,
		Origins:             cfg.OperatorAllowedOrigins,
		Catalog:             process.catalog,
		IntakeBaseURL:       cfg.IntakePublicURL,
		MinimumRelayVersion: cfg.MinimumRelayVersion,
	}.Router()
	if err != nil {
		return nil, fmt.Errorf("assembling the operator surface: %w", err)
	}

	listener, err := net.Listen("tcp", cfg.OperatorAddress)
	if err != nil {
		return nil, fmt.Errorf("listening for operators on %s: %w", cfg.OperatorAddress, err)
	}

	endpoint := &operatorEndpoint{server: &http.Server{
		Handler: router,
		// Bounded at every stage, not just the headers. This port answers across tenants and
		// its connections are unauthenticated until a request has been read, so a client that
		// opens one and then goes quiet must not be able to hold it.
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       operatorReadTimeout,
		WriteTimeout:      operatorWriteTimeout,
		IdleTimeout:       operatorIdleTimeout,
	}}

	process.logger.Info("listening for operators",
		slog.String("address", listener.Addr().String()))

	go func() {
		if serveErr := endpoint.server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			failed <- serveErr
		}
	}()
	return endpoint, nil
}

// operatorIdentity assembles who may reach the operator surface.
//
// Everything that could be wrong with the identity configuration is refused HERE, at startup,
// where the person who wrote it is still the person reading the error: a bootstrap role that
// names no role this build has, a key of the wrong length, a console on a plaintext origin. A
// deployment that started with an unusable identity configuration would look healthy and
// authenticate nobody, and this is the last moment anyone can be told.
func operatorIdentity(process assembled) (identity.Handlers, error) {
	cfg := process.config

	handlers := identity.Handlers{
		Placements: process.placements,
		Logger:     process.logger,
		OIDC:       identity.NewOIDC(),
		PublicURL:  cfg.OperatorPublicURL,
		ConsoleURL: cfg.OperatorConsoleURL,
		// This process starts the pruner unconditionally, so the policy surface may say that a
		// declared retention schedule is applied. It is passed rather than assumed because the
		// statement is made to an auditor, and the only way to keep it true is for the component
		// that starts the pruner to be the one that says it did.
		RetentionEnforced: true,
	}

	if len(cfg.IdentityEncryptionKey) > 0 {
		sealer, err := identity.NewSealer(cfg.IdentityEncryptionKey)
		if err != nil {
			return identity.Handlers{}, fmt.Errorf("%s: %w",
				config.EnvIdentityEncryptionKeyFile, err)
		}
		handlers.Sealer = sealer
	}

	if len(cfg.OperatorTokenDigest) == 0 {
		return handlers, nil
	}
	organization, err := tenancy.NewOrganization(cfg.OperatorTokenOrganization)
	if err != nil {
		return identity.Handlers{}, fmt.Errorf("%s: %w",
			config.EnvOperatorTokenOrganization, err)
	}
	// The default is applied here as well as in config.Load, because a Config may be
	// constructed directly — every harness in this package does — and a composition root that
	// only worked for configuration that came through the parser would fail in exactly the
	// place nobody exercises it.
	named := cfg.OperatorTokenRole
	if strings.TrimSpace(named) == "" {
		named = string(authz.Admin)
	}
	role, known := authz.ParseRole(named)
	if !known {
		return identity.Handlers{}, fmt.Errorf(
			"%s names %q, which is not a role this build has",
			config.EnvOperatorTokenRole, cfg.OperatorTokenRole)
	}
	handlers.Bootstrap = identity.Bootstrap{
		Digest:       cfg.OperatorTokenDigest,
		Organization: organization,
		Role:         role,
		// Named so an event produced by the bootstrap credential is distinguishable in the
		// record from one produced by a service account somebody created.
		Name: "bootstrap credential",
	}
	process.logger.Info("bootstrap operator credential configured",
		slog.String("organization", organization.String()),
		slog.String("role", string(role)))
	return handlers, nil
}

// startIntakeEndpoint listens for alert deliveries when one is configured. A deployment with
// no intake address takes no alerts, which is correct for an instance that only serves relays.
func startIntakeEndpoint(process assembled, failed chan<- error) (*intakeEndpoint, error) {
	cfg := process.config
	if cfg.IntakeAddress == "" {
		return nil, nil
	}

	listener, err := net.Listen("tcp", cfg.IntakeAddress)
	if err != nil {
		return nil, fmt.Errorf("listening for alert intake on %s: %w", cfg.IntakeAddress, err)
	}

	endpoint := &intakeEndpoint{server: &http.Server{
		Handler: intake.Handlers{
			Placements: process.placements,
			Logger:     process.logger,
			// The adapter table is assembled beside the catalog: the composition root is
			// the one place that knows every provider.
			Adapters: intake.Adapters{
				integrations.TypeAlertmanager: alertmanager.Adapter{},
			},
		}.Router(),
		// Bounded at every stage. This is the one surface a customer's infrastructure reaches
		// inbound, and its connections are unauthenticated until a request has been read, so a
		// client that opens one and then goes quiet must not be able to hold it.
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       intakeReadTimeout,
		WriteTimeout:      intakeWriteTimeout,
		IdleTimeout:       intakeIdleTimeout,
	}}

	process.logger.Info("listening for alert intake",
		slog.String("address", listener.Addr().String()))

	go func() {
		if serveErr := endpoint.server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			failed <- serveErr
		}
	}()
	return endpoint, nil
}

// auditPruneInterval is how often each tenant's declared retention schedule is applied.
//
// Retention is measured in days, so an hour is close enough to the horizon that the surface can
// honestly say the schedule is enforced, and far enough from it that nothing is spent looking.
const auditPruneInterval = time.Hour

// startAuditPruner runs the worker that applies each tenant's audit retention schedule.
//
// It is unconditional, and that is what lets the policy surface state that retention is enforced.
// A deployment that ran the control plane without it would report a schedule it does not keep,
// which is the thing that surface was written to avoid saying.
func startAuditPruner(process assembled) *backgroundWorker {
	ctx, stop := context.WithCancel(context.Background())
	pruner := audit.Pruner{
		Retentions: process.placements,
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
		Retention: process.placements,
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

// intakeEndpoint is the alert-intake listener.
type intakeEndpoint struct {
	server   *http.Server
	stopping sync.Once
}

// stop drains intake within the budget. The nil receiver is the configured-without-intake
// case, so the caller can defer this unconditionally.
func (e *intakeEndpoint) stop(budget time.Duration, logger *slog.Logger) {
	if e == nil {
		return
	}
	e.stopping.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()

		if err := e.server.Shutdown(ctx); err != nil {
			logger.Warn("alert intake did not drain within the budget; closing it",
				slog.Duration("budget", budget))
			_ = e.server.Close()
		}
	})
}

// operatorEndpoint is the operator-facing listener.
type operatorEndpoint struct {
	server   *http.Server
	stopping sync.Once
}

// stop drains the operator surface within the budget. The nil receiver is the
// configured-without-an-operator-surface case, so the caller can defer this unconditionally.
func (e *operatorEndpoint) stop(budget time.Duration, logger *slog.Logger) {
	if e == nil {
		return
	}
	e.stopping.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()

		if err := e.server.Shutdown(ctx); err != nil {
			logger.Warn("operator surface did not drain within the budget; closing it",
				slog.Duration("budget", budget))
			_ = e.server.Close()
		}
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
