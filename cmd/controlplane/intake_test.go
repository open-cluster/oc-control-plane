package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/intake"
)

// Intake is the boundary between a customer's alerting and this platform, and the thing that
// must not be faked is the credential check. So these run against the assembled process: real
// HTTP, a real database, real deliveries.
//
// The seam is deliberately the same one everything else here uses. An adapter is reached by
// delivering a request, because that is how it is reached in production.

const (
	intakeOrganization = "org-a"
	intakeSecret       = "a-source-secret-long-enough-to-be-one"
)

// intakePlane is a control plane with intake listening, plus a configured Integration to
// deliver through.
type intakePlane struct {
	*controlPlane
	address     string
	integration uuid.UUID
	dsn         string
}

func startIntake(t *testing.T) *intakePlane {
	t.Helper()

	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.IntakeAddress = "127.0.0.1:0"
		dsn = cfg.Placements["shared"]
	})

	address := listeningAddress(t, plane, "listening for alert intake")
	integration := configureIntegration(t, dsn, intakeOrganization, intakeSecret)
	return &intakePlane{
		controlPlane: plane, address: address,
		integration: integration, dsn: dsn,
	}
}

// alertmanagerTypeID mirrors the seeded integration_type row. Written out rather than
// imported so that renaming the constant in code cannot silently change what a configured
// row in the database means.
const alertmanagerTypeID = 1

