package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// What is asserted here is durable state: what the case version did, what a write from a lost
// lease was allowed to do, and what the database refused outright. The version tests are the
// load-bearing ones and are written to fail if the .NET defect returns — a version that tracks only
// lifecycle transitions leaves a polling client blind to the evidence growth it is watching for.

// heldCase is a case with one round claimed by a worker session, which is the state every write
// below has to be in.
type heldCase struct {
	placements   *storage.Placements
	organization tenancy.Organization
	connection   uuid.UUID
	// subject is the case itself. It is a named field rather than an embedded one: embedding would
	// let h.ID mean the case's identifier while h.round.ID means the round's, which is the kind of
	// ambiguity a test assertion should not have to resolve.
	subject investigation.Investigation
	round   investigation.Round
}

func (h heldCase) fence() investigation.Fence {
	return investigation.Fence{
		RoundID:      h.round.ID,
		LeaseSession: h.round.LeaseSession,
		LeaseEpoch:   h.round.LeaseEpoch,
	}
}

// version reads the case version as it stands now.
func (h heldCase) version(t *testing.T) int64 {
	t.Helper()
	version, err := h.placements.CaseVersion(
		context.Background(), h.organization, h.subject.ID)
	if err != nil {
		t.Fatalf("reading the case version: %v", err)
	}
	return version
}

// lifecycle reads the case's lifecycle, so a test asserting that the version moved without one can
// prove the second half rather than assume it.
func (h heldCase) lifecycle(t *testing.T) investigation.Lifecycle {
	t.Helper()
	found, err := h.placements.Investigation(
		context.Background(), h.organization, h.subject.ID)
	if err != nil {
		t.Fatalf("reading the case: %v", err)
	}
	return found.Lifecycle
}

func heldInvestigation(t *testing.T) heldCase {
	t.Helper()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)
	return holdCase(t, placements, organization, connection)
}

func holdCase(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, connection uuid.UUID,
) heldCase {
	t.Helper()
	ctx := context.Background()

	opened, err := placements.OpenInvestigation(ctx, ownerOf(t, organization), organization, investigation.New{
		Scope: investigation.Scope{
			Connection:   connection,
			Namespace:    "payments",
			WorkloadKind: investigation.WorkloadDeployment,
			WorkloadName: "checkout",
		},
		Window: investigation.Window{
			Start: time.Now().Add(-time.Hour).UTC(),
			End:   time.Now().UTC(),
		},
		Trigger: investigation.Trigger{
			Kind:        investigation.TriggerManual,
			RequestedBy: "127.0.0.1",
			At:          time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatalf("opening an investigation: %v", err)
	}

	if _, err = placements.OpenRound(ctx, ownerOf(t, organization), organization, investigation.Opening{
		InvestigationID: opened.ID,
		Controls:        investigation.DefaultControls(),
		Plan:            investigation.Plan{Template: "kubernetes-workload-v1"},
		Versions: investigation.Versions{
			Planner: "p1", Model: "recorded", PromptVersion: "1",
			SchemaVersion: "1", Investigator: "test",
		},
	}); err != nil {
		t.Fatalf("opening a round: %v", err)
	}

	claimed, err := placements.ClaimRounds(ctx, organization, investigation.RoundClaim{
		SessionID: uuid.New(), LeaseFor: time.Minute, Capacity: 4,
	})
	if err != nil {
		t.Fatalf("claiming a round: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rounds, want 1", len(claimed))
	}
	return heldCase{
		placements:   placements,
		organization: organization,
		connection:   connection,
		subject:      claimed[0].Investigation,
		round:        claimed[0].Round,
	}
}

func (h heldCase) recordEvidence(t *testing.T, statement string) investigation.Item {
	t.Helper()
	recorded, err := h.placements.RecordEvidence(
		context.Background(), h.organization, h.fence(), []investigation.Item{{
			CapabilityID:      "kubernetes.namespace.events",
			CapabilityVersion: 1,
			Connection:        h.connection,
			Statement:         statement,
			Content:           "the raw text this claim rests on",
			Trust:             investigation.TrustRelayAttested,
			SourceObservedAt:  time.Now().Add(-30 * time.Minute).UTC(),
			ReceivedAt:        time.Now().UTC(),
		}})
	if err != nil {
		t.Fatalf("recording evidence: %v", err)
	}
	return recorded[0]
}

func (h heldCase) recordHypothesis(t *testing.T, statement string) investigation.Hypothesis {
	t.Helper()
	recorded, err := h.placements.RecordHypotheses(
		context.Background(), h.organization, h.fence(), []investigation.Hypothesis{{
			Statement: statement,
			Falsifies: "a readiness probe that has been passing throughout",
			State:     investigation.HypothesisLive,
		}})
	if err != nil {
		t.Fatalf("recording a hypothesis: %v", err)
	}
	return recorded[0]
}

func (h heldCase) recordGap(t *testing.T, subject string) investigation.Gap {
	t.Helper()
	recorded, err := h.placements.RecordGaps(
		context.Background(), h.organization, h.fence(), []investigation.Gap{{
			Cause:        investigation.GapRetentionHorizon,
			CapabilityID: "kubernetes.namespace.events",
			Subject:      subject,
			Consequence:  "the first half of the window cannot be reconstructed",
		}})
	if err != nil {
		t.Fatalf("recording a coverage gap: %v", err)
	}
	return recorded[0]
}

// THE load-bearing test. Recording an evidence item advances the whole-case version with no
// lifecycle transition involved. The frozen .NET implementation shipped the opposite and its own
// frontend audit recorded the consequence, so this is written to fail if it returns.
func TestInvestigation_EvidenceAdvancesTheCaseVersionWithNoLifecycleTransition(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)

	before, lifecycleBefore := held.version(t), held.lifecycle(t)
	held.recordEvidence(t, "the container exited with code 1 four times in ten minutes")
	after, lifecycleAfter := held.version(t), held.lifecycle(t)

	if after <= before {
		t.Errorf("recording evidence left the case version at %d; it must advance from %d",
			after, before)
	}
	if lifecycleAfter != lifecycleBefore {
		t.Fatalf("the lifecycle moved from %s to %s; this test proves the version advances "+
			"WITHOUT one, so the assertion above would be vacuous",
			lifecycleBefore, lifecycleAfter)
	}
}

func TestInvestigation_ACoverageGapAdvancesTheCaseVersion(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)

	before, lifecycleBefore := held.version(t), held.lifecycle(t)
	held.recordGap(t, "events older than the cluster's retention")

	if after := held.version(t); after <= before {
		t.Errorf("recording a coverage gap left the case version at %d, want it past %d",
			after, before)
	}
	if after := held.lifecycle(t); after != lifecycleBefore {
		t.Fatalf("the lifecycle moved from %s to %s; this test proves the version advances "+
			"WITHOUT one, so the assertion above would be vacuous", lifecycleBefore, after)
	}
}

