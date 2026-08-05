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

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/health"
	"github.com/open-cluster/oc-control-plane/internal/identity"
	"github.com/open-cluster/oc-control-plane/internal/intake"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/observability"
	"github.com/open-cluster/oc-control-plane/internal/operator"
	"github.com/open-cluster/oc-control-plane/internal/reasoning"
	"github.com/open-cluster/oc-control-plane/internal/reasoning/providers"
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

// wiring is what a test may put in place of the real thing. There is exactly one entry, and it is
// the model boundary — the only component in this program that is faked in CI, for the reasons
// recorded at the seam itself in internal/investigation/reasoner.go.
//
// Production supplies nothing here. With no reasoner configured the investigator is wired to the
// provider-unavailable boundary, so a deployment that has not been given one produces honest failed
// rounds saying the reasoning step could not run — rather than starting, looking ready, and
// abstaining on every case for a reason nobody can find.
type wiring struct {
	onListen func(net.Addr)
	reasoner investigation.Reasoner
	// investigatorInterval overrides how often the worker looks for work. Production polls at a
	// rate suited to work that arrives a few times an hour; a test that waited that out would spend
	// most of its wall clock asleep.
	investigatorInterval time.Duration
	// controls tightens what a round runs under. A test that had to wait out the product's real
	// bounds would either be slow or would assert against bounds nobody ships; this lets it assert
	// the exhaustion path in seconds against the same code the real bounds run through. Zero fields
	// mean the product's own defaults, which is what production uses.
	controls investigation.Controls
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

	reasoner, versions, err := modelBoundary(cfg, logger, replace)
	if err != nil {
		return err
	}
	if service, live := reasoner.(*reasoning.Service); live {
		logger.Info("model provider configured", slog.String("chain", service.Describe()))
	}

	claimInterval := investigatorInterval
	if replace.investigatorInterval > 0 {
		claimInterval = replace.investigatorInterval
	}

	return serve(ctx, assembled{
		config:     cfg,
		controls:   investigation.DefaultControls().Restrict(replace.controls),
		interval:   claimInterval,
		logger:     logger,
		telemetry:  telemetry,
		placements: placements,
		reasoner:   reasoner,
		versions:   versions,
		onListen:   replace.onListen,
	})
}

// modelBoundary resolves what will answer the reasoning step.
//
// A test's own boundary wins, then a live provider, then a recorded transcript the deployment was
// given, and with none of them the boundary is the provider-unavailable one. Failing closed is
// deliberate: an instance that cannot reason must say so on every round rather than look healthy
// and abstain for a reason nobody can find.
//
// A live provider outranks a recorded transcript because a deployment that configured one asked for
// a real model, and replaying a recording at it would answer a different question than the one it
// was pointed at.
//
// A transcript that does not load, or a provider deployment that could not be assembled, is a
// REFUSAL TO START. An operator who pointed at one and got a control plane reasoning about nothing
// would have no way to tell that from one they never configured, and this is the last moment anyone
// can be told.
func modelBoundary(
	cfg config.Config, logger *slog.Logger, replace wiring,
) (investigation.Reasoner, investigation.Versions, error) {
	if replace.reasoner != nil {
		return replace.reasoner, investigatorVersions(replace.reasoner), nil
	}
	if cfg.Model.Configured() {
		service, err := liveBoundary(cfg, logger)
		if err != nil {
			return nil, investigation.Versions{}, err
		}
		return service, service.Versions(plannerVersion, version), nil
	}
	if len(cfg.ModelTranscript) == 0 {
		return investigation.Unavailable{}, investigatorVersions(investigation.Unavailable{}), nil
	}

	versions := investigatorVersions(nil)
	recorded, err := investigation.LoadTranscript(cfg.ModelTranscript, versions)
	if err != nil {
		return nil, investigation.Versions{}, fmt.Errorf(
			"%s: %w", config.EnvModelTranscriptFile, err)
	}
	return recorded, versions, nil
}

// liveBoundary assembles the reasoning service from the configured deployments.
//
// Everything that could be wrong with the configuration is refused here, at startup, where the
// person who wrote it is still the person reading the error: an unknown provider, an unpriced
// model, a provider nobody consented to, an effort level that does not exist.
func liveBoundary(cfg config.Config, logger *slog.Logger) (*reasoning.Service, error) {
	primary := deploymentFrom(cfg.Model)
	primaryProvider, err := providers.Open(primary, providers.Options{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvModelProvider, err)
	}

	options := reasoning.Options{
		Primary:     primaryProvider,
		Deployments: []reasoning.Deployment{primary},
		Tariff:      reasoning.DefaultTariff(),
		Consent:     reasoning.ConsentTo(cfg.ModelConsented...),
		Ceiling:     reasoning.NewCeiling(cfg.ModelCostCeilingMicroCents),
		// Without this the service holds a discard logger, and every completed call — the
		// attribution, the token counts, the cost — goes nowhere. A telemetry requirement met by a
		// logger nobody passed is a requirement met on paper.
		Logger: logger,
	}
	if cfg.ModelFallback.Configured() {
		fallback := deploymentFrom(cfg.ModelFallback)
		fallbackProvider, fallbackErr := providers.Open(fallback, providers.Options{})
		if fallbackErr != nil {
			return nil, fmt.Errorf("%s: %w", config.EnvModelFallbackProvider, fallbackErr)
		}
		options.Fallbacks = append(options.Fallbacks, fallbackProvider)
		options.Deployments = append(options.Deployments, fallback)
	}
	return reasoning.New(options)
}

