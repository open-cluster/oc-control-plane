package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/open-cluster/oc-control-plane/internal/api"
	"github.com/open-cluster/oc-control-plane/internal/auth/identity"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/genericwebhook"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
)

func serve(ctx context.Context, process assembled) error {
	cfg, logger := process.config, process.logger
	process.streamContext = ctx

	handler, err := httpRoutes(process)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	listener, err := net.Listen("tcp", cfg.HTTPListenAddress)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.HTTPListenAddress, err)
	}
	logger.Info("listening", slog.String("address", listener.Addr().String()))

	// One slot per surface that can report a failure. Too few would leave the last goroutines
	// blocked forever on a send nobody is left to receive.
	failed := make(chan error, 2)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, http.ErrServerClosed) {
			failed <- serveErr
			return
		}
		failed <- nil
	}()
	defer func() { _ = server.Close() }()

	// The Relay endpoint is a second listener on purpose. It speaks a different protocol to
	// a different kind of caller, and putting it on the HTTP port would place it behind that
	// surface's middleware and expose the health surface to relays.
	relays, err := startRelayEndpoint(process, failed)
	if err != nil {
		return err
	}
	defer relays.stop(defaultShutdownTimeout, logger)

	if process.onListen != nil {
		process.onListen(listener.Addr())
	}

	workerCtx, stopWorkers := context.WithCancel(ctx)
	workers, workerCtx := errgroup.WithContext(workerCtx)
	startWorkers(workerCtx, workers, process)
	defer func() {
		stopWorkers()
		_ = workers.Wait()
	}()

	select {
	case serveErr := <-failed:
		return serveErr
	case <-ctx.Done():
	}

	// Drain: stop accepting, let in-flight requests finish within the budget, then exit.
	logger.Info("draining", slog.Duration("timeout", defaultShutdownTimeout))
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultShutdownTimeout)
	defer cancel()

	var stopped sync.WaitGroup
	stopped.Go(func() {
		relays.stop(defaultShutdownTimeout, logger)
	})

	err = server.Shutdown(drainCtx)
	stopped.Wait()
	if err != nil {
		return fmt.Errorf("draining: %w", err)
	}
	<-failed
	logger.Info("stopped")
	return nil
}

// httpRoutes mounts the existing route owners behind one HTTP listener. Each owner keeps
// its own authentication, authorization, body limits, and request middleware.
func httpRoutes(process assembled) (http.Handler, error) {
	healthRouter := health.Handlers{
		Ready:   process.database.Ping,
		Metrics: process.telemetry.MetricsHandler,
		Logger:  process.logger,
	}.Router()

	mux := http.NewServeMux()
	mux.Handle("/healthz", healthRouter)
	mux.Handle("/readyz", healthRouter)
	mux.Handle("/metrics", healthRouter)

	mux.Handle("/webhooks/", webhookRouter(process))
	apiRoutes, err := apiRouter(process)
	if err != nil {
		return nil, err
	}
	mux.Handle("/api/", apiRoutes)
	return mux, nil
}

func logMigrationSummary(logger *slog.Logger, applied []string) {
	if len(applied) == 0 {
		logger.Info("schema current")
		return
	}
	logger.Info("migrations applied", slog.Any("versions", applied))
}

// apiRouter assembles the authenticated API route table.
func apiRouter(process assembled) (http.Handler, error) {
	cfg := process.config
	if bearing := process.catalog.CredentialBearing(); len(bearing) > 0 &&
		!process.sealer.Configured() {
		return nil, fmt.Errorf("%s is required: the catalog serves %s, which take a "+
			"credential, and this deployment has no key to seal one under",
			config.EnvSealingKeyFile, strings.Join(bearing, ", "))
	}

	identities, err := authHandlers(process)
	if err != nil {
		return nil, err
	}
	router, err := api.Handlers{
		Database:                process.database,
		Logger:                  process.logger,
		Identity:                identities,
		Origins:                 []string{cfg.PublicURL},
		Catalog:                 process.catalog,
		WebhookTypes:            webhookTypes(webhookAdapters()),
		Sealer:                  process.sealer,
		Investigations:          process.investigations,
		StreamContext:           process.streamContext,
		InvestigationWindowLead: defaultInvestigationWindowLead,
		MaxWaitingTurns:         cfg.MaxPendingInvestigations,
		PublicURL:               cfg.PublicURL,
	}.Router()
	if err != nil {
		return nil, fmt.Errorf("assembling the API surface: %w", err)
	}
	return router, nil
}

// authHandlers assembles authentication and identity handlers.
func authHandlers(process assembled) (identity.Handlers, error) {
	cfg := process.config
	handlers := identity.Handlers{
		Database:         process.database,
		Logger:           process.logger,
		OIDC:             identity.NewOIDC(),
		OIDCIssuer:       cfg.OIDCIssuer,
		OIDCClientID:     cfg.OIDCClientID,
		OIDCClientSecret: cfg.OIDCClientSecret,
		PublicURL:        cfg.PublicURL,
		SessionLifetime:  cfg.SessionLifetime,
	}

	handlers.Sealer = process.sealer

	if len(cfg.BootstrapTokenDigest) == 0 {
		return handlers, nil
	}
	handlers.Bootstrap = identity.Bootstrap{Digest: cfg.BootstrapTokenDigest}
	process.logger.Info("one-time bootstrap credential configured")
	return handlers, nil
}

// webhookRouter assembles authenticated Alertmanager and Slack webhook routes.
func webhookRouter(process assembled) http.Handler {
	cfg := process.config
	return webhooks.Handlers{
		Database: process.database,
		Logger:   process.logger,
		Adapters: webhookAdapters(),
		Slack:    newSlackAgent(cfg),
	}.Router()
}

func webhookAdapters() webhooks.Adapters {
	return webhooks.Adapters{
		"alertmanager":    alertmanager.Adapter{},
		"generic_webhook": genericwebhook.Adapter{},
	}
}

func webhookTypes(adapters webhooks.Adapters) map[integrations.Provider]bool {
	types := make(map[integrations.Provider]bool, len(adapters))
	for provider := range adapters {
		types[provider] = true
	}
	return types
}