func TestInvestigation_AHypothesisStanceChangeAdvancesTheCaseVersion(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)

	hypothesis := held.recordHypothesis(t, "the image cannot be pulled")
	item := held.recordEvidence(t, "the kubelet reports ErrImagePull")

	before, lifecycleBefore := held.version(t), held.lifecycle(t)
	if err := held.placements.RecordStances(
		context.Background(), held.organization, held.fence(), []investigation.Weighed{{
			HypothesisID: hypothesis.ID,
			EvidenceID:   item.ID,
			Stance:       investigation.StanceSupports,
			Reason:       "the reason names the image this workload declares",
		}}); err != nil {
		t.Fatalf("recording a stance: %v", err)
	}

	if after := held.version(t); after <= before {
		t.Errorf("a stance left the case version at %d, want it past %d", after, before)
	}
	if after := held.lifecycle(t); after != lifecycleBefore {
		t.Fatalf("the lifecycle moved from %s to %s; this test proves the version advances "+
			"WITHOUT one, so the assertion above would be vacuous", lifecycleBefore, after)
	}
}

// A heartbeat is not a change within the case. A version that advanced on one would tell every
// polling client that something happened every few seconds, which is the same defect as a version
// that never advances arriving from the other side.
func TestInvestigation_RenewingALeaseDoesNotAdvanceTheCaseVersion(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)

	before := held.version(t)
	if err := held.placements.RenewRoundLease(
		context.Background(), held.organization, held.fence(), 2*time.Minute); err != nil {
		t.Fatalf("renewing a lease: %v", err)
	}

	if after := held.version(t); after != before {
		t.Errorf("a lease renewal moved the case version from %d to %d; a heartbeat is not a "+
			"change within the case", before, after)
	}
}

// The fence, from the direction that matters: an execution that lost its round writes nothing.
func TestInvestigation_AWorkerWhoseLeaseMovedCannotWrite(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	stale := held.fence()

	// Another worker takes the round over. Expiring the lease is what a control-plane restart
	// leaves behind, and reclaiming is what the next instance does with it.
	expireRoundLease(t, held.placements, held.organization, held.round.ID)
	reclaimed, err := held.placements.ClaimRounds(ctx, held.organization,
		investigation.RoundClaim{SessionID: uuid.New(), LeaseFor: time.Minute, Capacity: 1})
	if err != nil {
		t.Fatalf("reclaiming: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed %d rounds, want 1", len(reclaimed))
	}
	if reclaimed[0].Round.LeaseEpoch <= stale.LeaseEpoch {
		t.Fatalf("reclaiming produced generation %d, which does not supersede %d",
			reclaimed[0].Round.LeaseEpoch, stale.LeaseEpoch)
	}

	_, err = held.placements.RecordEvidence(ctx, held.organization, stale,
		[]investigation.Item{{
			CapabilityID: "kubernetes.namespace.events", CapabilityVersion: 1,
			Connection: held.connection, Statement: "written by a worker that lost its lease",
			Trust: investigation.TrustRelayAttested,
		}})
	if !errors.Is(err, investigation.ErrLeaseLost) {
		t.Fatalf("a lost lease recorded evidence with %v, want ErrLeaseLost", err)
	}

	// And nothing was written, which is the half that matters.
	summary, err := held.placements.InvestigationSummary(
		ctx, held.organization, held.subject.ID)
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	if summary.Counts.Evidence != 0 {
		t.Errorf("the case holds %d evidence items; a lost lease must write none",
			summary.Counts.Evidence)
	}
}

