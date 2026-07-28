package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
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

// A relay speaking a protocol this control plane does not speak must be refused, and told so.
// Carrying on would mean exchanging messages whose meaning the two sides do not agree on, and
// the evidence this platform records is only worth what its provenance is worth.
func TestRelaySessionRefusesAProtocolItDoesNotSpeak(t *testing.T) {
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

	stream := openStream(t, connection, organization, relay)
	awaitSessionAccepted(t, stream)
	sayHello(t, stream, protocolVersionUnderTest+1, nil)

	var err error
	for err == nil {
		_, err = stream.Recv()
	}
	reported, ok := status.FromError(err)
	if !ok || reported.Code() != codes.FailedPrecondition {
		t.Fatalf("a relay speaking an unknown protocol was ended with %v, want "+
			"FailedPrecondition — a relay treats that as terminal rather than retrying "+
			"into the same mismatch", err)
	}
}

// A relay that reconnects part-way through a job is still holding that job's result. Without
// adoption the result arrives on a session that does not own the lease, is refused as stale,
// and the whole execution is thrown away and done again once the lease expires — so a network
// blip costs an investigation its evidence twice over.
func TestRelaySessionCarriesWorkAcrossAReconnection(t *testing.T) {
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

// The capability the test dispatches. Its version travels on the assignment because the
// relay picks an implementation by it.
const (
	capabilityUnderTest        = "kubernetes.workload.runtime"
	capabilityVersionUnderTest = 1
	protocolVersionUnderTest   = 1
)

// relayCredentials is a registered relay's durable identity, as the relay itself holds it.
type relayCredentials struct {
	registration uuid.UUID
	credential   string
}

// registerRelay enrols a relay the way one really enrols, through the registration service,
// so the session is authenticated against a credential the control plane actually issued.
func registerRelay(
	t *testing.T, connection *grpc.ClientConn, dsn, organization string,
) relayCredentials {
	t.Helper()

	token := "bootstrap-token-for-" + uuid.NewString()
	issueBootstrapToken(t, dsn, organization, token)

	client := relayv1.NewRelayRegistrationServiceClient(connection)
	response, err := register(t, client, organization, token, &relayv1.RegisterRequest{
		ProtocolVersion:    1,
		RelayVersion:       "0.1.0-test",
		ClusterFingerprint: "kube-system-uid-under-test",
		Capabilities: []*relayv1.CapabilityDescriptor{
			{CapabilityId: capabilityUnderTest, CapabilityVersion: capabilityVersionUnderTest},
		},
	})
	if err != nil {
		t.Fatalf("registering the relay the session needs: %v", err)
	}
	registration, err := uuid.Parse(response.GetRegistrationId())
	if err != nil {
		t.Fatalf("the issued registration identity is not an identity: %v", err)
	}
	return relayCredentials{registration: registration, credential: response.GetCredential()}
}

// connectSession opens a stream for a relay carrying nothing over from a previous one.
func connectSession(
	t *testing.T, connection *grpc.ClientConn, organization string, relay relayCredentials,
) relayv1.RelaySessionService_ConnectClient {
	t.Helper()
	return connectSessionDeclaring(t, connection, organization, relay, nil)
}

// connectSessionDeclaring opens the stream, says hello as a real relay does, and leaves it
// open for the duration of the test. The hello is what opens delivery, so a client that
// skipped it would be testing a session no relay ever has.
//
// The deadline bounds a failure: without it a message that never arrives hangs until the whole
// suite times out, which reports nothing about which message was missing.
func connectSessionDeclaring(
	t *testing.T,
	connection *grpc.ClientConn,
	organization string,
	relay relayCredentials,
	inFlight []*relayv1.InFlightJob,
) relayv1.RelaySessionService_ConnectClient {
	t.Helper()

	stream := openStream(t, connection, organization, relay)
	sayHello(t, stream, protocolVersionUnderTest, inFlight)
	return stream
}

// openStream opens the session without saying anything on it, for the tests whose subject is
// the hello itself.
func openStream(
	t *testing.T, connection *grpc.ClientConn, organization string, relay relayCredentials,
) relayv1.RelaySessionService_ConnectClient {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	stream, err := relayv1.NewRelaySessionServiceClient(connection).
		Connect(sessionMetadata(ctx, organization, relay))
	if err != nil {
		t.Fatalf("opening the session: %v", err)
	}
	return stream
}

func sayHello(
	t *testing.T,
	stream relayv1.RelaySessionService_ConnectClient,
	protocolVersion uint32,
	inFlight []*relayv1.InFlightJob,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_Hello{
		Hello: &relayv1.Hello{
			ProtocolVersion:   protocolVersion,
			RelayVersion:      "0.1.0-test",
			MaxConcurrentJobs: 4,
			Capabilities: []*relayv1.CapabilityDescriptor{
				{CapabilityId: capabilityUnderTest, CapabilityVersion: capabilityVersionUnderTest},
			},
			InFlight: inFlight,
		},
	}})
	if err != nil {
		t.Fatalf("saying hello: %v", err)
	}
}

// refuseSession opens a session that must not be accepted and returns the refusal message.
// A stream reports its refusal on the first receive rather than when it is opened, because
// the server has said nothing until then.
func refuseSession(
	t *testing.T, connection *grpc.ClientConn, organization string, relay relayCredentials,
) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := relayv1.NewRelaySessionServiceClient(connection).
		Connect(sessionMetadata(ctx, organization, relay))
	if err != nil {
		t.Fatalf("opening the session: %v", err)
	}
	if _, err = stream.Recv(); err == nil {
		t.Fatal("the session was accepted; an unproven identity must be refused")
	}
	reported, ok := status.FromError(err)
	if !ok || reported.Code() != codes.Unauthenticated {
		t.Fatalf("refused with %v, want Unauthenticated", err)
	}
	return reported.Message()
}

