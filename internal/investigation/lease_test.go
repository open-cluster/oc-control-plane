package investigation

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// THE FENCE, FROM THE WORKER'S SIDE.
//
// A worker that has lost its lease must stop. The database half of that is asserted at the
// storage seam; this is the other half — that discovering the loss actually ends the run
// rather than merely ending the goroutine that discovered it.

// losingLeases grants the claim and then reports the lease gone on the first heartbeat,
// which is what a worker whose lease was swept and re-claimed actually sees.
type losingLeases struct {
	mu        sync.Mutex
	heartbeat chan struct{}
	beaten    bool
}

func (l *losingLeases) ClaimInvestigation(context.Context, Claim) (
	tenancy.Organization, Investigation, bool, error,
) {
	return tenancy.Organization{}, Investigation{}, false, nil
}

func (l *losingLeases) TakeLease(
	context.Context, tenancy.Organization, uuid.UUID, Claim,
) (bool, error) {
	return true, nil
}

func (l *losingLeases) Heartbeat(
	context.Context, tenancy.Organization, uuid.UUID, Claim,
) (bool, error) {
	l.mu.Lock()
	first := !l.beaten
	l.beaten = true
	l.mu.Unlock()
	if first {
		close(l.heartbeat)
	}
	return false, nil
}

func (l *losingLeases) RecoverStale(context.Context, string, int) (int, error) {
	return 0, nil
}

// A worker that discovers its lease is gone STOPS. It does not keep reading, keep spending
// and keep writing for an investigation another worker now owns.
func TestAWorkerThatLosesItsLeaseStopsRunning(t *testing.T) {
	t.Parallel()

	leases := &losingLeases{heartbeat: make(chan struct{})}

	// The tool blocks until the heartbeat has reported the lease lost, then waits on the
	// run's own context. If losing the lease does not cancel the run, this never returns
	// and the test times out — which is the bug this pins.
	reads := 0
	var readMutex sync.Mutex
	catalog := stubType(t, func(request integrations.ToolRequest) (integrations.ToolResult, error) {
		readMutex.Lock()
		reads++
		readMutex.Unlock()
		<-leases.heartbeat
		return integrations.ToolResult{Summary: "read while the lease was lost"}, nil
	})

	store := &memoryStore{candidates: []integrations.Integration{
		stubIntegration("Deploy Slack"),
	}}
	sink := &recordingSink{}
	runner := &Runner{
		Store: store, Catalog: catalog, Events: sink, Leases: leases,
		Investigator: &scriptedInvestigator{exchange: &scriptedExchange{moves: []Move{
			{Calls: []AgentCall{{ID: "call-1", Tool: "stub.read"}}},
			{Calls: []AgentCall{{ID: "call-2", Tool: "stub.read"}}},
			{Calls: []AgentCall{{ID: "call-3", Tool: "stub.read"}}},
			{Conclusion: &Conclusion{Answer: "should never be reached"}},
		}}},
		// A heartbeat this test can wait on rather than a real one.
		HeartbeatEvery: 10 * time.Millisecond,
		Logger:         slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		runner.Start(organization, Investigation{
			ID: uuid.New(), Subject: "payments latency",
			WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
		})
		runner.running.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not stop after its lease was lost; losing a lease must " +
			"cancel the work, not just the goroutine that noticed")
	}

	if store.status != StatusFailed {
		t.Errorf("status = %v; a run cut short by a lost lease ends as a failure",
			store.status)
	}

	// Exactly one terminal event, whatever happened.
	terminals := 0
	for _, event := range sink.collected() {
		if event.Type.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("%d terminal events, want exactly one", terminals)
	}
}

// When the record cannot be ended — because somebody else already ended it — NO terminal
// event is written. The event follows the record or it does not happen; a second terminal
// would tell a reader the run finished twice.
func TestNoTerminalEventIsWrittenWhenTheRecordWasAlreadyEnded(t *testing.T) {
	t.Parallel()

	store := &memoryStore{
		candidates: []integrations.Integration{stubIntegration("Deploy Slack")},
		// The sweeper got there first: the row is no longer running, so ending it is
		// refused.
		endRefused: true,
	}
	catalog := stubType(t, func(integrations.ToolRequest) (integrations.ToolResult, error) {
		return integrations.ToolResult{Summary: "1 deploy"}, nil
	})

	sink := &recordingSink{}
	runner := &Runner{
		Store: store, Catalog: catalog, Events: sink,
		Investigator: &scriptedInvestigator{
			exchange: &scriptedExchange{failure: ErrReasonerUnavailable},
		},
		Logger: slog.New(slog.DiscardHandler),
	}
	organization, err := tenancy.NewOrganization("org-test")
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(organization, Investigation{
		ID: uuid.New(), Subject: "payments latency",
		WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
	})
	runner.running.Wait()

	for _, event := range sink.collected() {
		if event.Type.Terminal() {
			t.Errorf("a terminal %s event was written for an investigation this worker "+
				"could not end; somebody else already ended it and already said so",
				event.Type)
		}
	}
}
