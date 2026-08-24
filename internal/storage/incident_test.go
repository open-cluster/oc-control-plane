package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The operational episode, at the storage seam. Grouping itself is asserted through the
// intake listener at the composition root, because that is where an operator can observe
// it. What is asserted here is what the database keeps on its own: a merge that rewrites
// nothing, and two concurrent deliveries agreeing on one episode.

// recordEpisode writes one episode directly, standing in for the delivery that would have
// created it. Intake is not involved here deliberately: delivering an alert to produce one
// would make every assertion below depend on the adapter as well.
func recordEpisode(
	t *testing.T, database *storage.Database, organization tenancy.Organization,
	integration uuid.UUID, key string,
) uuid.UUID {
	t.Helper()

	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	id := uuid.New()
	now := time.Now().UTC()
	if _, err = pool.Exec(context.Background(), `
		INSERT INTO incident_episode
			(episode_id, org_id, integration_id, grouping_key,
			 grouping_basis, title, status, first_seen_at, last_seen_at, signal_count)
		VALUES ($1, $2, $3, $4, 1, 'a failure', 1, $5, $5, 1)`,
		id, organization.String(), integration, key, now); err != nil {
		t.Fatalf("recording an incident episode: %v", err)
	}
	return id
}

// A merge points the absorbed episode at the survivor and leaves everything else alone,
// including the Signals — the record of the grouping being corrected is what makes the
// correction checkable.
func TestAMerge_LeavesBothRecordsIntact(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	registration := enrolledRelay(t, database, organization)
	integration := kubernetesIntegration(t, database, organization, registration)

	absorbed := recordEpisode(t, database, organization, integration, "group-a")
	surviving := recordEpisode(t, database, organization, integration, "group-b")

	after, err := database.MergeEpisodes(
		context.Background(), ownerOf(t, organization), organization, incident.Merge{
			Absorbed: absorbed, Into: surviving, Reason: "one rollout, two alerts",
		})
	if err != nil {
		t.Fatalf("merging: %v", err)
	}
	if after.ID != surviving {
		t.Errorf("the merge returned %s, want the surviving episode %s", after.ID, surviving)
	}

	gone, err := database.Episode(context.Background(), organization, absorbed)
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
	if !recordedIncidentMerge(t, database, organization, absorbed) {
		t.Error("no audit event names the merged episode; a grouping correction nobody can " +
			"attribute is one an auditor cannot answer for")
	}
}

func recordedIncidentMerge(
	t *testing.T, database *storage.Database,
	organization tenancy.Organization, episode uuid.UUID,
) bool {
	t.Helper()

	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatalf("Pool: %v", err)
	}
	var count int
	if err = pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_event
		 WHERE org_id = $1 AND action = 'incident.merged' AND target_id = $2`,
		organization.String(), episode.String()).Scan(&count); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading the audit trail: %v", err)
	}
	return count > 0
}

// alertmanagerIntegration is an Alertmanager Integration deliveries arrive through.
func alertmanagerIntegration(
	t *testing.T, database *storage.Database, organization tenancy.Organization,
) uuid.UUID {
	t.Helper()

	created, err := database.CreateIntegration(
		context.Background(), ownerOf(t, organization), organization,
		integrations.NewIntegration{
			Type:                     integrations.TypeAlertmanager,
			Name:                     "alertmanager " + uuid.NewString(),
			WebhookSecretDigest:      randomDigest(t),
			WebhookSecretFingerprint: "fingerprint",
		})
	if err != nil {
		t.Fatalf("creating an alertmanager integration: %v", err)
	}
	return created.ID
}

// TWO DELIVERIES CARRYING ONE GROUP, AT ONCE.
//
// This is the case the grouping insert is shaped for, and it is the one a review caught:
// an ON CONFLICT DO NOTHING does not wait for the conflicting transaction and returns no
// row, so the delivery that lost would then read nothing — the winner has not committed —
// and fail a delivery that was never wrong. What must hold is that both deliveries
// succeed, they agree on one episode, and exactly one of them is recorded as having opened
// it.
func TestTwoDeliveriesCarryingOneGroupAtOnce_ProduceOneEpisodeAndBothSucceed(t *testing.T) {
	t.Parallel()

	database, organization := migratedDatabase(t)
	integration := alertmanagerIntegration(t, database, organization)

	const key = "{}:{alertname=KubePodCrashLooping}"
	began := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	delivery := func(fingerprint string, body byte) storage.Delivery {
		digest := make([]byte, 32)
		digest[0] = body
		return storage.Delivery{
			Integration: integration,
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
			outcome, err := database.RecordDelivery(
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

	page, err := database.QueryEpisodes(context.Background(), organization, incident.Query{
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
