package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// The session stream is a delivery channel and nothing more: every guarantee about a job not
// being lost or completed twice lives in the database. So what is asserted here is what a
// relay could observe of that guarantee — work waiting through an outage arrived, an outcome
// was acknowledged only once recorded, a resend was answered definitively, and a result from
// a lease that no longer owns the job was refused. How the server reaches those answers is
// deliberately not asserted, so the implementation can change without rewriting this.
//
// Durability is proven the same way a relay proves it: by resending. An acknowledgement of
// "already recorded" can only come from a committed row, so it is stronger evidence than
// reading the database from the test would be, and it uses nothing a relay cannot see.
func TestRelaySession(t *testing.T) {
	// The organization the harness assigns a placement to. An unassigned one is refused
	// exactly like a bad credential, which makes a wrong name here look like a session defect.
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	relay := registerRelay(t, connection, placementDSN, organization)
	placements := openPlacement(t, placementDSN)
	owner := namedOrganization(t, organization)

	// Enqueued before anything is connected. An outage must delay an investigation, never
	// lose it, so this job has to be waiting when the session arrives.
	waiting := enqueueJob(t, placements, owner, relay.registration, workloadArguments("payments"))
	fenced := enqueueJob(t, placements, owner, relay.registration, workloadArguments("checkout"))

	stream := connectSession(t, connection, organization, relay)

	var session string
	t.Run("the server mints the session identity", func(t *testing.T) {
		accepted := awaitSessionAccepted(t, stream)
		session = accepted.GetSessionId()
		if session == "" {
			t.Fatal("no session identity was issued; nothing could own a lease")
		}
		if accepted.GetHeartbeatInterval().AsDuration() <= 0 {
			t.Error("no heartbeat interval was declared; the relay cannot tell a quiet " +
				"session from a dead one")
		}
	})

	assignments := map[string]*relayv1.JobAssignment{}
	t.Run("work waiting through an outage is delivered on connect", func(t *testing.T) {
		for range 2 {
			assignment := awaitAssignment(t, stream)
			assignments[assignment.GetJobId()] = assignment
		}
		if _, ok := assignments[waiting.String()]; !ok {
			t.Fatalf("job %v was enqueued before the session and never arrived", waiting)
		}
		if _, ok := assignments[fenced.String()]; !ok {
			t.Fatalf("job %v was enqueued before the session and never arrived", fenced)
		}
	})

	t.Run("an assignment carries everything needed to execute it", func(t *testing.T) {
		assignment := assignments[waiting.String()]
		if assignment == nil {
			t.Skip("the assignment never arrived")
		}
		if assignment.GetOrgId() != organization {
			t.Errorf("assigned to organization %q, want %q", assignment.GetOrgId(), organization)
		}
		if assignment.GetRegistrationId() != relay.registration.String() {
			t.Errorf("assigned to registration %q, want %q",
				assignment.GetRegistrationId(), relay.registration)
		}
		if assignment.GetCapabilityId() != capabilityUnderTest {
			t.Errorf("assigned capability %q, want %q",
				assignment.GetCapabilityId(), capabilityUnderTest)
		}
		if assignment.GetCapabilityVersion() != capabilityVersionUnderTest {
			t.Errorf("assigned capability version %d, want %d — a relay chooses its "+
				"implementation by version and cannot execute an unversioned assignment",
				assignment.GetCapabilityVersion(), capabilityVersionUnderTest)
		}
		if assignment.GetLeaseEpoch() == 0 {
			t.Error("assigned lease generation 0; the result would echo a fence that never held")
		}
		if assignment.GetDeadlineBudget().AsDuration() <= 0 {
			t.Error("assigned no execution budget; the relay would run unbounded against a " +
				"lease that expires without it")
		}
		if assignment.GetIdempotencyKey() == "" {
			t.Error("assigned no idempotency key; a redelivery would be indistinguishable " +
				"from new work")
		}

		arguments := assignment.GetArguments().GetKubernetesWorkloadRuntimeV1()
		if arguments.GetWorkloadName() != "payments" {
			t.Errorf("arguments arrived as %q, want the enqueued %q — a job executed against "+
				"the wrong workload is worse than one never delivered",
				arguments.GetWorkloadName(), "payments")
		}
	})

	t.Run("a result is acknowledged as recorded", func(t *testing.T) {
		assignment := assignments[waiting.String()]
		if assignment == nil {
			t.Skip("the assignment never arrived")
		}
		sendResult(t, stream, assignment.GetJobId(), assignment.GetLeaseEpoch())

		acknowledged := awaitResultAck(t, stream)
		if acknowledged.GetJobId() != assignment.GetJobId() {
			t.Fatalf("acknowledged job %q, want %q", acknowledged.GetJobId(), assignment.GetJobId())
		}
		if acknowledged.GetDisposition() != relayv1.ResultAck_DISPOSITION_RECORDED {
			t.Fatalf("acknowledged as %v, want recorded", acknowledged.GetDisposition())
		}
	})

	t.Run("a resent result is answered definitively rather than recorded twice", func(t *testing.T) {
		assignment := assignments[waiting.String()]
		if assignment == nil {
			t.Skip("the assignment never arrived")
		}
		// A relay that never saw the first acknowledgement resends. Being told the outcome
		// already exists is the only answer that drains its buffer, and it can only come
		// from a committed row.
		sendResult(t, stream, assignment.GetJobId(), assignment.GetLeaseEpoch())

		acknowledged := awaitResultAck(t, stream)
		if acknowledged.GetDisposition() != relayv1.ResultAck_DISPOSITION_ALREADY_RECORDED {
			t.Errorf("a resend was acknowledged as %v, want already recorded — anything else "+
				"either loses the outcome or records it twice", acknowledged.GetDisposition())
		}
	})

	t.Run("a result under a lease that no longer owns the job is refused", func(t *testing.T) {
		assignment := assignments[fenced.String()]
		if assignment == nil {
			t.Skip("the assignment never arrived")
		}
		// The generation before the one that was assigned belongs to no execution that ever
		// held this job. A relay resending across a lease change produces exactly this.
		sendResult(t, stream, assignment.GetJobId(), assignment.GetLeaseEpoch()-1)

		acknowledged := awaitResultAck(t, stream)
		if acknowledged.GetDisposition() != relayv1.ResultAck_DISPOSITION_STALE_STOP_RESENDING {
			t.Errorf("a superseded result was acknowledged as %v, want stale — recording it "+
				"would overwrite the execution that owns the job now",
				acknowledged.GetDisposition())
		}
	})

	t.Run("work enqueued during a session is delivered without reconnecting", func(t *testing.T) {
		later := enqueueJob(t, placements, owner, relay.registration, workloadArguments("search"))

		assignment := awaitAssignment(t, stream)
		if assignment.GetJobId() != later.String() {
			t.Fatalf("delivered job %q, want the newly enqueued %v",
				assignment.GetJobId(), later)
		}
	})

	t.Run("a job that cannot be expressed does not strand the work behind it", func(t *testing.T) {
		// The arguments were written by the control plane, so this can only happen through a
		// defect here — but one undeliverable job holding up every job behind it turns a
		// defect into an outage, which is the part that must not happen.
		unexpressable := enqueueJob(t, placements, owner, relay.registration,
			[]byte{0x0a, 0x05}) // A length-delimited field claiming five bytes that follow nothing.
		behind := enqueueJob(t, placements, owner, relay.registration, workloadArguments("billing"))

		assignment := awaitAssignment(t, stream)
		if assignment.GetJobId() != behind.String() {
			t.Fatalf("delivered job %q, want %v — the job behind %v must not wait for it",
				assignment.GetJobId(), behind, unexpressable)
		}
	})

	t.Run("a job asked to stop is told once and still reports its own outcome", func(t *testing.T) {
		job := enqueueJob(t, placements, owner, relay.registration, workloadArguments("ledger"))
		assignment := awaitAssignment(t, stream)
		if assignment.GetJobId() != job.String() {
			t.Fatalf("delivered job %q, want %v", assignment.GetJobId(), job)
		}

		asked, err := placements.RequestJobCancellation(context.Background(), owner, job)
		if err != nil {
			t.Fatalf("asking the job to stop: %v", err)
		}
		if asked != storage.CancellationRequested {
			t.Fatalf("asking an executing job to stop gave %v, want it requested", asked)
		}

		cancellation := awaitCancellation(t, stream)
		if cancellation.GetJobId() != job.String() ||
			cancellation.GetLeaseEpoch() != assignment.GetLeaseEpoch() {
			t.Fatalf("asked to stop job %q at generation %d, want %v at %d — a relay cannot "+
				"tell which execution it is being asked to stop otherwise",
				cancellation.GetJobId(), cancellation.GetLeaseEpoch(),
				job, assignment.GetLeaseEpoch())
		}

		acknowledgeCancellation(t, stream, cancellation)

		// Long enough for several delivery rounds to pass. A stop repeated on every round
		// would be noise on a stream that carries real work, and a long execution would
		// receive hundreds of them.
		time.Sleep(12 * time.Second)

		// The terminal outcome still comes from the relay. A stop that recorded the outcome
		// itself would be deciding the fate of an execution it cannot see — one that may well
		// have finished before the request arrived.
		sendCancelledResult(t, stream, job.String(), assignment.GetLeaseEpoch())

		repeats := 0
		for {
			message, recvErr := stream.Recv()
			if recvErr != nil {
				t.Fatalf("waiting for the result to be acknowledged: %v", recvErr)
			}
			if message.GetCancellation() != nil {
				repeats++
			}
			if acknowledged := message.GetResultAck(); acknowledged != nil {
				if acknowledged.GetDisposition() != relayv1.ResultAck_DISPOSITION_RECORDED {
					t.Errorf("the outcome of a cancelled job was acknowledged as %v, want "+
						"recorded", acknowledged.GetDisposition())
				}
				break
			}
		}
		if repeats != 0 {
			t.Errorf("the stop was repeated %d times while the job was still executing", repeats)
		}
	})

	// The two subtests below each register their own relay, because both end a session on
	// purpose and would otherwise take the one the assertions above depend on with them.

	t.Run("a reconnection ends the session it replaces", func(t *testing.T) {
		reconnecting := registerRelay(t, connection, placementDSN, organization)

		replaced := connectSession(t, connection, organization, reconnecting)
		awaitSessionAccepted(t, replaced)

		successor := connectSession(t, connection, organization, reconnecting)
		awaitSessionAccepted(t, successor)

		// A relay has one session. Left running, the session it reconnected away from would go
		// on claiming work it can no longer receive, and hold it for the length of a lease.
		//
		// The elapsed bound is what makes this an assertion rather than a formality: the
		// client's own deadline would eventually end the stream too, and a test that accepted
		// any error would pass against a server that never supersedes anything.
		started := time.Now()
		told, err := awaitReconnectInstruction(t, replaced)
		waited := time.Since(started)

		if reported, ok := status.FromError(err); !ok || reported.Code() != codes.Aborted {
			t.Fatalf("the replaced session ended with %v, want Aborted", err)
		}
		if waited > 20*time.Second {
			t.Errorf("the replaced session took %v to end; the relay is already gone", waited)
		}

		// Being displaced is told, not merely done. A relay that is dropped without explanation
		// re-enters its backoff and goes quiet — and a relay displaced by something holding its
		// credential must come back rather than assume it was meant to stop.
		if told == nil {
			t.Fatal("the replaced session was closed without being told to reconnect")
		}
		if told.GetRetryAfter().AsDuration() <= 0 {
			t.Error("the reconnect instruction carries no delay, so two relays holding the " +
				"same identity would displace each other as fast as they can connect")
		}
	})

	t.Run("a session that stops proving it is alive is ended", func(t *testing.T) {
		silent := registerRelay(t, connection, placementDSN, organization)

		stream := connectSession(t, connection, organization, silent)
		awaitSessionAccepted(t, stream)

		// This waits out the real idle allowance rather than a shortened one. A relay behind a
		// half-open connection looks exactly like this, and the allowance is the only thing
		// that distinguishes it from a quiet but healthy relay — so it is the thing under test.
		started := time.Now()
		_, err := stream.Recv()
		waited := time.Since(started)

		if err == nil {
			t.Fatal("a silent session was never ended")
		}
		if reported, ok := status.FromError(err); !ok ||
			reported.Code() != codes.DeadlineExceeded {
			t.Fatalf("a silent session ended with %v, want DeadlineExceeded", err)
		}
		if waited < 20*time.Second {
			t.Errorf("a silent session was ended after %v; a healthy relay between "+
				"heartbeats must not be cut off", waited)
		}
		// The client's own deadline is longer than the allowance, so an upper bound is what
		// separates the server ending this session from the test's deadline ending it.
		if waited > 75*time.Second {
			t.Errorf("a silent session survived %v; a relay behind a half-open connection "+
				"holds its leases for exactly as long as this takes", waited)
		}
	})
}

