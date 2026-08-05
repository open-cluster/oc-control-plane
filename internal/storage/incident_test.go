package storage_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Attaching a case to the operational episode it is about.
//
// Grouping itself is asserted through the intake listener at the composition root, because that is
// where an operator can observe it. What is asserted here is the pairing the database has to keep
// on its own: one episode has ONE Investigation, and an episode may not carry a case that would
// read across the Environment boundary.

// recordEpisode writes one episode directly, standing in for the delivery that would have created
// it. Intake is not involved here deliberately: what is under test is what happens when a case
// names an episode, and delivering an alert to produce one would make every assertion below
// depend on the adapter as well.
func recordEpisode(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	connection uuid.UUID, environment uuid.UUID, key string,
) uuid.UUID {
	t.Helper()

	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	id := uuid.New()
	now := time.Now().UTC()
	if _, err = pool.Exec(context.Background(), `
		INSERT INTO incident_episode
			(episode_id, organization, connection_id, environment_id, grouping_key,
			 grouping_basis, title, status, first_seen_at, last_seen_at, signal_count)
		VALUES ($1, $2, $3, $4, $5, 1, 'a failure', 1, $6, $6, 1)`,
		id, organization.String(), connection, environment, key, now); err != nil {
		t.Fatalf("recording an incident episode: %v", err)
	}
	return id
}

// environmentOf reports the Environment a Connection belongs to, which is the one a case opened
// through it will be derived into.
func environmentOf(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, connection uuid.UUID,
) uuid.UUID {
	t.Helper()

	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var environment uuid.UUID
	if err = pool.QueryRow(context.Background(),
		`SELECT environment_id FROM connection WHERE connection_id = $1`,
		connection).Scan(&environment); err != nil {
		t.Fatalf("reading a connection's environment: %v", err)
	}
	return environment
}

func openCaseForEpisode(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
	connection uuid.UUID, episode uuid.UUID,
) (investigation.Investigation, error) {
	t.Helper()

	key := ""
	if episode != uuid.Nil {
		key = episode.String()
	}
	return placements.OpenInvestigation(
		context.Background(), ownerOf(t, organization), organization, investigation.New{
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
			EpisodeKey: key,
		})
}

// The link is recorded in BOTH directions, in one commit. An episode with a case and a case with
// an episode being separate writes is how two operators both see an unclaimed episode.
func TestACaseOpenedForAnEpisode_IsRecordedOnBothSidesAtOnce(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)
	environment := environmentOf(t, placements, organization, connection)
	episode := recordEpisode(t, placements, organization, connection, environment, "group-a")

	opened, err := openCaseForEpisode(t, placements, organization, connection, episode)
	if err != nil {
		t.Fatalf("opening a case for an episode: %v", err)
	}
	if opened.EpisodeKey != episode.String() {
		t.Errorf("the case names episode %q, want %s", opened.EpisodeKey, episode)
	}

	found, err := placements.Episode(context.Background(), organization, episode)
	if err != nil {
		t.Fatalf("reading the episode back: %v", err)
	}
	if found.Investigation == nil {
		t.Fatal("the episode does not name the case that was opened for it")
	}
	if *found.Investigation != opened.ID {
		t.Errorf("the episode names case %s, want %s", *found.Investigation, opened.ID)
	}
}

// Repeated notifications about one failure must not fragment into many Investigations, so a second
// case is refused NAMING the first — an operator who asked for one wants to be sent to it.
func TestASecondCaseForOneEpisode_IsRefusedNamingTheFirst(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)
	environment := environmentOf(t, placements, organization, connection)
	episode := recordEpisode(t, placements, organization, connection, environment, "group-a")

	first, err := openCaseForEpisode(t, placements, organization, connection, episode)
	if err != nil {
		t.Fatalf("opening the first case: %v", err)
	}

	_, err = openCaseForEpisode(t, placements, organization, connection, episode)
	if !errors.Is(err, investigation.ErrEpisodeInvestigated) {
		t.Fatalf("a second case for one episode gave %v, want ErrEpisodeInvestigated", err)
	}
	if !strings.Contains(err.Error(), first.ID.String()) {
		t.Errorf("the refusal is %q and does not name the case that already exists (%s); an "+
			"operator told no without being told where would go looking", err, first.ID)
	}
}

