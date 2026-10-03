package controlplane

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

	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
	intake "github.com/open-cluster/oc-control-plane/internal/webhooks"
)

type incidentPlane struct {
	*controlPlane
	intake      string
	api         string
	integration uuid.UUID
	dsn         string
}

func startIncidents(t *testing.T) *incidentPlane {
	t.Helper()

	apiAddress := freeAddress(t)
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = "127.0.0.1:0"
		cfg.HTTPListenAddress = apiAddress
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.BootstrapTokenDigest = digest[:]
		dsn = cfg.DatabaseDSN
	})

	address := listeningAddress(t, plane, "listening for alert intake")
	integration := configureIntegration(t, dsn, intakeOrganization, intakeSecret)
	return &incidentPlane{
		controlPlane: plane, intake: address, api: apiAddress,
		integration: integration, dsn: dsn,
	}
}

func (p *incidentPlane) deliver(t *testing.T, body string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s/webhooks/v1/integrations/%s/alert-events", p.intake, p.integration)
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
	url := "http://" + p.api + path
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.AddCookie(&http.Cookie{Name: session.CookieName, Value: p.sessionCookie})
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		request.Header.Set("Origin", "http://"+p.api)
	}
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

type incidentBody struct {
	ID              string `json:"id"`
	IntegrationID   string `json:"integrationId"`
	IntegrationName string `json:"integrationName"`
	Title           string `json:"title"`
	Status          string `json:"status"`
	Grouping        struct {
		Basis       string `json:"basis"`
		Explanation string `json:"explanation"`
		Key         string `json:"key"`
	} `json:"grouping"`
	FirstSeenAt        time.Time  `json:"firstSeenAt"`
	LastSeenAt         time.Time  `json:"lastSeenAt"`
	ResolvedAt         *time.Time `json:"resolvedAt"`
	AlertEventCount    int        `json:"alertEventCount"`
	PostmortemEligible bool       `json:"postmortemEligible"`
	InvestigationID    *string    `json:"investigationId"`
	Supersession       *struct {
		IncidentID string `json:"incidentId"`
		Reason     string `json:"reason"`
	} `json:"supersededBy"`
}

type incidentListBody struct {
	Items []incidentBody `json:"items"`
	Next  *string        `json:"next"`
}