// The relay states the highest protocol version it speaks, so the two ends of that comparison
// mean opposite things: a newer relay can speak this one and must be let in, while one that
// cannot reach this version has to be turned away. Getting the direction wrong either locks out
// every relay released after this control plane, or admits one that cannot understand what it
// is told — and two sides that disagree about meaning still exchange messages successfully,
// which is how evidence nobody can vouch for gets recorded.
func TestRelaySessionNegotiatesTheProtocolVersion(t *testing.T) {
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	placements := openPlacement(t, placementDSN)
	owner := namedOrganization(t, organization)

	t.Run("a relay that speaks a newer version is negotiated down", func(t *testing.T) {
		relay := registerRelay(t, connection, placementDSN, organization)
		job := enqueueJob(t, placements, owner, relay.registration, workloadArguments("payments"))

		stream := openStream(t, connection, organization, relay)
		awaitSessionAccepted(t, stream)
		sayHello(t, stream, protocolVersionUnderTest+1, nil)

		// Work arriving is the proof. The session being left open proves only that nothing has
		// gone wrong yet; delivery only starts once the hello has been accepted.
		assignment := awaitAssignment(t, stream)
		if assignment.GetJobId() != job.String() {
			t.Fatalf("delivered job %q, want %v", assignment.GetJobId(), job)
		}
	})

	t.Run("a relay that cannot reach this version is refused", func(t *testing.T) {
		relay := registerRelay(t, connection, placementDSN, organization)

		stream := openStream(t, connection, organization, relay)
		awaitSessionAccepted(t, stream)
		sayHello(t, stream, protocolVersionUnderTest-1, nil)

		var err error
		for err == nil {
			_, err = stream.Recv()
		}
		reported, ok := status.FromError(err)
		if !ok || reported.Code() != codes.FailedPrecondition {
			t.Fatalf("a relay below this protocol version was ended with %v, want "+
				"FailedPrecondition — a relay treats that as terminal rather than retrying "+
				"into the same mismatch", err)
		}
	})
}

