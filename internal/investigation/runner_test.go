package investigation

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type runnerStore struct {
	mu            sync.Mutex
	queue         []Investigation
	drained       []uuid.UUID
	status        Status
	drainAttempts chan struct{}
}

func (s *runnerStore) ClaimInvestigation(_ context.Context, _ Claim) (
	tenancy.Organization, Investigation, bool, error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return tenancy.Organization{}, Investigation{}, false, nil
	}
	opened := s.queue[0]
	s.queue = s.queue[1:]
	organization, _ := tenancy.NewOrganization("org-test")
	return organization, opened, true, nil
}

func (*runnerStore) Heartbeat(context.Context, tenancy.Organization, uuid.UUID, Claim) (bool, error) {
	return true, nil
}

func (*runnerStore) RecoverStale(context.Context, string, int) (int, error) { return 0, nil }

func (s *runnerStore) Investigation(
	context.Context, tenancy.Organization, uuid.UUID,
) (Investigation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status
	if status == 0 {
		status = StatusRunning
	}
	return Investigation{Status: status}, nil
}

func (s *runnerStore) DrainConversation(
	_ context.Context, _ tenancy.Organization, id uuid.UUID, _ time.Duration, _ int,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drained = append(s.drained, id)
	return true, nil
}

func (s *runnerStore) DrainQueuedConversation(context.Context, time.Duration, int) (bool, error) {
	if s.drainAttempts != nil {
		select {
		case s.drainAttempts <- struct{}{}:
		default:
		}
	}
	return false, nil
}

func TestRunnerRetriesDurableQueuedConversations(t *testing.T) {
	store := &runnerStore{drainAttempts: make(chan struct{}, 1)}
	runner := &Runner{
		Store: store, Agent: concludingAgent{}, Workers: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telemetry: &Telemetry{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	select {
	case <-store.drainAttempts:
	case <-time.After(2 * time.Second):
		t.Fatal("Runner did not retry durable queued Conversation work")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Runner did not stop after cancellation")
	}
}

type blockingAgent struct {
	started chan struct{}
	mu      sync.Mutex
	active  int
	maximum int
}

func (a *blockingAgent) Run(
	ctx context.Context, _ tenancy.Organization, _ Investigation,
) error {
	a.mu.Lock()
	a.active++
	if a.active > a.maximum {
		a.maximum = a.active
	}
	a.mu.Unlock()
	a.started <- struct{}{}
	<-ctx.Done()
	a.mu.Lock()
	a.active--
	a.mu.Unlock()
	return ctx.Err()
}

func TestRunnerDefaultsToEightConcurrentWorkersAndWaitsForShutdown(t *testing.T) {
	store := &runnerStore{}
	for range 9 {
		store.queue = append(store.queue, Investigation{ID: uuid.New()})
	}
	agent := &blockingAgent{started: make(chan struct{}, 9)}
	runner := &Runner{
		Store: store, Agent: agent, Worker: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	for range 8 {
		select {
		case <-agent.started:
		case <-time.After(time.Second):
			t.Fatal("all eight workers did not start")
		}
	}
	select {
	case <-agent.started:
		t.Fatal("a ninth investigation started above the worker limit")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Runner did not wait for its agents to stop")
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.maximum != 8 || agent.active != 0 {
		t.Fatalf("maximum=%d active=%d, want maximum 8 and no active work", agent.maximum, agent.active)
	}
}

type concludingAgent struct{}

func (concludingAgent) Run(context.Context, tenancy.Organization, Investigation) error { return nil }

type unterminatedAgent struct{}

func (unterminatedAgent) Run(context.Context, tenancy.Organization, Investigation) error {
	return context.DeadlineExceeded
}

func TestRunnerDrainsAConversationAfterTheAgentFinishes(t *testing.T) {
	conversationID := uuid.New()
	store := &runnerStore{}
	runner := &Runner{
		Store: store, Agent: concludingAgent{}, Worker: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	organization, _ := tenancy.NewOrganization("org-test")
	runner.runClaimed(context.Background(), organization,
		Investigation{ID: uuid.New(), ConversationID: conversationID})
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.drained) != 1 || store.drained[0] != conversationID {
		t.Fatalf("drained = %v, want %s", store.drained, conversationID)
	}
}

func TestRunnerDoesNotDrainAfterATerminalWriteFailure(t *testing.T) {
	store := &runnerStore{}
	runner := &Runner{
		Store: store, Agent: unterminatedAgent{}, Worker: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	organization, _ := tenancy.NewOrganization("org-test")
	runner.runClaimed(context.Background(), organization,
		Investigation{ID: uuid.New(), ConversationID: uuid.New()})
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.drained) != 0 {
		t.Fatalf("drained an unterminated Conversation: %v", store.drained)
	}
}

func TestRunnerStopsActiveAgentAfterRemoteCancellation(t *testing.T) {
	store := &runnerStore{}
	agent := &blockingAgent{started: make(chan struct{}, 1)}
	runner := &Runner{
		Store: store, Agent: agent, Worker: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	organization, _ := tenancy.NewOrganization("org-test")
	done := make(chan struct{})
	go func() {
		runner.runClaimed(context.Background(), organization, Investigation{ID: uuid.New()})
		close(done)
	}()
	select {
	case <-agent.started:
	case <-time.After(time.Second):
		t.Fatal("Agent did not start")
	}
	store.mu.Lock()
	store.status = StatusCancelled
	store.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("remote cancellation did not stop the Agent")
	}
}