// The Environment is derived from the Connection and never accepted. There is nowhere in the write
// for a caller-supplied one to arrive, which is stronger than ignoring one.
func TestInvestigation_EnvironmentIsDerivedFromTheConnection(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)

	connection, err := held.placements.ConnectionForOrganization(
		context.Background(), held.organization, held.connection)
	if err != nil {
		t.Fatalf("reading the connection: %v", err)
	}
	if held.subject.Environment != connection.Environment {
		t.Errorf("the case's environment is %s, want the Connection's %s",
			held.subject.Environment, connection.Environment)
	}
}

// A case cannot be opened through a Connection that could never answer a read. One refusal covers
// every way of getting it wrong, for the reason a refused job's does.
func TestInvestigation_ARefusedConnectionOpensNothing(t *testing.T) {
	t.Parallel()
	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	ctx := context.Background()

	environment, err := placements.EnsureDefaultEnvironment(ctx, ownerOf(t, organization), organization)
	if err != nil {
		t.Fatalf("ensuring the default environment: %v", err)
	}
	// Trigger-only: it delivers Signals inbound and answers nothing outbound.
	triggerOnly, err := placements.CreateConnection(ctx, ownerOf(t, organization), organization, storage.NewConnection{
		Environment:  environment.ID,
		Integration:  "alertmanager",
		Name:         "production alertmanager",
		Role:         storage.RoleTrigger,
		Locality:     storage.LocalityControlPlane,
		SecretDigest: randomDigest(t),
	})
	if err != nil {
		t.Fatalf("creating a trigger connection: %v", err)
	}

	for _, refused := range []struct {
		name       string
		connection uuid.UUID
	}{
		{"a trigger-only connection", triggerOnly.ID},
		{"a connection this organization does not have", uuid.New()},
	} {
		t.Run(refused.name, func(t *testing.T) {
			_, err := placements.OpenInvestigation(ctx, ownerOf(t, organization), organization, investigation.New{
				Scope: investigation.Scope{
					Connection:   refused.connection,
					Namespace:    "payments",
					WorkloadKind: investigation.WorkloadDeployment,
					WorkloadName: "checkout",
				},
				Window: investigation.Window{
					Start: time.Now().Add(-time.Hour), End: time.Now(),
				},
				Trigger: investigation.Trigger{Kind: investigation.TriggerManual, At: time.Now()},
			})
			if !errors.Is(err, investigation.ErrConnectionUnusable) {
				t.Fatalf("opening through %s returned %v, want a refusal", refused.name, err)
			}
		})
	}
	_ = registration
}

// A request naming one organization's identity and another's investigation must find nothing. Both
// organizations are on the same placement deliberately: an organization with no placement fails
// before any query runs, which would leave this passing against an implementation with no scoping
// at all.
func TestInvestigation_ANeighbourCannotReadThisCase(t *testing.T) {
	t.Parallel()

	placements := openPlacements(t,
		map[string]string{"shared": postgresDSN(t)},
		map[string]string{testOrganization: "shared", "org-neighbour": "shared"})
	if _, err := placements.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	organization := organization(t, testOrganization)
	neighbour := organization2(t, "org-neighbour")

	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)
	held := holdCase(t, placements, organization, connection)

	ctx := context.Background()
	if _, err := placements.Investigation(
		ctx, neighbour, held.subject.ID); !errors.Is(err, investigation.ErrUnknown) {
		t.Errorf("a neighbour read the case with %v, want it unknown to them", err)
	}
	if _, err := placements.InvestigationSummary(
		ctx, neighbour, held.subject.ID); !errors.Is(err, investigation.ErrUnknown) {
		t.Errorf("a neighbour read the summary with %v, want it unknown to them", err)
	}
	if _, err := placements.InvestigationEvidence(ctx, neighbour, held.subject.ID,
		investigation.EvidenceFilter{}, investigation.Page{}); !errors.Is(err, investigation.ErrUnknown) {
		t.Errorf("a neighbour read the evidence with %v, want it unknown to them", err)
	}
	if _, err := placements.AssembleCaseFile(
		ctx, neighbour, held.subject.ID, 0); !errors.Is(err, investigation.ErrUnknown) {
		t.Errorf("a neighbour assembled the case with %v, want it unknown to them", err)
	}
}