// A relay that reconnects part-way through a job is still holding that job's result. Without
// adoption the result arrives on a session that does not own the lease, is refused as stale,
// and the whole execution is thrown away and done again once the lease expires — so a network
// blip costs an investigation its evidence twice over.
func TestRelaySessionCarriesWorkAcrossAReconnection(t *testing.T) {
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})
	// Reconciling a reconnection is the one thing here the relay cannot see the reasoning for,
	// so a failure that reports only "nothing arrived" would say nothing about why.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("control plane logs:\n%s", plane.logs.String())
		}
	})

	connection := dialRelay(t, relayAddress)
	relay := registerRelay(t, connection, placementDSN, organization)
	placements := openPlacement(t, placementDSN)
	owner := namedOrganization(t, organization)

	// Two jobs, so the reconnection has something to carry over and something to give up.
	enqueueJob(t, placements, owner, relay.registration, workloadArguments("payments"))
	enqueueJob(t, placements, owner, relay.registration, workloadArguments("checkout"))

	before := connectSession(t, connection, organization, relay)
	awaitSessionAccepted(t, before)
	running := awaitAssignment(t, before)
	dropped := awaitAssignment(t, before)

	// The relay reconnects still executing one of them, and says so. Reconnecting is what ends
	// the session before it, so nothing has to be closed here.
	after := connectSessionDeclaring(t, connection, organization, relay,
		[]*relayv1.InFlightJob{{
			JobId:      running.GetJobId(),
			LeaseEpoch: running.GetLeaseEpoch(),
			ElapsedMs:  4_000,
		}})
	awaitSessionAccepted(t, after)

	// Redelivery is asserted before anything is sent, because it is the only thing the control
	// plane can be saying at this point. Sending the result first would race: whichever helper
	// skipped past the other's message would discard it, and the test would pass or fail on
	// which goroutine got there first rather than on the behaviour.
	t.Run("work the relay is not running comes back at once", func(t *testing.T) {
		// The job it did not declare is being executed by nothing: the session that held it is
		// gone. Leaving it leased would add a full lease of doing nothing to the far side of
		// every blip, so it must be redelivered now rather than in ten minutes.
		redelivered := awaitAssignment(t, after)
		if redelivered.GetJobId() != dropped.GetJobId() {
			t.Fatalf("delivered job %q, want the undeclared %q back",
				redelivered.GetJobId(), dropped.GetJobId())
		}
		if redelivered.GetLeaseEpoch() <= dropped.GetLeaseEpoch() {
			t.Errorf("redelivered at generation %d, which does not supersede %d — the "+
				"abandoned execution's late result would still be recordable",
				redelivered.GetLeaseEpoch(), dropped.GetLeaseEpoch())
		}
	})

	t.Run("work the relay never stopped can still be recorded", func(t *testing.T) {
		// The execution finishes on the new stream, still echoing the generation it was assigned
		// under, because adoption deliberately did not raise it.
		sendResult(t, after, running.GetJobId(), running.GetLeaseEpoch())

		acknowledged := awaitResultAck(t, after)
		if acknowledged.GetJobId() != running.GetJobId() {
			t.Fatalf("acknowledged job %q, want %q", acknowledged.GetJobId(), running.GetJobId())
		}
		if acknowledged.GetDisposition() != relayv1.ResultAck_DISPOSITION_RECORDED {
			t.Fatalf("work carried across a reconnection was acknowledged as %v, want "+
				"recorded — anything else discards an execution that actually finished",
				acknowledged.GetDisposition())
		}
	})
}