type incidentAlertEventsBody struct {
	Items []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"items"`
	Next *string `json:"next"`
}

func (p *incidentPlane) incidents(t *testing.T, query string) incidentListBody {
	t.Helper()

	path := "/api/v1/incidents" + query
	status, body := p.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("listing incidents answered %d: %s", status, body)
	}
	var list incidentListBody
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decoding the incident list: %v\nbody: %s", err, body)
	}
	return list
}

func (p *incidentPlane) incident(t *testing.T, id string) incidentBody {
	t.Helper()

	path := "/api/v1/incidents/" + id
	status, body := p.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("reading incident %s answered %d: %s", id, status, body)
	}
	var incident incidentBody
	if err := json.Unmarshal([]byte(body), &incident); err != nil {
		t.Fatalf("decoding the incident: %v\nbody: %s", err, body)
	}
	return incident
}

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

func TestIncidents_AlertsTheSourceGroupedBecomeOneIncident(t *testing.T) {
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

	list := plane.incidents(t, "")
	if len(list.Items) != 1 {
		t.Fatalf("three alerts the source grouped produced %d incidents, want 1: %+v",
			len(list.Items), list.Items)
	}
	incident := list.Items[0]
	if incident.AlertEventCount != 3 {
		t.Errorf("the incident holds %d alertEvents, want 3", incident.AlertEventCount)
	}
	if incident.Status != "open" {
		t.Errorf("the incident is %q while its alerts are firing, want open", incident.Status)
	}
	if incident.Grouping.Basis != "source_grouping" {
		t.Errorf("the grouping basis is %q, want source_grouping", incident.Grouping.Basis)
	}
	if incident.Grouping.Key != key {
		t.Errorf("the incident records %q as the source's grouping key, want %q",
			incident.Grouping.Key, key)
	}
	if incident.Grouping.Explanation == "" {
		t.Error("the grouping carries no explanation; a grouping an operator cannot explain is " +
			"one they will argue with rather than act on")
	}
}

func TestIncidents_AlertsTheSourceKeptApartStayApart(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	plane.deliver(t, grouped(`{}:{alertname="KubePodCrashLooping"}`, "fp-a", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(`{}:{alertname="NodeNotReady"}`, "fp-b", "NodeNotReady", began))

	list := plane.incidents(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("two alerts in different groups produced %d incidents, want 2: %+v",
			len(list.Items), list.Items)
	}
}

func TestIncidents_ASourceThatGroupsNothingGetsAnIncidentPerAlert(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	plane.deliver(t, ungrouped("fp-lonely-one", "DiskFilling", began))
	plane.deliver(t, ungrouped("fp-lonely-two", "MemoryPressure", began))

	list := plane.incidents(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("two ungrouped alerts produced %d incidents, want 2", len(list.Items))
	}
	for _, incident := range list.Items {
		if incident.Grouping.Basis != "ungrouped" {
			t.Errorf("an incident from a source that grouped nothing reports basis %q, want "+
				"ungrouped; claiming a grouping nobody made is this platform inventing an incident",
				incident.Grouping.Basis)
		}
	}
}

func TestIncidents_AnIncidentResolvesOnlyWhenEveryAlertInItHas(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	ended := began.Add(5 * time.Minute)

	const key = `{}:{alertname="KubePodCrashLooping"}`
	plane.deliver(t, grouped(key, "fp-one", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(key, "fp-two", "KubePodNotReady", began))

	list := plane.incidents(t, "")
	if len(list.Items) != 1 {
		t.Fatalf("want one incident, got %d", len(list.Items))
	}
	id := list.Items[0].ID

	plane.deliver(t, groupedResolution(key, "fp-one", "KubePodCrashLooping", began, ended))
	if incident := plane.incident(t, id); incident.Status != "open" {
		t.Fatalf("the incident is %q with one alert still firing, want open", incident.Status)
	}

	plane.deliver(t, groupedResolution(key, "fp-two", "KubePodNotReady", began, ended))
	incident := plane.incident(t, id)
	if incident.Status != "resolved" {
		t.Errorf("the incident is %q after every alert stopped, want resolved", incident.Status)
	}
	if incident.ResolvedAt == nil {
		t.Error("a resolved incident carries no resolution time")
	}
	if incident.AlertEventCount != 2 {
		t.Errorf("the resolved incident holds %d alertEvents, want the 2 it grouped",
			incident.AlertEventCount)
	}
}

func TestIncidents_TheSameFailureAgainOpensANewIncidentRatherThanReopeningTheOld(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	ended := began.Add(10 * time.Minute)
	again := began.Add(time.Hour)

	const key = `{}:{alertname="DiskFilling"}`
	plane.deliver(t, grouped(key, "fp-disk", "DiskFilling", began))
	plane.deliver(t, groupedResolution(key, "fp-disk", "DiskFilling", began, ended))
	plane.deliver(t, grouped(key, "fp-disk", "DiskFilling", again))

	list := plane.incidents(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("a failure that recurred produced %d incidents, want 2 — one closed and one "+
			"open: %+v", len(list.Items), list.Items)
	}

	var open, resolved int
	for _, incident := range list.Items {
		switch incident.Status {
		case "open":
			open++
		case "resolved":
			resolved++
		}
	}
	if open != 1 || resolved != 1 {
		t.Errorf("got %d open and %d resolved incidents, want one of each", open, resolved)
	}
}

func TestIncidents_TheAlertEventsGroupedIntoAnIncidentAreReadable(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)

	const key = `{}:{alertname="KubePodCrashLooping"}`
	plane.deliver(t, grouped(key, "fp-first", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(key, "fp-second", "KubePodNotReady", began.Add(time.Minute)))

	id := plane.incidents(t, "").Items[0].ID
	path := "/api/v1/incidents/" + id + "/alert-events"
	status, body := plane.call(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("reading an incident's alertEvents answered %d: %s", status, body)
	}

	var alertEvents incidentAlertEventsBody
	if err := json.Unmarshal([]byte(body), &alertEvents); err != nil {
		t.Fatalf("decoding the alertEvents: %v\nbody: %s", err, body)
	}
	if len(alertEvents.Items) != 2 {
		t.Fatalf("the incident reports %d alertEvents, want 2", len(alertEvents.Items))
	}
	if alertEvents.Items[0].Title != "KubePodCrashLooping" {
		t.Errorf("the first Alert Event is %q; an incident reads forwards, oldest first",
			alertEvents.Items[0].Title)
	}
}

func TestIncidents_AMergeRecordsTheCorrectionAndRewritesNothing(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)

	plane.deliver(t, grouped(`{}:{alertname="KubePodCrashLooping"}`, "fp-a", "KubePodCrashLooping", began))
	plane.deliver(t, grouped(`{}:{alertname="KubeDeploymentDegraded"}`, "fp-b", "KubeDeploymentDegraded", began))

	list := plane.incidents(t, "")
	if len(list.Items) != 2 {
		t.Fatalf("want two incidents to merge, got %d", len(list.Items))
	}
	absorbed, surviving := list.Items[0], list.Items[1]

	const reason = "both are the checkout rollout; the deployment degraded because its pods crash"
	path := "/api/v1/incidents/" + absorbed.ID + "/merge"
	status, body := plane.call(t, http.MethodPost, path,
		map[string]string{"into": surviving.ID, "reason": reason})
	if status != http.StatusOK {
		t.Fatalf("merging answered %d: %s", status, body)
	}

	after := plane.incident(t, absorbed.ID)
	if after.Supersession == nil {
		t.Fatal("the absorbed incident does not say it was merged")
	}
	if after.Supersession.IncidentID != surviving.ID {
		t.Errorf("the absorbed incident points at %s, want %s",
			after.Supersession.IncidentID, surviving.ID)
	}
	if after.Supersession.Reason != reason {
		t.Errorf("the merge reason is %q, want the operator's own words", after.Supersession.Reason)
	}
	if after.AlertEventCount != absorbed.AlertEventCount {
		t.Errorf("the absorbed incident now holds %d alertEvents and held %d; a merge that moved "+
			"alertEvents would destroy the record of the grouping it was correcting",
			after.AlertEventCount, absorbed.AlertEventCount)
	}
	if after.Grouping.Key != absorbed.Grouping.Key {
		t.Error("the absorbed incident's grouping key changed; the record of why it was its own " +
			"incident is what a reader checks the correction against")
	}
}

func TestIncidents_AMergeThatWouldNotMeanAnythingIsRefused(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)

	plane.deliver(t, grouped(`{}:{alertname="A"}`, "fp-a", "A", began))
	plane.deliver(t, grouped(`{}:{alertname="B"}`, "fp-b", "B", began))
	list := plane.incidents(t, "")
	first, second := list.Items[0], list.Items[1]

	base := "/api/v1/incidents/"

	status, _ := plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": first.ID, "reason": "because"})
	if status != http.StatusBadRequest {
		t.Errorf("merging an incident into itself answered %d, want 400", status)
	}

	status, _ = plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": second.ID})
	if status != http.StatusBadRequest {
		t.Errorf("merging with no reason answered %d, want 400", status)
	}

	status, _ = plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": uuid.NewString(), "reason": "because"})
	if status != http.StatusNotFound {
		t.Errorf("merging into an incident that does not exist answered %d, want 404", status)
	}

	if status, body := plane.call(t, http.MethodPost, base+first.ID+"/merge",
		map[string]string{"into": second.ID, "reason": "they are one"}); status != http.StatusOK {
		t.Fatalf("the first merge answered %d: %s", status, body)
	}
	plane.deliver(t, grouped(`{}:{alertname="C"}`, "fp-c", "C", began))
	var third string
	for _, incident := range plane.incidents(t, "").Items {
		if incident.ID != first.ID && incident.ID != second.ID {
			third = incident.ID
		}
	}
	status, body := plane.call(t, http.MethodPost, base+third+"/merge",
		map[string]string{"into": first.ID, "reason": "chain"})
	if status != http.StatusConflict {
		t.Errorf("merging into an already-merged incident answered %d, want 409: %s", status, body)
	}
}

func TestIncidents_TheListingRefusesAFilterItCannotServe(t *testing.T) {
	plane := startIncidents(t)

	path := "/api/v1/incidents"
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

func TestIncidents_AnIncidentNamesTheIntegrationThatDeliveredIt(t *testing.T) {
	plane := startIncidents(t)
	began := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)

	if status := plane.deliver(t, grouped(
		"{}:{alertname=\"PaymentsDown\"}", "f-named", "PaymentsDown", began),
	); status != http.StatusAccepted {
		t.Fatalf("delivering answered %d", status)
	}

	want := integrationName(t, plane.dsn, plane.integration)

	listed := plane.incidents(t, "")
	if len(listed.Items) != 1 {
		t.Fatalf("expected one incident, got %d", len(listed.Items))
	}
	for _, incident := range []incidentBody{listed.Items[0], plane.incident(t, listed.Items[0].ID)} {
		if incident.IntegrationID != plane.integration.String() {
			t.Errorf("integrationId = %q, want %s", incident.IntegrationID, plane.integration)
		}
		if incident.IntegrationName != want {
			t.Errorf("integrationName = %q, want %q", incident.IntegrationName, want)
		}
	}
}

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
