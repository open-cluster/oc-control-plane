package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/relay"
)

type relayEndpoint struct {
	server   *grpc.Server
	sessions *relay.SessionService
	stopping sync.Once
}

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
		sessions: relay.NewSessionService(process.database, process.logger, process.inventoryInterval),
	}
	relayv1.RegisterRelayRegistrationServiceServer(endpoint.server,
		relay.NewRegistrationService(process.database, cfg.RelaySPKIPins, process.logger))
	relayv1.RegisterRelaySessionServiceServer(endpoint.server, endpoint.sessions)

	process.logger.Info("listening for relays", slog.String("address", listener.Addr().String()))
	go func() {
		if serveErr := endpoint.server.Serve(listener); serveErr != nil &&
			!errors.Is(serveErr, grpc.ErrServerStopped) {
			failed <- serveErr
		}
	}()
	return endpoint, nil
}

func (e *relayEndpoint) stop(budget time.Duration, logger *slog.Logger) {
	if e == nil {
		return
	}
	e.stopping.Do(func() { e.drain(budget, logger) })
}

func (e *relayEndpoint) drain(budget time.Duration, logger *slog.Logger) {
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