// A relay identity that keeps being taken over is recorded as contested, and an ordinary
// reconnection is not. Both halves matter: an alert that fires on every reconnection is an
// alert nobody reads, and the thing it would be hiding is a stolen credential.
//
// What this cannot show from here is the part that distinguishes the two, because every
// connection a test makes comes from the loopback host. That distinction is exercised where the
// peer address is an input rather than a property of the machine running the suite.
func TestRelaySessionRecordsAContestedIdentity(t *testing.T) {
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	relay := registerRelay(t, connection, placementDSN, organization)
	placements := openPlacement(t, placementDSN)
	owner := namedOrganization(t, organization)

	conflict := func(t *testing.T) storage.SessionConflict {
		t.Helper()
		seen, err := placements.SessionConflict(context.Background(), owner, relay.registration)
		if err != nil {
			t.Fatalf("reading the session conflict: %v", err)
		}
		return seen
	}

	t.Run("reconnecting once is not contested", func(t *testing.T) {
		for range 2 {
			stream := connectSession(t, connection, organization, relay)
			awaitSessionAccepted(t, stream)
		}
		if seen := conflict(t); !seen.DetectedAt.IsZero() {
			t.Errorf("one reconnection was recorded as contested at %v; every relay that "+
				"restarts would raise a credential-theft alert", seen.DetectedAt)
		}
	})

	t.Run("being taken over repeatedly is", func(t *testing.T) {
		// Enough further connections that the takeovers stop being a sequence of unrelated
		// reconnections. They are made back to back, which is the shape of two parties holding
		// one credential rather than one relay recovering.
		for range 4 {
			stream := connectSession(t, connection, organization, relay)
			awaitSessionAccepted(t, stream)
		}

		seen := conflict(t)
		if seen.DetectedAt.IsZero() {
			t.Fatal("a relay identity taken over repeatedly was never recorded as contested; " +
				"the one party that can see this pattern would be reporting it to nobody")
		}
		if seen.DistinctHosts < 1 {
			t.Errorf("recorded %d hosts, want at least the one that connected",
				seen.DistinctHosts)
		}
		if !strings.Contains(plane.logs.String(), "relay identity is contested") {
			t.Error("nothing was said about a contested identity where an operator watching " +
				"would see it")
		}
	})
}

