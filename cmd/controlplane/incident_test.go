package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/intake"
)

// Signals grouping into the operational episode an investigation attaches to.
//
// The seam is the composition root, for the same reason intake's is: what is under test is what an
// operator could observe. Alerts are delivered as real signed requests to the real intake listener,
// and the episodes they produce are read back through the real operator API. Nothing here asserts
// how grouping is implemented, because a second Integration will change that.
//
// The one thing every test below turns on: the grouping identity is the SOURCE's. Two alerts land
// in one episode because the customer's own Alertmanager put them in one group, never because this
// platform decided their labels looked similar.

const incidentToken = "operator-token-for-the-incident-surface"

// incidentPlane is a control plane with both surfaces bound: intake to deliver alerts to, and the
// operator API to read the incidents they became.
type incidentPlane struct {
	*controlPlane
	intake      string
	operator    string
	integration uuid.UUID
	dsn         string
}

func startIncidents(t *testing.T) *incidentPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.IntakeAddress = "127.0.0.1:0"
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(incidentToken))
		cfg.OperatorTokenDigest = digest[:]
		cfg.OperatorTokenOrganization = intakeOrganization
		dsn = cfg.DatabaseDSN
	})

	address := listeningAddress(t, plane, "listening for alert intake")
	integration := configureIntegration(t, dsn, intakeOrganization, intakeSecret)
	return &incidentPlane{
		controlPlane: plane, intake: address, operator: operatorAddress,
		integration: integration, dsn: dsn,
	}
}

func (p *incidentPlane) deliver(t *testing.T, body string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s/intake/v1/integrations/%s/signals", p.intake, p.integration)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("building the delivery: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(intake.TokenHeader, intakeSecret)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("delivering: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func (p *incidentPlane) call(
	t *testing.T, method, path string, body any,
) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	url := "http://" + p.operator + path
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+incidentToken)
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()

	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response from %s: %v", path, err)
	}
	return response.StatusCode, string(answer)
}

// episodeBody mirrors what the surface answers with. It is written out rather than imported so a
// field renamed in the view is a failure here rather than a client silently reading a zero.
type episodeBody struct {
	ID            string `json:"id"`
	IntegrationID string `json:"integrationId"`
	// IntegrationName is what a responder reads: which of this tenant's installations
	// delivered the alerts. The identity beside it is what a link is built from.
	IntegrationName string `json:"integrationName"`
	Title           string `json:"title"`
	Status          string `json:"status"`
	Grouping        struct {
		Basis       string `json:"basis"`
		Explanation string `json:"explanation"`
		Key         string `json:"key"`
	} `json:"grouping"`
	FirstSeenAt     time.Time  `json:"firstSeenAt"`
	LastSeenAt      time.Time  `json:"lastSeenAt"`
	ResolvedAt      *time.Time `json:"resolvedAt"`
	SignalCount     int        `json:"signalCount"`
	InvestigationID *string    `json:"investigationId"`
	Supersession    *struct {
		EpisodeID string `json:"episodeId"`
		Reason    string `json:"reason"`
	} `json:"supersededBy"`
}

type episodeListBody struct {
	Items []episodeBody `json:"items"`
	Next  *string       `json:"next"`
}