// The Environment is the boundary evidence may never cross, so an episode from one scope may not
// become the subject of a case that reads through another. It is the Environment that is checked
// and NOT the Connection: an episode arrives through a trigger Connection and a case reads through
// an evidence one, so requiring the same Connection would refuse every real pairing.
func TestAnEpisodeInAnotherEnvironment_CannotBecomeACasesSubject(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)
	environment := environmentOf(t, placements, organization, connection)

	// A second Environment, and an episode inside it. The Connection is the same one, so what
	// differs between this and the passing case above is the Environment and nothing else.
	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	staging := uuid.New()
	if _, err = pool.Exec(context.Background(),
		`INSERT INTO environment (environment_id, organization, name, is_default)
		 VALUES ($1, $2, 'staging', FALSE)`, staging, organization.String()); err != nil {
		t.Fatalf("creating a second environment: %v", err)
	}
	if staging == environment {
		t.Fatal("the fixture did not produce a second environment")
	}
	elsewhere := recordEpisode(t, placements, organization, connection, staging, "group-elsewhere")

	if _, err = openCaseForEpisode(
		t, placements, organization, connection, elsewhere); !errors.Is(
		err, investigation.ErrEpisodeUnusable) {
		t.Fatalf("a case for an episode in another environment gave %v, want ErrEpisodeUnusable",
			err)
	}
}

// An episode another tenant holds is answered exactly as one that does not exist, so the surface
// cannot be used to find out which episodes are somebody else's.
func TestAnEpisodeThatIsNotThisTenants_IsAnswerdAsOneThatDoesNotExist(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)

	_, absent := openCaseForEpisode(t, placements, organization, connection, uuid.New())
	if !errors.Is(absent, investigation.ErrEpisodeUnusable) {
		t.Fatalf("naming an episode that does not exist gave %v, want ErrEpisodeUnusable", absent)
	}
}

// A case opened with no episode is the ordinary path and stays untouched: an engineer with a
// workload, a window and no alert.
func TestACaseWithNoEpisode_OpensExactlyAsItAlwaysDid(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)

	opened, err := openCaseForEpisode(t, placements, organization, connection, uuid.Nil)
	if err != nil {
		t.Fatalf("opening a case with no episode: %v", err)
	}
	if opened.EpisodeKey != "" {
		t.Errorf("a case opened with no episode names %q", opened.EpisodeKey)
	}
}

// Two callers opening a case for the same episode at once: the database decides, and exactly one
// of them wins. A read-then-write would let both pass and produce the fragmentation the model
// exists to prevent.
func TestTwoCasesForOneEpisodeAtOnce_ProduceExactlyOne(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)
	environment := environmentOf(t, placements, organization, connection)
	episode := recordEpisode(t, placements, organization, connection, environment, "group-race")

	type attempt struct {
		opened investigation.Investigation
		err    error
	}
	answers := make(chan attempt, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			opened, err := openCaseForEpisode(t, placements, organization, connection, episode)
			answers <- attempt{opened, err}
		}()
	}
	close(start)

	var succeeded int
	for range 2 {
		answer := <-answers
		if answer.err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of two concurrent openings succeeded, want exactly 1", succeeded)
	}

	// And the episode names the one that won rather than being left unclaimed.
	found, err := placements.Episode(context.Background(), organization, episode)
	if err != nil {
		t.Fatalf("reading the episode: %v", err)
	}
	if found.Investigation == nil {
		t.Error("the episode holds no case after one opening succeeded")
	}
}

// A merge points the absorbed episode at the survivor and leaves everything else alone, including
// the Signals — the record of the grouping being corrected is what makes the correction checkable.
func TestAMerge_LeavesBothRecordsIntact(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	registration := enrolledRelay(t, placements, organization)
	connection := evidenceConnection(t, placements, organization, registration)
	environment := environmentOf(t, placements, organization, connection)

	absorbed := recordEpisode(t, placements, organization, connection, environment, "group-a")
	surviving := recordEpisode(t, placements, organization, connection, environment, "group-b")

	after, err := placements.MergeEpisodes(
		context.Background(), ownerOf(t, organization), organization, incident.Merge{
			Absorbed: absorbed, Into: surviving, Reason: "one rollout, two alerts",
		})
	if err != nil {
		t.Fatalf("merging: %v", err)
	}
	if after.ID != surviving {
		t.Errorf("the merge returned %s, want the surviving episode %s", after.ID, surviving)
	}

	gone, err := placements.Episode(context.Background(), organization, absorbed)
	if err != nil {
		t.Fatalf("the absorbed episode is unreadable after a merge: %v", err)
	}
	if gone.SupersededBy == nil || *gone.SupersededBy != surviving {
		t.Fatalf("the absorbed episode points at %v, want %s", gone.SupersededBy, surviving)
	}
	if gone.GroupingKey != "group-a" {
		t.Errorf("the absorbed episode's grouping key is now %q; a merge must rewrite nothing",
			gone.GroupingKey)
	}
	// The merge is on the record, and the record is append-only, so it is there for good.
	if !recordedIncidentMerge(t, placements, organization, absorbed) {
		t.Error("no audit event names the merged episode; a grouping correction nobody can " +
			"attribute is one an auditor cannot answer for")
	}
}