// A capability payload carrying fields this build cannot read is refused, and refusing it
// touches nothing durable. Recording it would state that a workload was read and understood
// when part of what came back was never looked at, and evidence is only worth what its
// provenance is worth.
func TestRelaySessionRefusesAResultItCannotFullyRead(t *testing.T) {
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	relay := registerRelay(t, connection, placementDSN, organization)
	placements := openPlacement(t, placementDSN)
	owner := namedOrganization(t, organization)

	enqueueJob(t, placements, owner, relay.registration, workloadArguments("payments"))

	before := connectSession(t, connection, organization, relay)
	awaitSessionAccepted(t, before)
	assignment := awaitAssignment(t, before)

	sendUnreadableResult(t, before, assignment.GetJobId(), assignment.GetLeaseEpoch())

	var err error
	for err == nil {
		_, err = before.Recv()
	}
	if reported, ok := status.FromError(err); !ok || reported.Code() != codes.InvalidArgument {
		t.Fatalf("a result carrying unreadable fields ended the session with %v, want "+
			"InvalidArgument", err)
	}

	// Nothing durable may have happened. The relay reconnects, declares the job it is still
	// holding a result for, and sends a result this build can read: if the refused one had been
	// stored, this would come back as already recorded rather than recorded.
	after := connectSessionDeclaring(t, connection, organization, relay,
		[]*relayv1.InFlightJob{{
			JobId:      assignment.GetJobId(),
			LeaseEpoch: assignment.GetLeaseEpoch(),
		}})
	awaitSessionAccepted(t, after)
	sendResult(t, after, assignment.GetJobId(), assignment.GetLeaseEpoch())

	acknowledged := awaitResultAck(t, after)
	if acknowledged.GetDisposition() != relayv1.ResultAck_DISPOSITION_RECORDED {
		t.Errorf("after a refused result the job was acknowledged as %v, want recorded — a "+
			"protocol refusal must leave job truth exactly as it found it",
			acknowledged.GetDisposition())
	}
}

