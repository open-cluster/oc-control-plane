package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/identity"
	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/alertmanager"
	"github.com/open-cluster/oc-control-plane/internal/operator"
	"github.com/open-cluster/oc-control-plane/internal/relay"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

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

	// The callback fires only once EVERY configured surface is bound, because its promise
	// is "a test can address a port without racing the listener" — and a caller told about
	// one listener while three others are still binding would race exactly the way the
	// promise forbids.
	if process.onListen != nil {
		process.onListen(listener.Addr())
	}

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

	// Answering back in Slack. Started only where this deployment receives events at all,
	// because a worker sweeping for deliveries nobody can create is a query on a loop.
	slackReplies := startSlackReplies(process)
	defer slackReplies.stop()

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

	// The operator surface is where credentials enter and are used, so a deployment
	// serving a credential-bearing catalog without a sealing key is refused HERE, at
	// startup: the alternative is a setup flow that accepts a token it can only store in
	// the clear or drop.
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
	// The route table becomes a mux HERE, before the listener opens, so a route that cannot be
	// authorized correctly is a process that refuses to start rather than a route that is
	// served open. That is the runtime half of "a new route without a declared permission
	// cannot ship"; the compile-time half is that authz.Privileged takes the permission
	// positionally, and the gate in internal/gates is the third.
	router, err := operator.Handlers{
		Placements:              process.placements,
		Logger:                  process.logger,
		Identity:                identities,
		Origins:                 cfg.OperatorAllowedOrigins,
		Catalog:                 process.catalog,
		Sealer:                  process.sealer,
		Investigations:          process.investigations,
		InvestigationWindowLead: cfg.InvestigationWindowLead,
		ConversationsEnabled:    cfg.ConversationsEnabled,
		MaxWaitingTurns:         cfg.OrgWaitingInvestigations,
		IntakeBaseURL:           cfg.IntakePublicURL,
		PublicURL:               cfg.OperatorPublicURL,
		ConsoleURL:              cfg.OperatorConsoleURL,
		MinimumRelayVersion:     cfg.MinimumRelayVersion,
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

	handlers.Sealer = process.sealer

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
			// The Slack agent surface, served only where this deployment holds a signing
			// secret. A deployment with none serves no events endpoint at all rather than
			// one that refuses everything: an endpoint that exists and refuses is a
			// configuration to check, and one that does not exist is a deployment nobody
			// asked to receive events.
			Slack: slackAgent(cfg),
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
