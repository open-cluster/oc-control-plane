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
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/observability"
	"github.com/open-cluster/oc-control-plane/internal/operator"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/storage"
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
	return run(ctx, cfg, os.Stderr, nil)
}

// run assembles and serves the control plane until ctx is cancelled, then drains within
// the configured timeout. It returns nil on a clean shutdown.
//
// onListen, when non-nil, receives the bound address once the listener is open. Production
// passes nil; tests pass a callback so they can address an ephemeral port without racing
// the listener.
func run(
	ctx context.Context, cfg config.Config, logOutput io.Writer, onListen func(net.Addr),
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

	return serve(ctx, assembled{
		config:     cfg,
		logger:     logger,
		telemetry:  telemetry,
		placements: placements,
		onListen:   onListen,
	})
}

// assembled is the constructed process: the pieces serve needs, which are meaningless
// apart and always travel together.
type assembled struct {
	config     config.Config
	logger     *slog.Logger
	telemetry  *observability.Telemetry
	placements *storage.Placements
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
		sessions: relay.NewSessionService(process.placements, process.logger),
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

	listener, err := net.Listen("tcp", cfg.OperatorAddress)
	if err != nil {
		return nil, fmt.Errorf("listening for operators on %s: %w", cfg.OperatorAddress, err)
	}

	endpoint := &operatorEndpoint{server: &http.Server{
		Handler: operator.Handlers{
			Placements:  process.placements,
			Logger:      process.logger,
			TokenDigest: cfg.OperatorTokenDigest,
		}.Router(),
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