// sendUnreadableResult reports an outcome carrying a field this build has never heard of,
// nested inside a repeated message so the refusal has to be looking further than the top level.
// It is how a relay built against a newer schema would sound.
func sendUnreadableResult(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient, job string, epoch uint64,
) {
	t.Helper()

	pod := &relayv1.KubernetesPodRuntime{Name: "api-0", Phase: "Running"}
	// Field 999, a varint — a number no version of this schema assigns.
	pod.ProtoReflect().SetUnknown(protoreflect.RawFields{0xb8, 0x3e, 0x01})

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
		JobResult: &relayv1.JobResult{
			JobId:      job,
			LeaseEpoch: epoch,
			Outcome: &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{
				Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
					KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
						Outcome:  relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
						Pods:     []*relayv1.KubernetesPodRuntime{pod},
						Complete: true,
					},
				},
			}},
		},
	}})
	if err != nil {
		t.Fatalf("sending the unreadable result: %v", err)
	}
}

// A relay that stops reading, with work queued behind it, must still be ended.
//
// This does NOT prove why the liveness watch runs on its own goroutine. That change was made
// because acknowledgements queue on the same bounded channel as assignments, so the session
// loop can block inside a send while it is the only thing watching for silence — but the queue
// cannot actually fill while dispatch is capped at what the relay can hold, and this test was
// checked against the previous in-loop timer and passed. It is here for the behaviour it
// states, not as evidence for that reasoning.
func TestRelaySessionEndsARelayThatStoppedReading(t *testing.T) {
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	relay := registerRelay(t, connection, placementDSN, organization)
	placements := openPlacement(t, placementDSN)
	owner := namedOrganization(t, organization)

	// More work than the outbound queue holds, so delivery fills it and blocks.
	for range 40 {
		enqueueJob(t, placements, owner, relay.registration, workloadArguments("payments"))
	}

	stream := connectSession(t, connection, organization, relay)
	awaitSessionAccepted(t, stream)
	assignment := awaitAssignment(t, stream)

	// One result, which is what puts the session loop on the same blocked queue as delivery:
	// it has to send an acknowledgement, and there is nowhere to put it.
	sendResult(t, stream, assignment.GetJobId(), assignment.GetLeaseEpoch())

	// Nothing is read for longer than the allowance. A real relay in this state is one whose
	// process is wedged or whose connection is half-open.
	time.Sleep(55 * time.Second)

	// Whatever was already queued arrives first; what matters is that the stream then ends,
	// rather than staying open until this test's own deadline expires.
	resumed := time.Now()
	var err error
	for err == nil {
		_, err = stream.Recv()
	}
	ended := time.Since(resumed)

	if reported, ok := status.FromError(err); !ok || reported.Code() != codes.DeadlineExceeded {
		t.Fatalf("the session ended with %v, want DeadlineExceeded", err)
	}
	if ended > 20*time.Second {
		t.Errorf("the session was still open %v after reading resumed; it was never ended "+
			"server-side, and this test's own deadline is what closed it", ended)
	}
}