// An absence is only a fact with a completeness certificate, and the database says so as well as
// the validator. A rule enforced in one of two writers is a rule that holds until the second writer
// is added.
func TestInvestigation_AnAbsenceWithNoCertificateIsRefusedByTheDatabase(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)

	_, err := held.placements.RecordEvidence(
		context.Background(), held.organization, held.fence(), []investigation.Item{{
			CapabilityID:      "kubernetes.namespace.events",
			CapabilityVersion: 1,
			Connection:        held.connection,
			Statement:         "no events were recorded in the window",
			Absence:           true,
			Trust:             investigation.TrustRelayAttested,
		}})
	if err == nil {
		t.Fatal("an absence with no completeness certificate was recorded; it must be refused")
	}
}

// Every claim must resolve to an evidence item in the SAME case. The foreign key carries the case,
// so this is something the database refuses rather than something a test happens to check.
func TestInvestigation_AClaimCannotCiteAnotherCasesEvidence(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	// A second case in the same organization, with evidence of its own.
	other := holdCase(t, held.placements, held.organization, held.connection)
	foreign := other.recordEvidence(t, "belongs to another case entirely")

	err := held.placements.RecordOutcome(ctx, held.organization, held.fence(),
		investigation.Outcome{
			Kind:      investigation.OutcomeSupported,
			Statement: "the workload cannot start",
			Claims: []investigation.Claim{{
				Ordinal:   1,
				Role:      investigation.ClaimSupporting,
				Statement: "cited from somewhere else",
				Evidence:  []uuid.UUID{foreign.ID},
			}},
		})
	if err == nil {
		t.Fatal("a claim cited another case's evidence; the citation must be refused")
	}
}

// Superseding rather than rewriting: when a later round explains what an earlier one abstained on,
// the earlier outcome stays readable with its round and its time.
func TestInvestigation_ASupersededOutcomeStaysReadable(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	first := held.recordEvidence(t, "the pod is not ready")
	gap := held.recordGap(t, "node metrics")
	if err := held.placements.RecordOutcome(ctx, held.organization, held.fence(),
		investigation.Outcome{
			Kind:         investigation.OutcomeAbstained,
			Statement:    "no explanation is sufficiently supported",
			RelevantGaps: []uuid.UUID{gap.ID},
			Claims: []investigation.Claim{{
				Ordinal: 1, Role: investigation.ClaimContradicting,
				Statement: "the workload reports itself available",
				Evidence:  []uuid.UUID{first.ID},
			}},
		}); err != nil {
		t.Fatalf("recording the first outcome: %v", err)
	}
	if err := held.placements.FinishRound(ctx, held.organization, held.fence(),
		investigation.Finish{Outcome: investigation.RoundAbstained}); err != nil {
		t.Fatalf("finishing the first round: %v", err)
	}

	// A second round explains it.
	second := reopen(t, held)
	item := second.recordEvidence(t, "the limit change at 15:02 is what evicted it")
	if err := held.placements.RecordOutcome(ctx, held.organization, second.fence(),
		investigation.Outcome{
			Kind:      investigation.OutcomeSupported,
			Statement: "a memory limit change evicted the pod",
			Claims: []investigation.Claim{{
				Ordinal: 1, Role: investigation.ClaimSupporting,
				Statement: "the limit changed at 15:02", Evidence: []uuid.UUID{item.ID},
			}},
		}); err != nil {
		t.Fatalf("recording the second outcome: %v", err)
	}

	file, err := held.placements.AssembleCaseFile(ctx, held.organization, held.subject.ID, 0)
	if err != nil {
		t.Fatalf("assembling: %v", err)
	}
	if len(file.Outcomes) != 2 {
		t.Fatalf("the case holds %d outcomes, want both the superseded one and the current one",
			len(file.Outcomes))
	}

	var superseded, current int
	for _, outcome := range file.Outcomes {
		if outcome.Superseded {
			superseded++
			if outcome.Round != 1 {
				t.Errorf("the superseded outcome is attributed to round %d, want 1", outcome.Round)
			}
			if outcome.ReachedAt.IsZero() {
				t.Error("the superseded outcome must keep the time it was reached")
			}
			continue
		}
		current++
		if outcome.Kind != investigation.OutcomeSupported {
			t.Errorf("the current outcome is %s, want supported", outcome.Kind)
		}
	}
	if superseded != 1 || current != 1 {
		t.Errorf("%d superseded and %d current outcomes, want one of each", superseded, current)
	}
}