func sessionMetadata(
	ctx context.Context, organization string, relay relayCredentials,
) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		"opencluster-org-id", organization,
		"opencluster-registration-id", relay.registration.String(),
		"opencluster-relay-credential", relay.credential)
}

func awaitSessionAccepted(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.SessionAccepted {
	t.Helper()
	return awaitMessage(t, stream, "session acceptance",
		func(message *relayv1.ControlToRelay) *relayv1.SessionAccepted {
			return message.GetSessionAccepted()
		})
}

func awaitAssignment(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.JobAssignment {
	t.Helper()
	return awaitMessage(t, stream, "job assignment",
		func(message *relayv1.ControlToRelay) *relayv1.JobAssignment {
			return message.GetJobAssignment()
		})
}

func awaitResultAck(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.ResultAck {
	t.Helper()
	return awaitMessage(t, stream, "result acknowledgement",
		func(message *relayv1.ControlToRelay) *relayv1.ResultAck {
			return message.GetResultAck()
		})
}

// awaitReconnectInstruction drains a session that is being closed, returning the reconnect
// instruction if one arrived before the end and the status that ended it.
func awaitReconnectInstruction(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) (*relayv1.GracefulReconnect, error) {
	t.Helper()

	var told *relayv1.GracefulReconnect
	for {
		message, err := stream.Recv()
		if err != nil {
			return told, err
		}
		if reconnect := message.GetGracefulReconnect(); reconnect != nil {
			told = reconnect
		}
	}
}

func awaitCancellation(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient,
) *relayv1.Cancellation {
	t.Helper()
	return awaitMessage(t, stream, "cancellation",
		func(message *relayv1.ControlToRelay) *relayv1.Cancellation {
			return message.GetCancellation()
		})
}

// awaitMessage reads until the wanted kind arrives, ignoring the rest. Skipping rather than
// failing on an unexpected kind is deliberate: the server may add messages a real relay
// tolerates, and a test that breaks on a new heartbeat is testing the wire order instead of
// the guarantee.
func awaitMessage[T any](
	t *testing.T,
	stream relayv1.RelaySessionService_ConnectClient,
	wanted string,
	extract func(*relayv1.ControlToRelay) *T,
) *T {
	t.Helper()

	for {
		message, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for a %s: %v", wanted, err)
		}
		if found := extract(message); found != nil {
			return found
		}
	}
}

// sendResult reports a successful execution. The epoch is echoed rather than remembered
// server-side, which is what makes a result from a superseded lease refusable.
func sendResult(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient, job string, epoch uint64,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
		JobResult: &relayv1.JobResult{
			JobId:      job,
			LeaseEpoch: epoch,
			Outcome: &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{
				Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
					KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
						Outcome:  relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
						Complete: true,
					},
				},
			}},
			ExecutionDurationMs: 12,
		},
	}})
	if err != nil {
		t.Fatalf("sending the result for job %s: %v", job, err)
	}
}

// acknowledgeCancellation reports that the stop was processed. It changes nothing durable —
// the outcome still arrives as a result — so it is sent here mainly to prove that it does not.
func acknowledgeCancellation(
	t *testing.T,
	stream relayv1.RelaySessionService_ConnectClient,
	cancellation *relayv1.Cancellation,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_CancelAck{
		CancelAck: &relayv1.CancelAck{
			JobId:       cancellation.GetJobId(),
			LeaseEpoch:  cancellation.GetLeaseEpoch(),
			Disposition: relayv1.CancelAck_DISPOSITION_ABORTED,
		},
	}})
	if err != nil {
		t.Fatalf("acknowledging the cancellation: %v", err)
	}
}

// sendCancelledResult reports that an execution stopped because it was asked to. A cancelled
// job is a failure with a reason, not an absence of an outcome.
func sendCancelledResult(
	t *testing.T, stream relayv1.RelaySessionService_ConnectClient, job string, epoch uint64,
) {
	t.Helper()

	err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
		JobResult: &relayv1.JobResult{
			JobId:      job,
			LeaseEpoch: epoch,
			Outcome: &relayv1.JobResult_Failure{Failure: &relayv1.JobFailure{
				Kind: relayv1.JobFailure_KIND_CANCELLED,
			}},
		},
	}})
	if err != nil {
		t.Fatalf("sending the cancelled result for job %s: %v", job, err)
	}
}

// workloadArguments encodes a capability argument as the planner will: already reduced to
// the wire form, so nothing has to interpret it between here and the relay.
func workloadArguments(workload string) []byte {
	encoded, err := proto.Marshal(&relayv1.CapabilityArguments{
		Arguments: &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
				Namespace:    "production",
				WorkloadKind: relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT,
				WorkloadName: workload,
				MaxPods:      50,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return encoded
}

func enqueueJob(
	t *testing.T,
	placements *storage.Placements,
	organization tenancy.Organization,
	registration uuid.UUID,
	arguments []byte,
) uuid.UUID {
	t.Helper()

	job := storage.Job{
		ID:                uuid.New(),
		RegistrationID:    registration,
		CapabilityID:      capabilityUnderTest,
		CapabilityVersion: capabilityVersionUnderTest,
		Arguments:         arguments,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := placements.EnqueueJob(ctx, organization, job); err != nil {
		t.Fatalf("enqueueing a job: %v", err)
	}
	return job.ID
}

func namedOrganization(t *testing.T, organization string) tenancy.Organization {
	t.Helper()

	named, err := tenancy.NewOrganization(organization)
	if err != nil {
		t.Fatalf("naming the organization: %v", err)
	}
	return named
}