// A long-lived stream does not end because the process was asked to stop, so shutdown has to
// end it. Waiting for the relay to notice would let one unresponsive relay hold the process
// open for as long as it stayed silent — a deploy that hangs on a customer's network problem.
func TestRelayEndpointStopsWithinItsBudget(t *testing.T) {
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		cfg.ShutdownTimeout = 5 * time.Second
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	unresponsive := registerRelay(t, connection, placementDSN, organization)
	stream := connectSession(t, connection, organization, unresponsive)
	awaitSessionAccepted(t, stream)
	// Nothing reads or writes on that stream again. It is established, idle, and holding a
	// handler open — which is what a relay behind a stalled network looks like from here.

	started := time.Now()
	plane.shutdown()
	took := time.Since(started)

	if plane.exitErr != nil {
		t.Fatalf("the control plane did not stop cleanly: %v", plane.exitErr)
	}
	if took > 20*time.Second {
		t.Errorf("stopping took %v against a shutdown budget of 5s; an idle session must "+
			"not decide how long a deploy takes", took)
	}
}

// A session is refused with one status whatever is wrong with the identity presented. The
// alternative tells a caller which registrations exist, which is the same disclosure the
// enrolment refusals are shaped to avoid.
func TestRelaySessionRefusesUnprovenIdentity(t *testing.T) {
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	relay := registerRelay(t, connection, placementDSN, organization)

	unknown := refuseSession(t, connection, organization, relayCredentials{
		registration: uuid.New(), credential: relay.credential,
	})
	wrong := refuseSession(t, connection, organization, relayCredentials{
		registration: relay.registration, credential: "a-credential-that-was-never-issued",
	})

	if unknown != wrong {
		t.Errorf("an unknown registration says %q and a wrong credential says %q; the "+
			"difference tells a caller which registrations exist", unknown, wrong)
	}
}
