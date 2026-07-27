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
	"syscall"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/api"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/observability"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// version is stamped at release build time via -ldflags; "dev" otherwise.
var version = "dev"

// readHeaderTimeout bounds how long a client may take to send its headers, which is the
// cheapest defence against a slow-loris holding connections open.
const readHeaderTimeout = 10 * time.Second

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

	handlers := api.Handlers{
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

	failed := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			failed <- serveErr
			return
		}
		failed <- nil
	}()

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

	if err := server.Shutdown(drainCtx); err != nil {
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