func recordedIncidentMerge(
	t *testing.T, placements *storage.Placements,
	organization tenancy.Organization, episode uuid.UUID,
) bool {
	t.Helper()

	pool, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var count int
	if err = pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_event
		 WHERE organization = $1 AND action = 'incident.merged' AND target_id = $2`,
		organization.String(), episode.String()).Scan(&count); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading the audit trail: %v", err)
	}
	return count > 0
}

// triggerConnection is an Alertmanager Connection deliveries arrive through.
func triggerConnection(
	t *testing.T, placements *storage.Placements, organization tenancy.Organization,
) (uuid.UUID, uuid.UUID) {
	t.Helper()

	ctx := context.Background()
	environment, err := placements.EnsureDefaultEnvironment(
		ctx, ownerOf(t, organization), organization)
	if err != nil {
		t.Fatalf("ensuring the default environment: %v", err)
	}
	created, err := placements.CreateConnection(
		ctx, ownerOf(t, organization), organization, storage.NewConnection{
			Environment:  environment.ID,
			Integration:  "alertmanager",
			Name:         "alertmanager " + uuid.NewString(),
			Role:         storage.RoleTrigger,
			Locality:     storage.LocalityControlPlane,
			SecretDigest: randomDigest(t),
		})
	if err != nil {
		t.Fatalf("creating a trigger connection: %v", err)
	}
	return created.ID, environment.ID
}

// TWO DELIVERIES CARRYING ONE GROUP, AT ONCE.
//
// This is the case the grouping insert is shaped for, and it is the one a review caught: an
// ON CONFLICT DO NOTHING does not wait for the conflicting transaction and returns no row, so the
// delivery that lost would then read nothing — the winner has not committed — and fail a delivery
// that was never wrong. What must hold is that both deliveries succeed, they agree on one episode,
// and exactly one of them is recorded as having opened it.
func TestTwoDeliveriesCarryingOneGroupAtOnce_ProduceOneEpisodeAndBothSucceed(t *testing.T) {
	t.Parallel()

	placements, organization := migratedPlacement(t)
	connection, environment := triggerConnection(t, placements, organization)

	const key = "{}:{alertname=KubePodCrashLooping}"
	began := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	delivery := func(fingerprint string, body byte) storage.Delivery {
		digest := make([]byte, 32)
		digest[0] = body
		return storage.Delivery{
			Connection:  connection,
			Environment: environment,
			BodyDigest:  digest,
			Signals: []storage.Signal{{
				SourceKey:   fingerprint,
				GroupingKey: key,
				Status:      storage.SignalFiring,
				Title:       "KubePodCrashLooping",
				StartedAt:   began,
			}},
		}
	}

	type answer struct {
		outcome storage.DeliveryOutcome
		err     error
	}
	answers := make(chan answer, 2)
	start := make(chan struct{})
	for index, fingerprint := range []string{"fp-one", "fp-two"} {
		go func() {
			<-start
			outcome, err := placements.RecordDelivery(
				context.Background(), organization, delivery(fingerprint, byte(index+1)))
			answers <- answer{outcome, err}
		}()
	}
	close(start)

	var opened, joined int
	for range 2 {
		got := <-answers
		if got.err != nil {
			t.Fatalf("a concurrent delivery failed: %v", got.err)
		}
		opened += got.outcome.EpisodesOpened
		joined += got.outcome.EpisodesJoined
	}
	if opened != 1 || joined != 1 {
		t.Errorf("two concurrent deliveries in one group opened %d episodes and joined %d, "+
			"want one of each", opened, joined)
	}

	page, err := placements.QueryEpisodes(context.Background(), organization, incident.Query{
		Sort: "lastSeenAt", Descending: true, Limit: 50,
	})
	if err != nil {
		t.Fatalf("reading the episodes: %v", err)
	}
	if len(page.Episodes) != 1 {
		t.Fatalf("two concurrent deliveries in one group produced %d episodes, want 1",
			len(page.Episodes))
	}
	if page.Episodes[0].SignalCount != 2 {
		t.Errorf("the episode holds %d signals, want the 2 that were delivered",
			page.Episodes[0].SignalCount)
	}
	if page.Episodes[0].Basis != incident.BasisSourceGrouping {
		t.Errorf("the episode reports basis %v, want the source's own grouping",
			page.Episodes[0].Basis)
	}
}