// Every section response carries the case version it represents, and a section read before an
// update is detectably older than a summary read after it.
func TestInvestigation_SectionsCarryTheVersionTheyRepresent(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	held.recordEvidence(t, "the first item")

	early, err := held.placements.InvestigationEvidence(ctx, held.organization,
		held.subject.ID, investigation.EvidenceFilter{}, investigation.Page{})
	if err != nil {
		t.Fatalf("listing evidence: %v", err)
	}
	if early.CaseVersion == 0 {
		t.Fatal("a section must carry the case version it represents")
	}

	held.recordEvidence(t, "the second item, which the page above does not know about")

	summary, err := held.placements.InvestigationSummary(
		ctx, held.organization, held.subject.ID)
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	if summary.Investigation.CaseVersion <= early.CaseVersion {
		t.Errorf("the summary is at version %d and the older section at %d; a stale section must "+
			"be detectable", summary.Investigation.CaseVersion, early.CaseVersion)
	}
	if summary.Counts.Evidence != 2 {
		t.Errorf("the summary counts %d evidence items, want 2", summary.Counts.Evidence)
	}
}

// A listing is not the size of its contents: content is fetched per item, on demand.
func TestInvestigation_EvidenceContentIsFetchedSeparatelyAndBounded(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	item := held.recordEvidence(t, "the container logged a refused connection")

	listed, err := held.placements.InvestigationEvidence(ctx, held.organization,
		held.subject.ID, investigation.EvidenceFilter{}, investigation.Page{})
	if err != nil {
		t.Fatalf("listing evidence: %v", err)
	}
	if len(listed.Items) != 1 {
		t.Fatalf("listed %d items, want 1", len(listed.Items))
	}
	if listed.Items[0].Content != "" {
		t.Error("a listing must not carry evidence content")
	}

	full, version, err := held.placements.EvidenceItem(
		ctx, held.organization, held.subject.ID, item.ID)
	if err != nil {
		t.Fatalf("reading an evidence item: %v", err)
	}
	if full.Content == "" {
		t.Error("the item read must carry its content")
	}
	if len(full.Content) > investigation.MaxEvidenceContentBytes {
		t.Errorf("content is %d bytes, above the %d bound the read path applies",
			len(full.Content), investigation.MaxEvidenceContentBytes)
	}
	if version == 0 {
		t.Error("an item read must carry the case version it represents")
	}
}

// Evidence is navigable in a case with hundreds of items, which means filterable.
func TestInvestigation_EvidenceIsFilterableByCapabilitySourceAndStance(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	events := held.recordEvidence(t, "the kubelet reports BackOff")
	logs, err := held.placements.RecordEvidence(ctx, held.organization, held.fence(),
		[]investigation.Item{{
			CapabilityID: "kubernetes.container.logs", CapabilityVersion: 1,
			Connection: held.connection, Statement: "connection refused to 10.0.0.4:5432",
			Trust: investigation.TrustRelayAttested,
		}})
	if err != nil {
		t.Fatalf("recording evidence: %v", err)
	}
	hypothesis := held.recordHypothesis(t, "the database is unreachable")
	if err = held.placements.RecordStances(ctx, held.organization, held.fence(),
		[]investigation.Weighed{{
			HypothesisID: hypothesis.ID, EvidenceID: logs[0].ID,
			Stance: investigation.StanceSupports, Reason: "it names the database's address",
		}}); err != nil {
		t.Fatalf("recording a stance: %v", err)
	}

	byCapability, err := held.placements.InvestigationEvidence(ctx, held.organization,
		held.subject.ID,
		investigation.EvidenceFilter{CapabilityID: "kubernetes.container.logs"}, investigation.Page{})
	if err != nil {
		t.Fatalf("filtering by capability: %v", err)
	}
	if len(byCapability.Items) != 1 || byCapability.Items[0].ID != logs[0].ID {
		t.Errorf("filtering by capability returned %d items, want only the log item",
			len(byCapability.Items))
	}

	bySource, err := held.placements.InvestigationEvidence(ctx, held.organization,
		held.subject.ID, investigation.EvidenceFilter{Source: held.connection}, investigation.Page{})
	if err != nil {
		t.Fatalf("filtering by source: %v", err)
	}
	if len(bySource.Items) != 2 {
		t.Errorf("filtering by the one source returned %d items, want both",
			len(bySource.Items))
	}

	byStance, err := held.placements.InvestigationEvidence(ctx, held.organization,
		held.subject.ID,
		investigation.EvidenceFilter{Stance: investigation.StanceSupports}, investigation.Page{})
	if err != nil {
		t.Fatalf("filtering by stance: %v", err)
	}
	if len(byStance.Items) != 1 || byStance.Items[0].ID != logs[0].ID {
		t.Errorf("filtering by stance returned %d items, want only the supporting one",
			len(byStance.Items))
	}
	_ = events
}

