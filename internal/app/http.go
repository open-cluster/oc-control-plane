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

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/errgroup"

	"github.com/open-cluster/oc-control-plane/internal/api"
	"github.com/open-cluster/oc-control-plane/internal/auth/identity"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/correlation"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/genericwebhook"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
	"github.com/open-cluster/oc-control-plane/internal/telemetry"
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

	mux.Handle("/webhooks/", webhookSurface(process))
	apiRoutes, err := apiRouter(process)
	if err != nil {
		return nil, err
	}
	mux.Handle("/api/", apiRoutes)
	return mux, nil
}

func webhookSurface(process assembled) http.Handler {
	logged := observability.HTTPRequestLogger(process.logger, webhookRouter(process))
	return correlation.Middleware(otelhttp.NewHandler(logged, "webhooks"))
}

func logMigrationSummary(logger *slog.Logger, applied []string) {
	if len(applied) == 0 {
		logger.Info("schema current")
		return
	}
	logger.Info("migrations applied", slog.Any("versions", applied))
}

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
	protected, err := api.Handlers{
		Database:                process.database,
		Logger:                  process.logger,
		Identity:                identities,
		Origin:                  cfg.PublicURL,
		Catalog:                 process.catalog,
		WebhookTypes:            webhookTypes(webhookAdapters()),
		Sealer:                  process.sealer,
		Investigations:          process.investigations,
		AgentAvailable:          process.agentAvailable(),
		StreamContext:           process.streamContext,
		InvestigationWindowLead: defaultInvestigationWindowLead,
		MaxWaitingTurns:         cfg.MaxPendingInvestigations,
		PublicURL:               cfg.PublicURL,
	}.Router()
	if err != nil {
		return nil, fmt.Errorf("assembling the API surface: %w", err)
	}
	authentication := identities.Authentication(cfg.PublicURL)
	mux := http.NewServeMux()
	mux.Handle("POST "+identity.Base+"/auth/local/bootstrap", authentication.LocalBootstrap)
	mux.Handle("POST "+identity.Base+"/auth/local/sign-in", authentication.LocalSignIn)
	mux.Handle("GET "+identity.Base+"/auth/oidc/start", authentication.OIDCStart)
	mux.Handle("GET "+identity.Base+"/auth/oidc/callback", authentication.OIDCCallback)
	mux.Handle("DELETE "+identity.Base+"/session", authentication.SignOut)
	mux.Handle("/api/", protected)
	return correlation.Middleware(mux), nil
}

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

	if len(cfg.BootstrapTokenDigest) == 0 {
		return handlers, nil
	}
	handlers.Bootstrap = identity.Bootstrap{Digest: cfg.BootstrapTokenDigest}
	process.logger.Info("one-time bootstrap credential configured")
	return handlers, nil
}

func webhookRouter(process assembled) http.Handler {
	cfg := process.config
	agentAvailable := process.agentAvailable()
	return webhooks.Handlers{
		Database: process.database,
		Logger:   process.logger,
		Adapters: webhookAdapters(),
		Slack:    newSlackAgent(cfg, agentAvailable),
		AlertAdmission: storage.AlertAdmissionPolicy{
			AgentAvailable: agentAvailable,
			WindowLead:     defaultInvestigationWindowLead,
			MaximumPending: cfg.MaxPendingInvestigations,
		},
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