// listeningAddress pulls a surface's bound address out of the startup log, which is the only
// place an ephemeral port is reported for a listener the test did not open itself.
func listeningAddress(t *testing.T, plane *controlPlane, message string) string {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, entry := range plane.logs.logLines(t) {
			if entry["msg"] == message {
				if address, ok := entry["address"].(string); ok && address != "" {
					return address
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q line appeared\nlogs:\n%s", message, plane.logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// configureIntegration records an Alertmanager Integration, storing only the digest of its
// webhook secret. It writes the row directly rather than going through the operator API:
// what these tests are about is the delivery path, and a second surface between them and it
// would mean a failure here could be either one.
func configureIntegration(t *testing.T, dsn, organization, secret string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	database, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	id := uuid.New()
	digest := sha256.Sum256([]byte(secret))
	_, err = database.Exec(ctx, `
		INSERT INTO integration
			(integration_id, org_id, integration_type_id, name,
			 webhook_secret_digest, webhook_secret_fingerprint, webhook_secret_created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())`,
		id, organization, alertmanagerTypeID, "the source "+id.String(), digest[:],
		id.String()[:8])
	if err != nil {
		t.Fatalf("configuring the integration: %v", err)
	}
	return id
}

// deliver posts a body to intake with the given secret, and reports the status.
func (p *intakePlane) deliver(t *testing.T, secret, body string) int {
	t.Helper()

	url := fmt.Sprintf("http://%s/intake/v1/integrations/%s/signals", p.address, p.integration)
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if secret != "" {
		request.Header.Set(intake.TokenHeader, secret)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

// signals reports what is durably recorded for the organization.
func (p *intakePlane) signals(t *testing.T) []recordedSignal {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	rows, err := connection.Query(ctx, `
		SELECT source_key, status, title, summary, labels, started_at, resolved_at, received_at
		  FROM signal WHERE org_id = $1 ORDER BY source_key, started_at`, intakeOrganization)
	if err != nil {
		t.Fatalf("reading signals: %v", err)
	}
	defer rows.Close()

	var recorded []recordedSignal
	for rows.Next() {
		var signal recordedSignal
		var labels []byte
		if err = rows.Scan(&signal.SourceKey, &signal.Status, &signal.Title,
			&signal.Summary, &labels, &signal.StartedAt, &signal.ResolvedAt,
			&signal.ReceivedAt); err != nil {
			t.Fatalf("scanning signal: %v", err)
		}
		if err = json.Unmarshal(labels, &signal.Labels); err != nil {
			t.Fatalf("decoding labels: %v", err)
		}
		recorded = append(recorded, signal)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("reading signals: %v", err)
	}
	return recorded
}

// truncatedCount reports how many omitted alerts the recorded deliveries carry.
func (p *intakePlane) truncatedCount(t *testing.T) int {
	t.Helper()
	ctx := context.Background()

	connection, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()

	var total int
	err = connection.QueryRow(ctx,
		`SELECT coalesce(sum(truncated), 0) FROM integration_delivery
		  WHERE org_id = $1 AND outcome = 1`,
		intakeOrganization).Scan(&total)
	if err != nil {
		t.Fatalf("reading truncation counts: %v", err)
	}
	return total
}

// scopes reports which tenant and Integration each recorded Signal landed under, across
// EVERY organization in the database rather than one. Scoping the query to the expected
// tenant would make a signal written to the wrong one invisible, which is the failure being
// tested for.
func (p *intakePlane) scopes(t *testing.T) []recordedScope {
	t.Helper()
	ctx := context.Background()

	database, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	rows, err := database.Query(ctx,
		`SELECT org_id, integration_id FROM signal ORDER BY received_at`)
	if err != nil {
		t.Fatalf("reading signal scopes: %v", err)
	}
	defer rows.Close()

	var recorded []recordedScope
	for rows.Next() {
		var scope recordedScope
		if err = rows.Scan(&scope.organization, &scope.integration); err != nil {
			t.Fatalf("scanning a signal scope: %v", err)
		}
		recorded = append(recorded, scope)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("reading signal scopes: %v", err)
	}
	return recorded
}

// setDisabled turns this plane's Integration off or back on.
func (p *intakePlane) setDisabled(t *testing.T, disabled bool) {
	t.Helper()
	ctx := context.Background()

	database, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	var at *time.Time
	if disabled {
		now := time.Now()
		at = &now
	}
	if _, err = database.Exec(ctx,
		`UPDATE integration SET disabled_at = $2 WHERE integration_id = $1`,
		p.integration, at); err != nil {
		t.Fatalf("setting the disabled state: %v", err)
	}
}

type recordedScope struct {
	organization string
	integration  uuid.UUID
}

type recordedSignal struct {
	SourceKey  string
	Status     int16
	Title      string
	Summary    string
	Labels     map[string]string
	StartedAt  time.Time
	ResolvedAt *time.Time
	ReceivedAt time.Time
}

// firing renders a v4 webhook payload for an episode that began at startsAt and has not ended.
func firing(fingerprint string, startsAt time.Time) string {
	return alertmanagerBody(fingerprint, "firing", startsAt, time.Time{}, 0)
}

// resolved renders the resolution of the episode that began at startsAt. Alertmanager sends
// the original start time on a resolution, which is what lets it resolve the episode it
// belongs to rather than opening a second one.
func resolved(fingerprint string, startsAt, endsAt time.Time) string {
	return alertmanagerBody(fingerprint, "resolved", startsAt, endsAt, 0)
}

// alertmanagerBody renders a v4 webhook payload for one episode of one alert.
func alertmanagerBody(
	fingerprint, status string, startsAt, endsAt time.Time, truncated int,
) string {
	ends := "0001-01-01T00:00:00Z"
	if !endsAt.IsZero() {
		ends = endsAt.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf(`{
	  "version": "4",
	  "groupKey": "{}:{alertname=NodeNotReady}",
	  "status": %q,
	  "truncatedAlerts": %d,
	  "alerts": [{
	    "status": %q,
	    "fingerprint": %q,
	    "labels": {"alertname": "NodeNotReady", "severity": "critical", "namespace": "payments"},
	    "annotations": {"summary": "the node stopped reporting"},
	    "startsAt": %q,
	    "endsAt": %q
	  }]
	}`, status, truncated, status, fingerprint,
		startsAt.Format(time.RFC3339Nano), ends)
}

// The sentence: a correctly authenticated delivery becomes a durable, normalised Signal.
func TestIntake_AcceptsASignedDeliveryAndNormalisesIt(t *testing.T) {
	plane := startIntake(t)
	// The alert started well before this delivery on purpose: it is what lets the clock
	// assertion below distinguish the receiver's own clock from the source's, without
	// comparing two machines' clocks at sub-second precision.
	observed := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	status := plane.deliver(t, intakeSecret, firing("fp-node-not-ready", observed))
	if status != http.StatusAccepted {
		t.Fatalf("a correctly authenticated delivery = %d, want 202\nlogs:\n%s",
			status, plane.logs.String())
	}

	recorded := plane.signals(t)
	if len(recorded) != 1 {
		t.Fatalf("recorded %d signals, want 1", len(recorded))
	}
	signal := recorded[0]

	if signal.SourceKey != "fp-node-not-ready" {
		t.Errorf("source key = %q; deduplication must use the source's own identity",
			signal.SourceKey)
	}
	if signal.Status != 1 {
		t.Errorf("status = %d, want firing", signal.Status)
	}
	if signal.Title != "NodeNotReady" {
		t.Errorf("title = %q, want the alert name", signal.Title)
	}
	if signal.Summary != "the node stopped reporting" {
		t.Errorf("summary = %q", signal.Summary)
	}
	if signal.Labels["severity"] != "critical" || signal.Labels["namespace"] != "payments" {
		t.Errorf("labels did not survive normalisation: %v", signal.Labels)
	}
	if !signal.StartedAt.Equal(observed) {
		t.Errorf("started at %s, want the source's own time %s", signal.StartedAt, observed)
	}
	if signal.ResolvedAt != nil {
		t.Errorf("a firing signal carries a resolution time %s", signal.ResolvedAt)
	}
	// Both clocks are kept. Collapsing them would make a delayed delivery indistinguishable
	// from a delayed failure, and an investigator reasons about ordering. The alert started
	// ten minutes ago, so a received time copied from the source's clock would sit ten
	// minutes early — far outside any honest skew between this process and the database.
	if signal.ReceivedAt.Sub(signal.StartedAt) < 5*time.Minute {
		t.Errorf("received at %s, near the source's own start time (%s); the two clocks were "+
			"collapsed", signal.ReceivedAt, signal.StartedAt)
	}
}

// The credential check is the thing that must not be faked, so it is asserted in both
// directions: a wrong secret and an absent one are refused, and nothing is written.
func TestIntake_RefusesADeliveryWithoutTheSourcesSecret(t *testing.T) {
	plane := startIntake(t)
	body := firing("fp-1", time.Now().UTC())

	cases := []struct {
		name   string
		secret string
	}{
		{name: "no token at all", secret: ""},
		{name: "the wrong token", secret: "not-the-configured-secret-but-long"},
		// A near-miss. It does not test the constant-time comparison — the comparison is over
		// SHA-256 digests, so a prefix of the secret hashes to something unrelated and any
		// comparison at all rejects it. What it does test is that the secret is checked whole
		// rather than by prefix somewhere above the digest.
		{name: "a prefix of the token", secret: intakeSecret[:len(intakeSecret)-1]},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if status := plane.deliver(t, testCase.secret, body); status != http.StatusUnauthorized {
				t.Errorf("delivery with %s = %d, want 401", testCase.name, status)
			}
		})
	}

	if recorded := plane.signals(t); len(recorded) != 0 {
		t.Errorf("an unauthenticated delivery produced %d signals", len(recorded))
	}
}

// At-least-once webhooks retry. A retry must not produce a second anything, and must be
// answered so the source stops rather than retrying again.
func TestIntake_RedeliveryProducesNoSecondSignal(t *testing.T) {
	plane := startIntake(t)
	body := firing("fp-same", time.Now().UTC())

	if status := plane.deliver(t, intakeSecret, body); status != http.StatusAccepted {
		t.Fatalf("first delivery = %d, want 202", status)
	}
	if status := plane.deliver(t, intakeSecret, body); status != http.StatusOK {
		t.Errorf("redelivery = %d, want 200 so the source stops retrying", status)
	}

	if recorded := plane.signals(t); len(recorded) != 1 {
		t.Errorf("redelivery produced %d signals, want 1", len(recorded))
	}
}

// A resolution updates the episode it resolves and does not erase when that episode began.
//
// The firing time is the assertion that matters. It is what an investigator needs to reason
// about ordering, and it is the one an implementation that overwrites the row wholesale
// destroys while still looking correct on the status.
func TestIntake_AResolutionUpdatesTheEpisodeItResolves(t *testing.T) {
	plane := startIntake(t)
	firedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	endedAt := time.Now().UTC().Truncate(time.Second)

	if status := plane.deliver(t, intakeSecret,
		firing("fp-resolving", firedAt)); status != http.StatusAccepted {
		t.Fatalf("firing delivery = %d, want 202", status)
	}
	if status := plane.deliver(t, intakeSecret,
		resolved("fp-resolving", firedAt, endedAt)); status != http.StatusAccepted {
		t.Fatalf("resolving delivery = %d, want 202", status)
	}

	recorded := plane.signals(t)
	if len(recorded) != 1 {
		t.Fatalf("a resolution produced %d signals, want the one episode it resolves", len(recorded))
	}
	signal := recorded[0]

	if signal.Status != 2 {
		t.Errorf("status = %d, want resolved", signal.Status)
	}
	if !signal.StartedAt.Equal(firedAt) {
		t.Errorf("started at %s after resolution, want the original %s; the resolution erased "+
			"when it fired", signal.StartedAt, firedAt)
	}
	if signal.ResolvedAt == nil || !signal.ResolvedAt.Equal(endedAt) {
		t.Errorf("resolved at %v, want %s", signal.ResolvedAt, endedAt)
	}
}

// The same alert firing again is a new episode, not an overwrite of the last one.
//
// This is the property the source key alone cannot carry: Alertmanager's fingerprint is a hash
// of the label set, so the same disk filling up next month arrives under the same one. Keyed on
// it alone, a re-fire silently destroys the resolved record of the previous occurrence, and the
// history an investigator opens is missing the thing they came to look at.
func TestIntake_ARefireIsANewEpisodeNotAnOverwrite(t *testing.T) {
	plane := startIntake(t)
	firstStart := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	firstEnd := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	secondStart := time.Now().UTC().Truncate(time.Second)

	for _, body := range []string{
		firing("fp-recurring", firstStart),
		resolved("fp-recurring", firstStart, firstEnd),
		firing("fp-recurring", secondStart),
	} {
		if status := plane.deliver(t, intakeSecret, body); status != http.StatusAccepted {
			t.Fatalf("delivery = %d, want 202", status)
		}
	}

	recorded := plane.signals(t)
	if len(recorded) != 2 {
		t.Fatalf("the same alert firing twice produced %d signals, want an episode each",
			len(recorded))
	}
	// Ordered by started_at, so the first is the occurrence that already ended.
	if recorded[0].Status != 2 || !recorded[0].StartedAt.Equal(firstStart) {
		t.Errorf("the earlier episode is status %d started %s; the re-fire overwrote it",
			recorded[0].Status, recorded[0].StartedAt)
	}
	if recorded[1].Status != 1 || !recorded[1].StartedAt.Equal(secondStart) {
		t.Errorf("the later episode is status %d started %s, want firing at %s",
			recorded[1].Status, recorded[1].StartedAt, secondStart)
	}
}

// Webhooks are at-least-once AND unordered, so a redelivery of the firing can arrive after the
// resolution that ended it. It must not resurrect a resolved episode.
func TestIntake_ALateFiringDoesNotResurrectAResolvedEpisode(t *testing.T) {
	plane := startIntake(t)
	startedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	endedAt := time.Now().UTC().Truncate(time.Second)

	if status := plane.deliver(t, intakeSecret,
		resolved("fp-late", startedAt, endedAt)); status != http.StatusAccepted {
		t.Fatalf("resolving delivery = %d, want 202", status)
	}
	// The same episode's firing, arriving late. A different body, so the delivery digest does
	// not deduplicate it — this has to be caught by the model, not by the retry guard.
	if status := plane.deliver(t, intakeSecret,
		firing("fp-late", startedAt)); status != http.StatusAccepted {
		t.Fatalf("late firing delivery = %d, want 202", status)
	}

	recorded := plane.signals(t)
	if len(recorded) != 1 {
		t.Fatalf("recorded %d signals, want the one episode", len(recorded))
	}
	if recorded[0].Status != 2 {
		t.Errorf("status = %d; a late firing resurrected a resolved episode", recorded[0].Status)
	}
}

// A source that truncated its own payload has told us the record is incomplete. Refusing would
// lose the alerts that did arrive, since it will not send them again — so it is accepted, and
// what was omitted is recorded rather than inferred from a count that looks fine.
func TestIntake_RecordsWhatTheSourceSaysItLeftOut(t *testing.T) {
	plane := startIntake(t)
	startedAt := time.Now().UTC().Truncate(time.Second)

	body := alertmanagerBody("fp-truncated", "firing", startedAt, time.Time{}, 17)
	if status := plane.deliver(t, intakeSecret, body); status != http.StatusAccepted {
		t.Fatalf("a truncated delivery = %d, want 202: the alerts that arrived are real", status)
	}

	if got := plane.truncatedCount(t); got != 17 {
		t.Errorf("recorded %d omitted alerts, want 17; a truncated delivery is "+
			"indistinguishable from a complete one", got)
	}
}

// A database outage must answer retryable. Answering 401 would tell Alertmanager the delivery
// was permanently rejected, and the alert it would otherwise have retried is gone.
func TestIntake_ADatabaseOutageIsRetryableNotUnauthorized(t *testing.T) {
	plane := startIntake(t)
	body := firing("fp-outage", time.Now().UTC())

	if status := plane.deliver(t, intakeSecret, body); status != http.StatusAccepted {
		t.Fatalf("the delivery must work before an outage means anything, got %d", status)
	}

	plane.database.closeGate()
	defer plane.database.openGate()

	if status := plane.deliver(t, intakeSecret, body); status != http.StatusServiceUnavailable {
		t.Errorf("a delivery during a database outage = %d, want 503; anything in the 4xx "+
			"range tells the source to stop retrying and the alert is lost", status)
	}
}

// A malformed payload behind a valid credential is refused permanently and writes nothing.
// The status matters as much as the refusal: 4xx tells Alertmanager to stop, and answering
// 5xx would turn one bad payload into a retry storm.
func TestIntake_RefusesAMalformedPayloadWithoutAPartialWrite(t *testing.T) {
	plane := startIntake(t)

	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: "this is not json"},
		{name: "no alerts", body: `{"version":"4","alerts":[]}`},
		{name: "no fingerprint to identify the alert by", body: `{"alerts":[
			{"status":"firing","labels":{"alertname":"X"},"startsAt":"2026-07-29T10:00:00Z"}]}`},
		{name: "a status this adapter does not know", body: `{"alerts":[
			{"status":"flapping","fingerprint":"fp","startsAt":"2026-07-29T10:00:00Z"}]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if status := plane.deliver(t, intakeSecret, testCase.body); status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400 so the source does not retry it", testCase.name, status)
			}
		})
	}

	if recorded := plane.signals(t); len(recorded) != 0 {
		t.Errorf("a malformed payload produced %d signals", len(recorded))
	}
}

// One unusable alert fails its whole delivery rather than being dropped from it. Accepting the
// rest would leave the source told it succeeded while part of what it sent vanished, and it
// will never send that part again.
func TestIntake_OneUnusableAlertRefusesTheWholeDelivery(t *testing.T) {
	plane := startIntake(t)

	body := `{"alerts":[
		{"status":"firing","fingerprint":"fp-good","labels":{"alertname":"Good"},
		 "startsAt":"2026-07-29T10:00:00Z"},
		{"status":"firing","labels":{"alertname":"NoFingerprint"},
		 "startsAt":"2026-07-29T10:00:00Z"}]}`

	if status := plane.deliver(t, intakeSecret, body); status != http.StatusBadRequest {
		t.Errorf("a delivery with one unusable alert = %d, want 400", status)
	}
	if recorded := plane.signals(t); len(recorded) != 0 {
		t.Errorf("a partially unusable delivery wrote %d signals; it must write none or all",
			len(recorded))
	}
}

// An oversized payload is refused without being buffered whole.
func TestIntake_RefusesAnOversizedPayload(t *testing.T) {
	plane := startIntake(t)

	// Two megabytes of valid JSON, over the one-megabyte bound.
	var body bytes.Buffer
	body.WriteString(`{"alerts":[{"status":"firing","fingerprint":"fp","labels":{"pad":"`)
	body.WriteString(strings.Repeat("x", 2<<20))
	body.WriteString(`"},"startsAt":"2026-07-29T10:00:00Z"}]}`)

	if status := plane.deliver(t, intakeSecret, body.String()); status != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized delivery = %d, want 413", status)
	}
	if recorded := plane.signals(t); len(recorded) != 0 {
		t.Errorf("an oversized delivery produced %d signals", len(recorded))
	}
}

// A delivery names its Integration and nothing else, so the tenancy question changes
// shape: there is no longer a path parameter to get wrong, and what has to be proven is
// that the Signal lands under the organization of the Integration row rather than under
// anything a caller could influence.
//
// Both organizations share one placement deliberately. An organization with no placement
// fails before any query runs, which would leave this passing against an implementation
// with no scoping at all — the exact defect it exists to catch.
func TestIntake_ADeliveryLandsUnderItsIntegrationsTenantAndNoOther(t *testing.T) {
	const neighbour = "org-neighbour"

	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.IntakeAddress = "127.0.0.1:0"
		cfg.Assignments[neighbour] = "shared"
		dsn = cfg.Placements["shared"]
	})
	address := listeningAddress(t, plane, "listening for alert intake")

	// Two Integrations in two organizations on one database, each with its own secret
	// digest — which here is the same secret, so a scoping mistake would be invisible if
	// the lookup leaked between them.
	mine := configureIntegration(t, dsn, intakeOrganization, intakeSecret)
	theirs := configureIntegration(t, dsn, neighbour, intakeSecret)
	owner := &intakePlane{
		controlPlane: plane, address: address,
		integration: mine, dsn: dsn,
	}

	if status := owner.deliver(t, intakeSecret, firing("fp-x", time.Now().UTC())); status !=
		http.StatusAccepted {
		t.Fatalf("the delivery = %d, want 202", status)
	}

	recorded := owner.scopes(t)
	if len(recorded) != 1 {
		t.Fatalf("one delivery recorded %d signals across every tenant, want 1", len(recorded))
	}
	if recorded[0].organization != intakeOrganization {
		t.Errorf("the signal landed under %q, want the integration's own %q",
			recorded[0].organization, intakeOrganization)
	}
	if recorded[0].integration != mine {
		t.Errorf("the signal names integration %s, want %s", recorded[0].integration, mine)
	}
	if recorded[0].integration == theirs {
		t.Error("the delivery reached the neighbouring tenant's record")
	}
}

// Two Integrations of one type, each with its own secret. This is the shape a customer
// running production and staging Alertmanager has, and it is the whole reason the
// Integration Type and the Integration are separate concepts: one adapter, two records,
// two credentials.
func TestIntake_TwoIntegrationsOneTypeEachWithItsOwnSecret(t *testing.T) {
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.IntakeAddress = "127.0.0.1:0"
		dsn = cfg.Placements["shared"]
	})
	address := listeningAddress(t, plane, "listening for alert intake")

	const stagingSecret = "a-different-secret-that-is-long-enough"
	production := configureIntegration(t, dsn, intakeOrganization, intakeSecret)
	staging := configureIntegration(t, dsn, intakeOrganization, stagingSecret)

	both := map[string]*intakePlane{
		"production": {controlPlane: plane, address: address,
			integration: production, dsn: dsn},
		"staging": {controlPlane: plane, address: address,
			integration: staging, dsn: dsn},
	}

	// Each accepts its own secret.
	if status := both["production"].deliver(t, intakeSecret,
		firing("fp-prod", time.Now().UTC())); status != http.StatusAccepted {
		t.Errorf("production with its own secret = %d, want 202", status)
	}
	if status := both["staging"].deliver(t, stagingSecret,
		firing("fp-staging", time.Now().UTC())); status != http.StatusAccepted {
		t.Errorf("staging with its own secret = %d, want 202", status)
	}

	// And refuses the other's, in both directions.
	if status := both["staging"].deliver(t, intakeSecret,
		firing("fp-x", time.Now().UTC())); status != http.StatusUnauthorized {
		t.Errorf("production's secret on staging = %d, want 401", status)
	}
	if status := both["production"].deliver(t, stagingSecret,
		firing("fp-y", time.Now().UTC())); status != http.StatusUnauthorized {
		t.Errorf("staging's secret on production = %d, want 401", status)
	}

	// Each Signal names the Integration that delivered it, and nothing crossed.
	scopes := both["production"].scopes(t)
	if len(scopes) != 2 {
		t.Fatalf("two accepted deliveries recorded %d signals, want 2", len(scopes))
	}
	byIntegration := map[uuid.UUID]int{}
	for _, scope := range scopes {
		byIntegration[scope.integration]++
	}
	if byIntegration[production] != 1 || byIntegration[staging] != 1 {
		t.Fatalf("the signals did not land one per integration: %v", byIntegration)
	}
}

// An Integration an operator turned off refuses deliveries. It is still a row — disabling
// is not deleting, so the record of what it produced survives — but nothing new arrives
// through it.
func TestIntake_ADisabledIntegrationRefusesDeliveries(t *testing.T) {
	plane := startIntake(t)

	if status := plane.deliver(t, intakeSecret, firing("fp-before", time.Now().UTC())); status !=
		http.StatusAccepted {
		t.Fatalf("the delivery before disabling = %d, want 202", status)
	}
	plane.setDisabled(t, true)

	if status := plane.deliver(t, intakeSecret, firing("fp-after", time.Now().UTC())); status !=
		http.StatusUnauthorized {
		t.Errorf("a delivery to a disabled connection = %d, want 401", status)
	}
	if recorded := plane.signals(t); len(recorded) != 1 {
		t.Errorf("a disabled connection recorded %d signals, want only the one from before",
			len(recorded))
	}
}

// Nothing intake logs may carry the payload or the secret. The payload is untrusted text from
// a customer's systems, and a log that quoted either would turn diagnosis into a disclosure.
func TestIntake_LogsNeitherThePayloadNorTheSecret(t *testing.T) {
	plane := startIntake(t)

	const marker = "a-very-distinctive-string-from-the-payload"
	body := fmt.Sprintf(`{"alerts":[{"status":"firing","fingerprint":"fp-log",
		"labels":{"alertname":"X"},"annotations":{"summary":%q},
		"startsAt":"2026-07-29T10:00:00Z"}]}`, marker)

	if status := plane.deliver(t, intakeSecret, body); status != http.StatusAccepted {
		t.Fatalf("the accepted delivery = %d, want 202", status)
	}
	if status := plane.deliver(t, "the-wrong-secret-entirely-and-long", body); status != http.StatusUnauthorized {
		t.Fatalf("the refused delivery = %d, want 401", status)
	}

	logs := plane.logs.String()
	// Both deliveries must have been logged at all. Asserting only that the payload is absent
	// would pass against a surface that logged nothing, which is not the property — the
	// refusal in particular has to be investigable.
	if !strings.Contains(logs, "delivery accepted") {
		t.Error("an accepted delivery was not logged")
	}
	if !strings.Contains(logs, "delivery refused") {
		t.Error("a refused delivery was not logged; a credential guess must leave a trace")
	}
	if strings.Contains(logs, marker) {
		t.Error("a log line carried the alert payload")
	}
	if strings.Contains(logs, intakeSecret) {
		t.Error("a log line carried the source's secret")
	}
	if strings.Contains(logs, "the-wrong-secret-entirely-and-long") {
		t.Error("a log line carried a presented credential")
	}
}