// A growing case must not reshuffle the pages a client is reading.
func TestInvestigation_PaginatedSectionsKeepOneOrderWhileTheCaseGrows(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	for index := range 5 {
		held.recordEvidence(t, "observation "+string(rune('a'+index)))
	}

	first, err := held.placements.InvestigationEvidence(ctx, held.organization,
		held.subject.ID, investigation.EvidenceFilter{}, investigation.Page{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Items) != 2 || first.Next == "" {
		t.Fatalf("first page returned %d items and next %q", len(first.Items), first.Next)
	}

	// The case grows between the pages, which is what a live case does.
	held.recordEvidence(t, "an item that arrives mid-read")

	second, err := held.placements.InvestigationEvidence(ctx, held.organization,
		held.subject.ID, investigation.EvidenceFilter{},
		investigation.Page{Limit: 2, After: first.Next})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Items) != 2 {
		t.Fatalf("second page returned %d items, want 2", len(second.Items))
	}
	for _, item := range second.Items {
		for _, seen := range first.Items {
			if item.ID == seen.ID {
				t.Errorf("item %s appeared on both pages; the order is not stable", item.ID)
			}
		}
	}
	if second.CaseVersion <= first.CaseVersion {
		t.Errorf("the second page is stamped %d and the first %d; the case grew between them",
			second.CaseVersion, first.CaseVersion)
	}
}

// One assembly, three consumers. Two assemblies at one pinned version must be identical, or a
// shared link can say something the application does not.
func TestInvestigation_AssemblyAtAPinnedVersionIsRepeatableAndRefusesAMovedCase(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	item := held.recordEvidence(t, "the pod restarted nine times")
	held.recordGap(t, "container logs from the previous instance")
	if err := held.placements.RecordOutcome(ctx, held.organization, held.fence(),
		investigation.Outcome{
			Kind: investigation.OutcomeSupported, Statement: "the container exits on start",
			Claims: []investigation.Claim{{
				Ordinal: 1, Role: investigation.ClaimSupporting,
				Statement: "it restarted nine times", Evidence: []uuid.UUID{item.ID},
			}},
		}); err != nil {
		t.Fatalf("recording an outcome: %v", err)
	}

	pinned := held.version(t)
	first, err := held.placements.AssembleCaseFile(
		ctx, held.organization, held.subject.ID, pinned)
	if err != nil {
		t.Fatalf("assembling: %v", err)
	}
	second, err := held.placements.AssembleCaseFile(
		ctx, held.organization, held.subject.ID, pinned)
	if err != nil {
		t.Fatalf("assembling again: %v", err)
	}

	if first.CaseVersion != pinned || second.CaseVersion != pinned {
		t.Errorf("assemblies stamped %d and %d, want both at %d",
			first.CaseVersion, second.CaseVersion, pinned)
	}
	if len(first.Rounds) != 1 || first.Rounds[0].Ordinal != 1 {
		t.Errorf("an assembly must name the rounds it includes, got %d", len(first.Rounds))
	}
	if !identicalAssemblies(first, second) {
		t.Error("two assemblies at one pinned version differ")
	}

	// The summary at the same version must agree with the assembly, or the shared version says
	// something the application does not.
	summary, err := held.placements.InvestigationSummary(
		ctx, held.organization, held.subject.ID)
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	if summary.Counts.Evidence != len(first.Evidence) ||
		summary.Counts.Gaps != len(first.Gaps) {
		t.Errorf("the summary counts %d evidence and %d gaps; the assembly holds %d and %d",
			summary.Counts.Evidence, summary.Counts.Gaps,
			len(first.Evidence), len(first.Gaps))
	}

	held.recordEvidence(t, "something arrives after the pin")
	if _, err = held.placements.AssembleCaseFile(
		ctx, held.organization, held.subject.ID, pinned); !errors.Is(err, investigation.ErrCaseMoved) {
		t.Errorf("assembling at a version the case has passed returned %v, want a refusal", err)
	}
}

// identicalAssemblies compares what two assemblies contain. It is deliberately field by field
// rather than a deep equality over the struct, so that adding a clock to the assembled content
// fails here rather than silently making two pinned exports differ.
func identicalAssemblies(first, second investigation.CaseFile) bool {
	if first.CaseVersion != second.CaseVersion ||
		len(first.Rounds) != len(second.Rounds) ||
		len(first.Evidence) != len(second.Evidence) ||
		len(first.Gaps) != len(second.Gaps) ||
		len(first.Outcomes) != len(second.Outcomes) ||
		len(first.Timeline) != len(second.Timeline) {
		return false
	}
	for index := range first.Evidence {
		if first.Evidence[index].ID != second.Evidence[index].ID ||
			first.Evidence[index].Statement != second.Evidence[index].Statement ||
			first.Evidence[index].Content != second.Evidence[index].Content {
			return false
		}
	}
	for index := range first.Outcomes {
		if first.Outcomes[index].ID != second.Outcomes[index].ID ||
			first.Outcomes[index].Statement != second.Outcomes[index].Statement ||
			len(first.Outcomes[index].Claims) != len(second.Outcomes[index].Claims) {
			return false
		}
	}
	return true
}

