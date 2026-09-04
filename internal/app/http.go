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
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/identity"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/integrations/genericwebhook"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
)

// serve opens the listener and runs the HTTP surface until ctx is cancelled, then drains.
func serve(ctx context.Context, process assembled) error {
	cfg, logger := process.config, process.logger

	handler, err := httpRoutes(process)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       operatorReadTimeout,
		WriteTimeout:      operatorWriteTimeout,
		IdleTimeout:       operatorIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

	listener, err := net.Listen("tcp", cfg.HTTPAddress)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.HTTPAddress, err)
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
	// The backstop for the paths that return before the drain at the bottom — an endpoint
	// that refused to start must not leave this goroutine serving forever. On the ordinary
	// path the drain has already shut the server down and this is a no-op.
	defer func() { _ = server.Close() }()

	// The Relay endpoint is a second listener on purpose. It speaks a different protocol to
	// a different kind of caller, and putting it on the HTTP port would place it behind that
	// surface's middleware and expose the health surface to relays.
	relays, err := startRelayEndpoint(process, failed)
	if err != nil {
		return err
	}
	defer relays.stop(defaultShutdownTimeout, logger)

	// The callback fires only once EVERY configured surface is bound, because its promise
	// is "a test can address a port without racing the listener" — and a caller told about
	// one listener while three others are still binding would race exactly the way the
	// promise forbids.
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
	// The shutdown context is detached from the already-cancelled process context, or the
	// drain would end instantly and defeat the point.
	logger.Info("draining", slog.Duration("timeout", defaultShutdownTimeout))
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultShutdownTimeout)
	defer cancel()

	// Both surfaces drain at once, under one budget. Draining them in sequence would let a slow
	// HTTP drain spend the whole budget and leave relay sessions none of it, and would make a
	// shutdown take twice as long as it was configured to.
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

	mux.Handle("/webhooks/", intakeRouter(process))
	operatorRoutes, err := operatorRouter(process)
	if err != nil {
		return nil, err
	}
	mux.Handle("/api/", operatorRoutes)
	// Retired pre-release route families must stay absent instead of falling through to
	// the browser application's deep-link handler.
	mux.HandleFunc("/operator/", http.NotFound)
	mux.HandleFunc("/operator", http.NotFound)
	mux.HandleFunc("/intake/", http.NotFound)
	mux.HandleFunc("/intake", http.NotFound)
	return mux, nil
}

// logMigrations reports the schema effect of this start, so a deployment's schema change
// is visible without querying the database.
func logMigrations(logger *slog.Logger, applied []string) {
	if len(applied) == 0 {
		logger.Info("schema current")
		return
	}
	logger.Info("migrations applied", slog.Any("versions", applied))
}

// operatorRouter assembles the authenticated operator route table.
func operatorRouter(process assembled) (http.Handler, error) {
	cfg := process.config
	if bearing := process.catalog.CredentialBearing(); len(bearing) > 0 &&
		!process.sealer.Configured() {
		return nil, fmt.Errorf("%s is required: the catalog serves %s, which take a "+
			"credential, and this deployment has no key to seal one under",
			config.EnvSealingKeyFile, strings.Join(bearing, ", "))
	}

	identities, err := operatorIdentity(process)
	if err != nil {
		return nil, err
	}
	router, err := api.Handlers{
		Database:                process.database,
		Logger:                  process.logger,
		Identity:                identities,
		Origins:                 []string{cfg.OperatorPublicURL},
		Catalog:                 process.catalog,
		Sealer:                  process.sealer,
		Investigations:          process.investigations,
		InvestigationWindowLead: defaultInvestigationWindowLead,
		ConversationsEnabled:    true,
		MaxWaitingTurns:         cfg.MaxPendingInvestigationsPerOrganization,
		IntakeBaseURL:           cfg.OperatorPublicURL,
		PublicURL:               cfg.OperatorPublicURL,
		ConsoleURL:              cfg.OperatorPublicURL,
		MinimumRelayVersion:     "",
	}.Router()
	if err != nil {
		return nil, fmt.Errorf("assembling the operator surface: %w", err)
	}
	return router, nil
}

// operatorIdentity assembles who may reach the operator surface.
func operatorIdentity(process assembled) (identity.Handlers, error) {
	cfg := process.config
	handlers := identity.Handlers{
		Database:         process.database,
		Logger:           process.logger,
		OIDC:             identity.NewOIDC(),
		OIDCIssuer:       cfg.OIDCIssuer,
		OIDCClientID:     cfg.OIDCClientID,
		OIDCClientSecret: cfg.OIDCClientSecret,
		PublicURL:        cfg.OperatorPublicURL,
		ConsoleURL:       cfg.OperatorPublicURL,
		// This process starts the pruner unconditionally, so the policy surface may say that a
		// declared retention schedule is applied. It is passed rather than assumed because the
		// statement is made to an auditor, and the only way to keep it true is for the component
		// that starts the pruner to be the one that says it did.
		RetentionEnforced: true,
		CanCreateOrganization: func(principal authz.Principal) bool {
			return principal.Kind() == authz.KindUser
		},
	}

	handlers.Sealer = process.sealer

	if len(cfg.OperatorTokenDigest) == 0 {
		return handlers, nil
	}
	organization, err := tenancy.NewOrganization(cfg.OperatorTokenOrganization)
	if err != nil {
		return identity.Handlers{}, fmt.Errorf("bootstrap organization: %w", err)
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
			"bootstrap role", cfg.OperatorTokenRole)
	}
	handlers.Bootstrap = identity.Bootstrap{
		Digest:       cfg.OperatorTokenDigest,
		Organization: organization,
		Role:         role,
		// Named so an event produced by the bootstrap credential is distinguishable in the record.
		Name: "bootstrap credential",
	}
	process.logger.Info("bootstrap operator credential configured",
		slog.String("organization", organization.String()),
		slog.String("role", string(role)))
	return handlers, nil
}

// intakeRouter assembles authenticated Alertmanager and Slack webhook routes.
func intakeRouter(process assembled) http.Handler {
	cfg := process.config
	return webhooks.Handlers{
		Database: process.database,
		Logger:   process.logger,
		Adapters: webhooks.Adapters{
			integrations.TypeAlertmanager:   alertmanager.Adapter{},
			integrations.TypeGenericWebhook: genericwebhook.Adapter{},
		},
		Slack: slackAgent(cfg),
	}.Router()
}