// deploymentFrom translates configuration into the reasoning package's own shape. The credential
// becomes a Secret here, which is the last point at which it is an ordinary string.
func deploymentFrom(configured config.ModelDeployment) reasoning.Deployment {
	return reasoning.Deployment{
		Provider:        configured.Provider,
		Model:           configured.Model,
		Effort:          reasoning.Effort(configured.Effort),
		MaxOutputTokens: configured.MaxOutputTokens,
		MaxPromptTokens: configured.MaxPromptTokens,
		BaseURL:         configured.BaseURL,
		Credential:      reasoning.Secret(configured.Credential),
	}.WithDefaults()
}

// assembled is the constructed process: the pieces serve needs, which are meaningless
// apart and always travel together.
type assembled struct {
	config     config.Config
	logger     *slog.Logger
	telemetry  *observability.Telemetry
	placements *storage.Placements
	reasoner   investigation.Reasoner
	// controls is what a round opened through this process runs under.
	controls investigation.Controls
	// interval is how often the investigator looks for work.
	interval time.Duration
	// versions is what every round this build opens is stamped with, so a recorded transcript made
	// for different components is detected rather than silently replayed.
	versions investigation.Versions
	onListen func(net.Addr)
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

	// The investigator is not a listener: it claims durable work rather than answering a caller.
	// It stops with the process context and finishes what it holds, and anything it cannot finish
	// stays leased until the lease runs out and another instance takes it — which is what makes a
	// deploy in the middle of an investigation recoverable rather than a lost run.
	investigators := startInvestigator(process)
	defer investigators.stop()

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
		Controls:            process.controls,
		Versions:            process.versions,
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
		named = string(authz.OrganizationOwner)
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

// Bounds on the investigator. They are conservative on purpose: a worker that claims more than it
// can run holds leases nothing is executing, and a poll interval short enough to feel instant is a
// query per instance per second forever for work that arrives a few times an hour.
const (
	investigatorInterval = 2 * time.Second
	investigatorLease    = 2 * time.Minute
	investigatorCapacity = 4
)

// investigatorVersions is what every round this build opens is pinned with. The investigator's
// version is the binary's, so a case read months later names the build that produced it.
//
// A nil reasoner asks for the versions a RECORDING must have been made against, which is what a
// transcript is checked against before it is accepted. The check has to happen before the boundary
// exists, so it cannot ask the boundary what it is.
func investigatorVersions(reasoner investigation.Reasoner) investigation.Versions {
	model := "recorded"
	if reasoner != nil {
		model = modelIdentity(reasoner)
	}
	return investigation.Versions{
		Planner: plannerVersion,
		Model:   model,
		// The prompt and schema versions belong to the package that owns the words and the output
		// contract, so that changing either is a diff in one place that forces the number up. A
		// recorded transcript is keyed on them exactly as a live round is.
		PromptVersion: reasoning.PromptVersion,
		SchemaVersion: reasoning.SchemaVersion,
		Investigator:  version,
	}
}

// plannerVersion names the planning strategy every round this build opens runs under.
const plannerVersion = "bounded-adaptive-v1"

// modelIdentity names the boundary in use, so a round's pinned versions say what actually answered
// it rather than what the configuration hoped would.
func modelIdentity(reasoner investigation.Reasoner) string {
	if _, unavailable := reasoner.(investigation.Unavailable); unavailable {
		return "unavailable"
	}
	// Everything else is a recording, in this build. A live provider names itself here when there
	// is one, and the round's pinned versions then say which model actually answered it.
	return "recorded"
}

// startInvestigator runs the worker that claims investigation rounds and executes them.
func startInvestigator(process assembled) *investigatorWorker {
	ctx, stop := context.WithCancel(context.Background())
	worker := investigation.Worker{
		Runner: investigation.Runner{
			Store:    process.placements,
			Reasoner: process.reasoner,
			Logger:   process.logger,
			Versions: process.versions,
		},
		Store:     process.placements,
		Pending:   process.placements,
		Logger:    process.logger,
		Interval:  process.interval,
		LeaseFor:  investigatorLease,
		Capacity:  investigatorCapacity,
		SessionID: uuid.New(),
	}

	running := &investigatorWorker{stopping: stop, done: make(chan struct{})}
	go func() {
		defer close(running.done)
		worker.Run(ctx)
	}()
	process.logger.Info("investigator started",
		slog.String("session_id", worker.SessionID.String()),
		slog.String("model", process.versions.Model))
	return running
}

// investigatorWorker is the running worker and the handle that stops it.
type investigatorWorker struct {
	stopping context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

// stop asks the worker to finish and waits for it. It is deliberately unbudgeted: what it is
// waiting for is rounds recording the outcome they reached, and cutting that short would leave a
// case reading as running with nothing working on it until its lease expired.
func (w *investigatorWorker) stop() {
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