// The list read carries what a row renders, in one request, whatever the row count. The frozen
// .NET audit recorded the alternative and its consequence: one request per row.
func TestInvestigation_TheListCarriesCountsAndSummaryFields(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	item := held.recordEvidence(t, "the workload has no ready replicas")
	held.recordGap(t, "node conditions")
	if err := held.placements.RecordOutcome(ctx, held.organization, held.fence(),
		investigation.Outcome{
			Kind: investigation.OutcomeSupported, Statement: "the deployment cannot schedule",
			Claims: []investigation.Claim{{
				Ordinal: 1, Role: investigation.ClaimSupporting,
				Statement: "no replicas are ready", Evidence: []uuid.UUID{item.ID},
			}},
		}); err != nil {
		t.Fatalf("recording an outcome: %v", err)
	}

	list, err := held.placements.ListInvestigations(
		ctx, held.organization, investigation.ListFilter{}, investigation.Page{})
	if err != nil {
		t.Fatalf("listing investigations: %v", err)
	}
	if len(list.Rows) != 1 {
		t.Fatalf("listed %d cases, want 1", len(list.Rows))
	}

	row := list.Rows[0]
	if row.Counts.Evidence != 1 || row.Counts.Gaps != 1 || row.Counts.Rounds != 1 {
		t.Errorf("row counts are %+v; a row must carry them without a second request", row.Counts)
	}
	if row.OutcomeKind != investigation.OutcomeSupported {
		t.Errorf("row outcome is %v, want the case's present tense", row.OutcomeKind)
	}
	if row.OutcomeStatement == "" {
		t.Error("a row must carry the outcome's statement without a second request")
	}
}

// Cancelling stops the case, ends its round, and takes the lease away from whoever held it.
func TestInvestigation_CancellingIsTerminalAndStopsTheWorker(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	if err := held.placements.CancelInvestigation(
		ctx, ownerOf(t, held.organization), held.organization, held.subject.ID); err != nil {
		t.Fatalf("cancelling: %v", err)
	}

	found, err := held.placements.Investigation(ctx, held.organization, held.subject.ID)
	if err != nil {
		t.Fatalf("reading the cancelled case: %v", err)
	}
	if found.Lifecycle != investigation.LifecycleCancelled || !found.Terminal() {
		t.Errorf("a cancelled case is %s; it must be terminal", found.Lifecycle)
	}

	// The worker that was running it can no longer write, which is what makes the stop real.
	if _, err = held.placements.RecordEvidence(ctx, held.organization, held.fence(),
		[]investigation.Item{{
			CapabilityID: "kubernetes.namespace.events", CapabilityVersion: 1,
			Connection: held.connection, Statement: "written after a cancellation",
			Trust: investigation.TrustRelayAttested,
		}}); !errors.Is(err, investigation.ErrLeaseLost) {
		t.Errorf("a cancelled round accepted a write with %v, want ErrLeaseLost", err)
	}

	if err = held.placements.CancelInvestigation(
		ctx, ownerOf(t, held.organization), held.organization, held.subject.ID); !errors.Is(
		err, investigation.ErrAlreadyTerminal) {
		t.Errorf("cancelling twice returned %v, want ErrAlreadyTerminal", err)
	}
}

// A round replays from its own record. The case pack is read by round and consults nothing live,
// which is what makes a surprising conclusion examinable months later.
func TestInvestigation_ARoundReplaysFromItsCasePack(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	brief := investigation.Brief{
		Resource: investigation.ResourceIdentity{
			Kind: "deployment", Name: "checkout", Namespace: "payments",
			UID: "9f1c-uid", Resolved: true,
		},
		Window:      held.subject.Window,
		AssembledAt: time.Now().UTC(),
	}
	if err := held.placements.RecordBrief(
		ctx, held.organization, held.fence(), brief); err != nil {
		t.Fatalf("recording a brief: %v", err)
	}
	hypothesis := held.recordHypothesis(t, "the image tag does not exist")
	item := held.recordEvidence(t, "the kubelet reports ErrImagePull")
	held.recordGap(t, "the registry's own audit log")

	pack, err := held.placements.CasePack(ctx, held.organization, held.round.ID)
	if err != nil {
		t.Fatalf("reading the case pack: %v", err)
	}
	if pack.Round.Brief.Resource.UID != "9f1c-uid" {
		t.Errorf("the pack's brief names uid %q, want the one pinned",
			pack.Round.Brief.Resource.UID)
	}
	if pack.Round.Controls.MaxRequests == 0 {
		t.Error("the pack must carry the resolved control snapshot the round ran under")
	}
	if pack.Round.Versions.Planner == "" || pack.Round.Versions.Model == "" {
		t.Error("the pack must carry the component versions that produced the round")
	}
	if len(pack.Hypotheses) != 1 || pack.Hypotheses[0].ID != hypothesis.ID {
		t.Errorf("the pack holds %d hypotheses, want the one proposed", len(pack.Hypotheses))
	}
	if len(pack.Evidence) != 1 || pack.Evidence[0].ID != item.ID {
		t.Errorf("the pack holds %d evidence items, want the one recorded", len(pack.Evidence))
	}
	if len(pack.Gaps) != 1 {
		t.Errorf("the pack holds %d gaps, want the one recorded", len(pack.Gaps))
	}
}