type episodeSignalsBody struct {
	Items []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"items"`
	Next *string `json:"next"`
}

func (p *incidentPlane) episodes(t *testing.T, query string) episodeListBody {
	t.Helper()

	path := "/operator/v1/organizations/" + intakeOrganization + "/incidents" + query
	status, body := p.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("listing incidents answered %d: %s", status, body)
	}
	var list episodeListBody
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decoding the incident list: %v\nbody: %s", err, body)
	}
	return list
}

func (p *incidentPlane) episode(t *testing.T, id string) episodeBody {
	t.Helper()

	path := "/operator/v1/organizations/" + intakeOrganization + "/incidents/" + id
	status, body := p.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("reading incident %s answered %d: %s", id, status, body)
	}
	var episode episodeBody
	if err := json.Unmarshal([]byte(body), &episode); err != nil {
		t.Fatalf("decoding the incident: %v\nbody: %s", err, body)
	}
	return episode
}

// grouped renders a v4 payload for one alert under a named group key, which is the identity
// Alertmanager computes from the group_by its own operator wrote.
func grouped(groupKey, fingerprint, alertName string, startsAt time.Time) string {
	return groupedBody(groupKey, fingerprint, alertName, "firing", startsAt, time.Time{})
}

func groupedResolution(
	groupKey, fingerprint, alertName string, startsAt, endsAt time.Time,
) string {
	return groupedBody(groupKey, fingerprint, alertName, "resolved", startsAt, endsAt)
}

func groupedBody(
	groupKey, fingerprint, alertName, status string, startsAt, endsAt time.Time,
) string {
	ends := "0001-01-01T00:00:00Z"
	if !endsAt.IsZero() {
		ends = endsAt.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf(`{
	  "version": "4",
	  "groupKey": %q,
	  "status": %q,
	  "truncatedAlerts": 0,
	  "alerts": [{
	    "status": %q,
	    "fingerprint": %q,
	    "labels": {"alertname": %q, "severity": "critical", "namespace": "payments"},
	    "annotations": {"summary": "something is wrong"},
	    "startsAt": %q,
	    "endsAt": %q
	  }]
	}`, groupKey, status, status, fingerprint, alertName,
		startsAt.Format(time.RFC3339Nano), ends)
}

// ungrouped renders a payload carrying no group key at all, which is what a source that groups
// nothing looks like.
func ungrouped(fingerprint, alertName string, startsAt time.Time) string {
	return fmt.Sprintf(`{
	  "version": "4",
	  "status": "firing",
	  "truncatedAlerts": 0,
	  "alerts": [{
	    "status": "firing",
	    "fingerprint": %q,
	    "labels": {"alertname": %q},
	    "annotations": {"summary": "something is wrong"},
	    "startsAt": %q,
	    "endsAt": "0001-01-01T00:00:00Z"
	  }]
	}`, fingerprint, alertName, startsAt.Format(time.RFC3339Nano))
}

// The sentence the whole slice exists for: a single failure does not open twenty investigations.
func TestIncidents_AlertsTheSourceGroupedBecomeOneEpisode(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	const key = `{}/{severity="critical"}:{alertname="KubePodCrashLooping"}`
	for _, alert := range []struct{ fingerprint, name string }{
		{"fp-pod-crashlooping", "KubePodCrashLooping"},
		{"fp-pod-not-ready", "KubePodNotReady"},
		{"fp-deployment-degraded", "KubeDeploymentReplicasMismatch"},
	} {
		if status := plane.deliver(t, grouped(key, alert.fingerprint, alert.name, began)); status != http.StatusAccepted {
			t.Fatalf("delivering %s answered %d\nlogs:\n%s",
				alert.fingerprint, status, plane.logs.String())
		}
	}

	list := plane.episodes(t, "")
	if len(list.Items) != 1 {
		t.Fatalf("three alerts the source grouped produced %d episodes, want 1: %+v",
			len(list.Items), list.Items)
	}
	episode := list.Items[0]
	if episode.SignalCount != 3 {
		t.Errorf("the episode holds %d signals, want 3", episode.SignalCount)
	}
	if episode.Status != "open" {
		t.Errorf("the episode is %q while its alerts are firing, want open", episode.Status)
	}
	// The grouping is EXPLAINABLE. An operator looking at three alerts in one incident is told who
	// decided that, in the source's own terms, without having to read the code.
	if episode.Grouping.Basis != "source_grouping" {
		t.Errorf("the grouping basis is %q, want source_grouping", episode.Grouping.Basis)
	}
	if episode.Grouping.Key != key {
		t.Errorf("the episode records %q as the source's grouping key, want %q",
			episode.Grouping.Key, key)
	}
	if episode.Grouping.Explanation == "" {
		t.Error("the grouping carries no explanation; a grouping an operator cannot explain is " +
			"one they will argue with rather than act on")
	}
}

// Grouping is CONSERVATIVE. Two failures the customer's own alerting kept apart stay apart here,
// because a wrong merge produces one investigation with an incoherent scope and a wrong split
// produces one redundant record.
func TestIncidents_AlertsTheSourceKeptApartStayApart(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	plane.deliver(t, grouped(`{}:{alertname="KubePodCrashLooping"}`, "fp-a", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(`{}:{alertname="NodeNotReady"}`, "fp-b", "NodeNotReady", began))

	list := plane.episodes(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("two alerts in different groups produced %d episodes, want 2: %+v",
			len(list.Items), list.Items)
	}
}

// A source that supplies no grouping identity gets one episode per alert, and the record says so
// rather than implying somebody grouped them.
func TestIncidents_ASourceThatGroupsNothingGetsAnEpisodePerAlert(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	plane.deliver(t, ungrouped("fp-lonely-one", "DiskFilling", began))
	plane.deliver(t, ungrouped("fp-lonely-two", "MemoryPressure", began))

	list := plane.episodes(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("two ungrouped alerts produced %d episodes, want 2", len(list.Items))
	}
	for _, episode := range list.Items {
		if episode.Grouping.Basis != "ungrouped" {
			t.Errorf("an episode from a source that grouped nothing reports basis %q, want "+
				"ungrouped; claiming a grouping nobody made is this platform inventing an incident",
				episode.Grouping.Basis)
		}
	}
}

// An episode is resolved when every alert in it has stopped, and not before. A record that said a
// failure recovered while part of it was still firing is the worst thing this table could say.
func TestIncidents_AnEpisodeResolvesOnlyWhenEveryAlertInItHas(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	ended := began.Add(5 * time.Minute)

	const key = `{}:{alertname="KubePodCrashLooping"}`
	plane.deliver(t, grouped(key, "fp-one", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(key, "fp-two", "KubePodNotReady", began))

	list := plane.episodes(t, "")
	if len(list.Items) != 1 {
		t.Fatalf("want one episode, got %d", len(list.Items))
	}
	id := list.Items[0].ID

	plane.deliver(t, groupedResolution(key, "fp-one", "KubePodCrashLooping", began, ended))
	if episode := plane.episode(t, id); episode.Status != "open" {
		t.Fatalf("the episode is %q with one alert still firing, want open", episode.Status)
	}

	plane.deliver(t, groupedResolution(key, "fp-two", "KubePodNotReady", began, ended))
	episode := plane.episode(t, id)
	if episode.Status != "resolved" {
		t.Errorf("the episode is %q after every alert stopped, want resolved", episode.Status)
	}
	if episode.ResolvedAt == nil {
		t.Error("a resolved episode carries no resolution time")
	}
	// And the record of what happened survives: the Signals are still there and still readable.
	if episode.SignalCount != 2 {
		t.Errorf("the resolved episode holds %d signals, want the 2 it grouped",
			episode.SignalCount)
	}
}

// A resolved episode releases its grouping key, so the same failure next month is a NEW episode
// rather than the resolved record of the last one being reopened. It is the same rule the Signal
// table already keeps for an alert's own episodes.
func TestIncidents_TheSameFailureAgainOpensANewEpisodeRatherThanReopeningTheOld(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	ended := began.Add(10 * time.Minute)
	again := began.Add(time.Hour)

	const key = `{}:{alertname="DiskFilling"}`
	plane.deliver(t, grouped(key, "fp-disk", "DiskFilling", began))
	plane.deliver(t, groupedResolution(key, "fp-disk", "DiskFilling", began, ended))
	plane.deliver(t, grouped(key, "fp-disk", "DiskFilling", again))

	list := plane.episodes(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("a failure that recurred produced %d episodes, want 2 — one closed and one "+
			"open: %+v", len(list.Items), list.Items)
	}

	var open, resolved int
	for _, episode := range list.Items {
		switch episode.Status {
		case "open":
			open++
		case "resolved":
			resolved++
		}
	}
	if open != 1 || resolved != 1 {
		t.Errorf("got %d open and %d resolved episodes, want one of each", open, resolved)
	}
}

// The Signals an episode grouped are readable, oldest first, because a reader following an
// incident follows it forwards.
func TestIncidents_TheSignalsGroupedIntoAnEpisodeAreReadable(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)

	const key = `{}:{alertname="KubePodCrashLooping"}`
	plane.deliver(t, grouped(key, "fp-first", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(key, "fp-second", "KubePodNotReady", began.Add(time.Minute)))

	id := plane.episodes(t, "").Items[0].ID
	path := "/operator/v1/organizations/" + intakeOrganization + "/incidents/" + id + "/signals"
	status, body := plane.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("reading an episode's signals answered %d: %s", status, body)
	}

	var signals episodeSignalsBody
	if err := json.Unmarshal([]byte(body), &signals); err != nil {
		t.Fatalf("decoding the signals: %v\nbody: %s", err, body)
	}
	if len(signals.Items) != 2 {
		t.Fatalf("the episode reports %d signals, want 2", len(signals.Items))
	}
	if signals.Items[0].Title != "KubePodCrashLooping" {
		t.Errorf("the first signal is %q; an incident reads forwards, oldest first",
			signals.Items[0].Title)
	}
}

// Correcting a grouping does not destroy the record of having made the original one.
func TestIncidents_AMergeRecordsTheCorrectionAndRewritesNothing(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)

	plane.deliver(t, grouped(`{}:{alertname="KubePodCrashLooping"}`, "fp-a", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(`{}:{alertname="KubeDeploymentDegraded"}`, "fp-b", "KubeDeploymentDegraded", began))

	list := plane.episodes(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("want two episodes to merge, got %d", len(list.Items))
	}
	absorbed, surviving := list.Items[0], list.Items[1]

	const reason = "both are the checkout rollout; the deployment degraded because its pods crash"
	path := "/operator/v1/organizations/" + intakeOrganization +
		"/incidents/" + absorbed.ID + "/merge"
	status, body := plane.call(t, http.MethodPost, path,
		map[string]string{"into": surviving.ID, "reason": reason})
	if status != http.StatusOK {
		t.Fatalf("merging answered %d: %s", status, body)
	}

	// NOTHING is rewritten. The absorbed episode keeps its identity, its signals and its own
	// grouping key, and gains a pointer to the one that survives it with the operator's reason.
	after := plane.episode(t, absorbed.ID)
	if after.Supersession == nil {
		t.Fatal("the absorbed episode does not say it was merged")
	}
	if after.Supersession.EpisodeID != surviving.ID {
		t.Errorf("the absorbed episode points at %s, want %s",
			after.Supersession.EpisodeID, surviving.ID)
	}
	if after.Supersession.Reason != reason {
		t.Errorf("the merge reason is %q, want the operator's own words", after.Supersession.Reason)
	}
	if after.SignalCount != absorbed.SignalCount {
		t.Errorf("the absorbed episode now holds %d signals and held %d; a merge that moved "+
			"signals would destroy the record of the grouping it was correcting",
			after.SignalCount, absorbed.SignalCount)
	}
	if after.Grouping.Key != absorbed.Grouping.Key {
		t.Error("the absorbed episode's grouping key changed; the record of why it was its own " +
			"episode is what a reader checks the correction against")
	}
}

// A merge is refused when it would not mean anything, and the refusal says which reason applies —
// the caller is an operator correcting a grouping, and a refusal nobody can act on is a defect.
func TestIncidents_AMergeThatWouldNotMeanAnythingIsRefused(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)

	plane.deliver(t, grouped(`{}:{alertname="A"}`, "fp-a", "A", began))
	plane.deliver(t, grouped(`{}:{alertname="B"}`, "fp-b", "B", began))
	list := plane.episodes(t, "")
	first, second := list.Items[0], list.Items[1]

	base := "/operator/v1/organizations/" + intakeOrganization + "/incidents/"

	// Into itself.
	status, _ := plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": first.ID, "reason": "because"})
	if status != http.StatusBadRequest {
		t.Errorf("merging an episode into itself answered %d, want 400", status)
	}

	// With no reason. A merge nobody explained is a grouping decision a later reader cannot check,
	// which is exactly what recording the basis exists to prevent in the automatic case.
	status, _ = plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": second.ID})
	if status != http.StatusBadRequest {
		t.Errorf("merging with no reason answered %d, want 400", status)
	}

	// Into an episode that does not exist.
	status, _ = plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": uuid.NewString(), "reason": "because"})
	if status != http.StatusNotFound {
		t.Errorf("merging into an episode that does not exist answered %d, want 404", status)
	}

	// And a chain: merging into an episode that has itself been merged. A reader that had to walk
	// a chain would find a different answer depending on where it started.
	if status, body := plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": second.ID, "reason": "they are one"}); status != http.StatusOK {
		t.Fatalf("the first merge answered %d: %s", status, body)
	}
	plane.deliver(t, grouped(`{}:{alertname="C"}`, "fp-c", "C", began))
	var third string
	for _, episode := range plane.episodes(t, "").Items {
		if episode.ID != first.ID && episode.ID != second.ID {
			third = episode.ID
		}
	}
	status, body := plane.call(t, http.MethodPost, base+third+"/merge",
		map[string]string{"into": first.ID, "reason": "chain"})
	if status != http.StatusConflict {
		t.Errorf("merging into an already-merged episode answered %d, want 409: %s", status, body)
	}
}

// A listing that ignored a filter would answer a question nobody asked, and an empty page is
// exactly what "this tenant has none of those" looks like.
func TestIncidents_TheListingRefusesAFilterItCannotServe(t *testing.T) {
	plane := startIncidents(t)

	path := "/operator/v1/organizations/" + intakeOrganization + "/incidents"
	if status, _ := plane.call(t, http.MethodGet, path+"?status=resolvd", nil); status != http.StatusBadRequest {
		t.Errorf("an unserveable status filter answered %d, want 400", status)
	}
	if status, _ := plane.call(t, http.MethodGet, path+"?severity=critical", nil); status != http.StatusBadRequest {
		t.Errorf("an unoffered filter answered %d, want 400", status)
	}
	if status, _ := plane.call(t, http.MethodGet, path+"?sort=whenever", nil); status != http.StatusBadRequest {
		t.Errorf("an unoffered sort answered %d, want 400", status)
	}
}

// An episode belongs to the tenant whose Connection delivered it and to no other. A caller naming
// another organization is answered exactly as one naming an organization that does not exist.
func TestIncidents_AreReachableOnlyByTheTenantWhoseConnectionDeliveredThem(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	plane.deliver(t, grouped(`{}:{alertname="A"}`, "fp-a", "A", began))

	id := plane.episodes(t, "").Items[0].ID
	status, _ := plane.call(t, http.MethodGet,
		"/operator/v1/organizations/org-neighbour/incidents/"+id, nil)
	if status != http.StatusNotFound {
		t.Errorf("reading another tenant's incident answered %d, want 404", status)
	}
}

// A responder arriving from their own alerting wants to know whether to go and look at
// Alertmanager or at something else. The view carried the identity alone, so the only field
// a console could render restated its own label.
func TestIncidents_AnEpisodeNamesTheIntegrationThatDeliveredIt(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)

	if status := plane.deliver(t, grouped(
		"{}:{alertname=\"PaymentsDown\"}", "f-named", "PaymentsDown", began),
	); status != http.StatusAccepted {
		t.Fatalf("delivering answered %d", status)
	}

	// The name the integration was configured with, read back off the same record the
	// listing resolves it from, so this asserts the join rather than a literal.
	want := integrationName(t, plane.dsn, plane.integration)

	listed := plane.episodes(t, "")
	if len(listed.Items) != 1 {
		t.Fatalf("expected one episode, got %d", len(listed.Items))
	}
	for _, episode := range []episodeBody{listed.Items[0], plane.episode(t, listed.Items[0].ID)} {
		if episode.IntegrationID != plane.integration.String() {
			t.Errorf("integrationId = %q, want %s", episode.IntegrationID, plane.integration)
		}
		if episode.IntegrationName != want {
			t.Errorf("integrationName = %q, want %q", episode.IntegrationName, want)
		}
	}
}

// integrationName reads what the integration is actually called, so the assertion above
// compares the served name against the record rather than against a repeated literal.
func integrationName(t *testing.T, dsn string, id uuid.UUID) string {
	t.Helper()
	ctx := context.Background()

	database, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	var name string
	if err := database.QueryRow(ctx,
		`SELECT name FROM integration WHERE integration_id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("reading the integration's name: %v", err)
	}
	return name
}