// The brief is pinned before any hypothesis exists, and the column is NULL until it is — which is
// what makes the ordering observable rather than an assertion about two function calls.
func TestInvestigation_TheBriefExistsBeforeAnyHypothesisDoes(t *testing.T) {
	t.Parallel()
	held := heldInvestigation(t)
	ctx := context.Background()

	pack, err := held.placements.CasePack(ctx, held.organization, held.round.ID)
	if err != nil {
		t.Fatalf("reading the case pack: %v", err)
	}
	if pack.Round.Brief.AssembledAt.IsZero() == false {
		t.Fatal("a round starts with no brief; this test would prove nothing otherwise")
	}
	if len(pack.Hypotheses) != 0 {
		t.Fatal("a round starts with no hypotheses")
	}

	if err = held.placements.RecordBrief(ctx, held.organization, held.fence(),
		investigation.Brief{AssembledAt: time.Now().UTC()}); err != nil {
		t.Fatalf("recording a brief: %v", err)
	}
	if held.lifecycle(t) != investigation.LifecycleReasoning {
		t.Errorf("after the brief the case is %s, want reasoning", held.lifecycle(t))
	}

	// The state that makes the ordering observable: the brief is pinned and the case still holds no
	// hypothesis. It is asserted as durable state rather than by comparing two timestamps, because
	// one of those clocks is this process's and the other is the database's, and an ordering that
	// depends on them agreeing to the millisecond is a flake rather than a property.
	pack, err = held.placements.CasePack(ctx, held.organization, held.round.ID)
	if err != nil {
		t.Fatalf("re-reading the case pack: %v", err)
	}
	if pack.Round.Brief.AssembledAt.IsZero() {
		t.Fatal("the brief must be pinned on the round")
	}
	if len(pack.Hypotheses) != 0 {
		t.Fatalf("the case holds %d hypotheses at the moment the brief was pinned; it must hold "+
			"none", len(pack.Hypotheses))
	}

	held.recordHypothesis(t, "the first hypothesis this case has held")

	pack, err = held.placements.CasePack(ctx, held.organization, held.round.ID)
	if err != nil {
		t.Fatalf("re-reading the case pack: %v", err)
	}
	if pack.Round.Brief.AssembledAt.IsZero() {
		t.Error("pinning a hypothesis must not disturb the brief")
	}
	if len(pack.Hypotheses) != 1 {
		t.Errorf("the case holds %d hypotheses, want the one proposed", len(pack.Hypotheses))
	}
}

// reopen adds a second round to a case and claims it, which is what reinvestigation does. It never
// creates a second case: the identity, the URL and the permalink survive.
func reopen(t *testing.T, held heldCase) heldCase {
	t.Helper()
	ctx := context.Background()

	if _, err := held.placements.OpenRound(ctx, ownerOf(t, held.organization), held.organization, investigation.Opening{
		InvestigationID: held.subject.ID,
		Controls:        investigation.DefaultControls(),
		Plan:            investigation.Plan{Template: "kubernetes-workload-v1"},
		Versions: investigation.Versions{
			Planner: "p1", Model: "recorded", PromptVersion: "1",
			SchemaVersion: "1", Investigator: "test",
		},
	}); err != nil {
		t.Fatalf("opening a second round: %v", err)
	}
	claimed, err := held.placements.ClaimRounds(ctx, held.organization,
		investigation.RoundClaim{SessionID: uuid.New(), LeaseFor: time.Minute, Capacity: 4})
	if err != nil {
		t.Fatalf("claiming the second round: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d rounds, want the new one", len(claimed))
	}
	next := held
	next.round = claimed[0].Round
	next.subject = claimed[0].Investigation
	return next
}

// expireRoundLease models what a control-plane restart leaves behind: a round leased to a session
// that is gone, whose lease then runs out.
func expireRoundLease(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, roundID uuid.UUID,
) {
	t.Helper()
	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	if _, err = pool.Exec(context.Background(),
		`UPDATE investigation_round SET lease_expires_at = now() - interval '1 second'
		  WHERE round_id = $1`, roundID); err != nil {
		t.Fatalf("expiring a round lease: %v", err)
	}
}

// organization2 names a second tenant. The existing helper is called organization, which shadows
// the package-level function inside tests that also have a variable of that name.
func organization2(t *testing.T, id string) tenancy.Organization {
	t.Helper()
	value, err := tenancy.NewOrganization(id)
	if err != nil {
		t.Fatalf("NewOrganization(%q): %v", id, err)
	}
	return value
}
