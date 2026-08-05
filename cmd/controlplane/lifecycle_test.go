package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/operator"
)

// The Connection lifecycle end to end: validate with a partial result, revise with a revision
// number, a credential that no read path returns, and a delete that refuses when something
// depends on it.

type detailBody struct {
	connectionBody
	State                 string         `json:"state"`
	ConfigurationRevision int            `json:"configurationRevision"`
	Configuration         map[string]any `json:"configuration"`
	Credential            *struct {
		Method      string     `json:"method"`
		Reference   string     `json:"reference"`
		Fingerprint string     `json:"fingerprint"`
		CreatedAt   time.Time  `json:"createdAt"`
		RotatedAt   *time.Time `json:"rotatedAt"`
	} `json:"credential"`
	Trigger *struct {
		DeliveryEndpoint   string         `json:"deliveryEndpoint"`
		Observed           bool           `json:"observed"`
		LastReceivedAt     *time.Time     `json:"lastReceivedAt"`
		LastAcceptedAt     *time.Time     `json:"lastAcceptedAt"`
		Accepted           int            `json:"accepted"`
		Duplicates         int            `json:"duplicates"`
		Rejected           int            `json:"rejected"`
		RejectionsByReason map[string]int `json:"rejectionsByReason"`
		WindowDays         int            `json:"windowDays"`
	} `json:"trigger"`
}

type validationBody struct {
	ID                    string `json:"id"`
	ConfigurationRevision int    `json:"configurationRevision"`
	Outcome               string `json:"outcome"`
	Authentication        string `json:"authentication"`
	AuthenticationReason  string `json:"authenticationReason"`
	Capabilities          []struct {
		Capability string `json:"capability"`
		Readiness  string `json:"readiness"`
		Reason     string `json:"reason"`
	} `json:"capabilities"`
	Note string `json:"note"`
}

type validationListBody struct {
	Items []validationBody `json:"items"`
	Next  *string          `json:"next"`
}

type deliveryListBody struct {
	Items []struct {
		ID          string `json:"id"`
		Outcome     string `json:"outcome"`
		Reason      string `json:"reason"`
		SignalCount int    `json:"signalCount"`
		Test        bool   `json:"test"`
	} `json:"items"`
}

type dependentsBody struct {
	Error      string `json:"error"`
	Dependents struct {
		Signals                   int  `json:"signals"`
		Deliveries                int  `json:"deliveries"`
		Jobs                      int  `json:"jobs"`
		LastEvidenceInEnvironment bool `json:"lastEvidenceInEnvironment"`
		Any                       bool `json:"any"`
	} `json:"dependents"`
}

func TestConnectionLifecycle(t *testing.T) {
	plane := startConnectionPlane(t)
	base := plane.base(surfaceOrg)
	environment := plane.defaultEnvironment(t, surfaceOrg)
	connections := base + "/connections"

	// A trigger Connection to exercise delivery health against, and an evidence one bound to the
	// enrolled relay so validation has something real to check.
	var trigger, evidence createdConnectionBody

	t.Run("a connection is created against a definition that declares no configuration", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId": environment.ID,
			"integration":   "alertmanager",
			"name":          "Lifecycle Alertmanager",
			"role":          "trigger",
			"locality":      "control_plane",
		})
		if status != http.StatusCreated {
			t.Fatalf("creating = %d: %s", status, body)
		}
		decodeInto(t, body, &trigger)
		if trigger.Secret == "" {
			t.Fatal("no secret was shown at creation")
		}
	})

	t.Run("a configuration field the definition does not declare is refused", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId": environment.ID,
			"integration":   "alertmanager",
			"name":          "Misspelled",
			"role":          "trigger",
			"locality":      "control_plane",
			"configuration": map[string]any{"notAField": "typo"},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("a misspelled configuration field = %d, want 400: %s", status, body)
		}
	})

	t.Run("the detail route carries state, revision and credential metadata", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, connections+"/"+trigger.Connection.ID, nil)
		if status != http.StatusOK {
			t.Fatalf("reading = %d: %s", status, body)
		}
		var detail detailBody
		decodeInto(t, body, &detail)

		if detail.State != "configured" {
			t.Errorf("state = %q, want configured: nothing has checked it yet", detail.State)
		}
		if detail.ConfigurationRevision != 1 {
			t.Errorf("configurationRevision = %d, want 1", detail.ConfigurationRevision)
		}
		if len(detail.Configuration) != 0 {
			t.Errorf("configuration = %v; alertmanager declares no fields, because everything "+
				"about it is already on the Connection record", detail.Configuration)
		}
		if detail.Credential == nil {
			t.Fatal("a trigger connection reports no credential metadata (story 21)")
		}
		if detail.Credential.Method != "shared_secret" || detail.Credential.Fingerprint == "" {
			t.Errorf("credential metadata = %+v; an operator cannot tell one credential from "+
				"the next", *detail.Credential)
		}
		if detail.Credential.RotatedAt != nil {
			t.Error("a credential that has never been rotated reports a rotation")
		}
		if strings.Contains(body, trigger.Secret) {
			t.Fatal("the detail route returned the secret")
		}
	})

	t.Run("revising increments the revision and returns the connection to configured", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPatch, connections+"/"+trigger.Connection.ID,
			map[string]any{
				"labels": map[string]string{"team": "platform"},
			})
		if status != http.StatusOK {
			t.Fatalf("revising = %d: %s", status, body)
		}
		var revised detailBody
		decodeInto(t, body, &revised)
		if revised.ConfigurationRevision != 2 {
			t.Errorf("configurationRevision = %d after one revision, want 2 (story 24)",
				revised.ConfigurationRevision)
		}
		if revised.Labels["team"] != "platform" {
			t.Errorf("the revision did not take: %v", revised.Labels)
		}
	})

	t.Run("rotating a credential changes its fingerprint and stamps the rotation", func(t *testing.T) {
		before := plane.detail(t, connections+"/"+trigger.Connection.ID)

		status, body := plane.call(t, http.MethodPost,
			connections+"/"+trigger.Connection.ID+"/trigger/rotate-secret", nil)
		if status != http.StatusOK {
			t.Fatalf("rotating = %d: %s", status, body)
		}

		after := plane.detail(t, connections+"/"+trigger.Connection.ID)
		if after.Credential.Fingerprint == before.Credential.Fingerprint {
			t.Error("the fingerprint did not change, so an operator cannot tell whether the " +
				"rotation they asked for happened (story 22)")
		}
		if after.Credential.RotatedAt == nil {
			t.Error("the rotation was not stamped")
		}
	})

	t.Run("validating a trigger connection reports what it did not check", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			connections+"/"+trigger.Connection.ID+"/validate", nil)
		if status != http.StatusOK {
			t.Fatalf("validating = %d: %s", status, body)
		}
		var result validationBody
		decodeInto(t, body, &result)

		// Alertmanager is reached inbound only. A pass would be a green tick that meant nothing,
		// so authentication is `not_attempted` with the reason — which is the honest answer and
		// the one an operator can act on.
		if result.Authentication != "not_attempted" {
			t.Errorf("authentication = %q for an inbound-only provider, want not_attempted",
				result.Authentication)
		}
		if result.AuthenticationReason == "" {
			t.Error("nothing says why authentication was not attempted")
		}
		if result.Note == "" {
			t.Error("the result carries no note saying what it does not establish")
		}
		// A run that exercised nothing is PARTIAL and never `passed`, and it leaves the
		// Connection's state alone. An empty capability list counted as "everything available"
		// would hand an inbound-only provider a green tick resting on zero checks.
		if result.Outcome != "partial" {
			t.Errorf("outcome = %q for a run that attempted nothing, want partial", result.Outcome)
		}
		if state := plane.detail(t, connections+"/"+trigger.Connection.ID).State; state != "configured" {
			t.Errorf("the connection is %q after a check that ran nothing, want it left at "+
				"configured: a state moved by a check that did not happen is a claim nobody made",
				state)
		}
		if result.ConfigurationRevision != 2 {
			t.Errorf("the result names revision %d, want the current 2; a result that does not "+
				"name its revision keeps looking current after the configuration changed",
				result.ConfigurationRevision)
		}
	})

	t.Run("an evidence connection validates against what its relay advertises", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId":       environment.ID,
			"integration":         "kubernetes",
			"name":                "Lifecycle Cluster",
			"role":                "evidence",
			"locality":            "relay",
			"relayRegistrationId": plane.relay.registration.String(),
			"configuration":       map[string]any{"namespaceAllowList": "payments,checkout"},
		})
		if status != http.StatusCreated {
			t.Fatalf("creating an evidence connection = %d: %s", status, body)
		}
		decodeInto(t, body, &evidence)

		// The provider-specific configuration round-trips through the schema unchanged.
		configured := plane.detail(t, connections+"/"+evidence.Connection.ID)
		if configured.Configuration["namespaceAllowList"] != "payments,checkout" {
			t.Errorf("configuration = %v, want what was submitted", configured.Configuration)
		}

		status, body = plane.call(t, http.MethodPost,
			connections+"/"+evidence.Connection.ID+"/validate", nil)
		if status != http.StatusOK {
			t.Fatalf("validating = %d: %s", status, body)
		}
		var result validationBody
		decodeInto(t, body, &result)

		if len(result.Capabilities) == 0 {
			t.Fatal("a kubernetes connection validated with no per-capability result; a boolean " +
				"is exactly what story 18 asks not to be given")
		}
		for _, readiness := range result.Capabilities {
			switch readiness.Readiness {
			case "available", "unauthorized", "unavailable", "not_attempted":
			default:
				t.Errorf("%s reports readiness %q, which is not the coverage vocabulary",
					readiness.Capability, readiness.Readiness)
			}
			if readiness.Readiness != "available" && readiness.Reason == "" {
				t.Errorf("%s is %q with no reason", readiness.Capability, readiness.Readiness)
			}
		}
		// Whatever the relay advertised, the outcome has to be one of the three and the
		// Connection's state has to agree with it.
		switch result.Outcome {
		case "passed", "partial", "failed":
		default:
			t.Errorf("outcome = %q", result.Outcome)
		}
		detail := plane.detail(t, connections+"/"+evidence.Connection.ID)
		wanted := map[string]string{"passed": "active", "partial": "degraded", "failed": "failed"}
		if detail.State != wanted[result.Outcome] {
			t.Errorf("the connection is %q after a %q validation, want %q",
				detail.State, result.Outcome, wanted[result.Outcome])
		}
	})

	t.Run("validation history records every run", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet,
			connections+"/"+trigger.Connection.ID+"/validations", nil)
		if status != http.StatusOK {
			t.Fatalf("reading validations = %d: %s", status, body)
		}
		var history validationListBody
		decodeInto(t, body, &history)
		if len(history.Items) == 0 {
			t.Fatal("no validation history, so an outage cannot be told from a configuration " +
				"that never worked (story 19)")
		}
	})

	t.Run("delivery health tells a rejected source from a quiet one", func(t *testing.T) {
		if code := plane.deliverThrough(t, trigger.Connection.ID, "the-wrong-secret"); code !=
			http.StatusUnauthorized {
			t.Fatalf("delivering with a wrong secret = %d, want 401", code)
		}

		detail := plane.detail(t, connections+"/"+trigger.Connection.ID)
		if detail.Trigger == nil {
			t.Fatal("a trigger connection reports no delivery health")
		}
		if detail.Trigger.Rejected == 0 {
			t.Error("a delivery refused for a wrong secret was not counted; a source delivering " +
				"and being rejected looks exactly like a quiet night (story 29)")
		}
		if detail.Trigger.RejectionsByReason["unauthenticated"] == 0 {
			t.Errorf("nothing says WHY it was refused: %v", detail.Trigger.RejectionsByReason)
		}
		if detail.Trigger.LastReceivedAt == nil {
			t.Error("nothing was received according to health, and a delivery was just made")
		}
		if detail.Trigger.LastAcceptedAt != nil {
			t.Error("a refused delivery counted as accepted; the two timestamps exist precisely " +
				"so that cannot happen (story 28)")
		}
	})

	t.Run("a real delivery is accepted and separates the two timestamps", func(t *testing.T) {
		// The secret was rotated above, so a fresh one is minted here and used immediately.
		status, body := plane.call(t, http.MethodPost,
			connections+"/"+trigger.Connection.ID+"/trigger/rotate-secret", nil)
		if status != http.StatusOK {
			t.Fatalf("rotating = %d: %s", status, body)
		}
		var rotated struct {
			Secret string `json:"secret"`
		}
		decodeInto(t, body, &rotated)

		if code := plane.deliverThrough(t, trigger.Connection.ID, rotated.Secret); code !=
			http.StatusAccepted {
			t.Fatalf("delivering with the current secret = %d, want 202", code)
		}

		detail := plane.detail(t, connections+"/"+trigger.Connection.ID)
		if detail.Trigger.Accepted == 0 {
			t.Error("an accepted delivery was not counted")
		}
		if detail.Trigger.LastAcceptedAt == nil {
			t.Error("nothing was accepted according to health, and a delivery just was")
		}
	})

	t.Run("a test event says what it proves and what it does not", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			connections+"/"+trigger.Connection.ID+"/trigger/test-event", nil)
		if status != http.StatusAccepted {
			t.Fatalf("sending a test event = %d, want 202: %s", status, body)
		}
		var answer struct {
			Recorded     bool   `json:"recorded"`
			Proves       string `json:"proves"`
			DoesNotProve string `json:"doesNotProve"`
		}
		decodeInto(t, body, &answer)
		if !answer.Recorded || answer.Proves == "" || answer.DoesNotProve == "" {
			t.Errorf("a test event that does not say what it establishes is a result somebody "+
				"will believe means more than it does: %+v", answer)
		}

		status, body = plane.call(t, http.MethodGet,
			connections+"/"+trigger.Connection.ID+"/deliveries", nil)
		if status != http.StatusOK {
			t.Fatalf("reading deliveries = %d: %s", status, body)
		}
		var history deliveryListBody
		decodeInto(t, body, &history)

		tests, rejections := 0, 0
		for _, delivery := range history.Items {
			if delivery.Test {
				tests++
			}
			if delivery.Outcome == "rejected" && delivery.Reason == "" {
				t.Error("a rejection with no reason")
			}
			if delivery.Outcome == "rejected" {
				rejections++
			}
		}
		if tests == 0 {
			t.Error("the test event is not marked as a test in the history; it would be " +
				"indistinguishable from a delivery the customer's system sent")
		}
		if rejections == 0 {
			t.Error("the refused delivery is not in the history (story 31)")
		}
	})

	t.Run("validating a disabled connection claims nothing about whether it works", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId": environment.ID,
			"integration":   "alertmanager",
			"name":          "Turned Off",
			"role":          "trigger",
			"locality":      "control_plane",
		})
		if status != http.StatusCreated {
			t.Fatalf("creating = %d: %s", status, body)
		}
		var off createdConnectionBody
		decodeInto(t, body, &off)

		if status, body = plane.call(t, http.MethodPost,
			connections+"/"+off.Connection.ID+"/enabled",
			map[string]any{"enabled": false}); status != http.StatusNoContent {
			t.Fatalf("disabling = %d: %s", status, body)
		}

		status, body = plane.call(t, http.MethodPost,
			connections+"/"+off.Connection.ID+"/validate", nil)
		if status != http.StatusOK {
			t.Fatalf("validating a disabled connection = %d: %s", status, body)
		}
		var result validationBody
		decodeInto(t, body, &result)
		if result.Authentication != "not_attempted" {
			t.Errorf("authentication = %q for a disabled connection; being turned off is "+
				"orthogonal to working, which is why disabled_at is its own column",
				result.Authentication)
		}
		if state := plane.detail(t, connections+"/"+off.Connection.ID).State; state == "failed" {
			t.Error("a disabled connection reads as failed after a check that ran nothing; that " +
				"tells an operator a check found something wrong when no check ran")
		}
	})

	t.Run("the delivery history is filtered by the backend, not after paging", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet,
			connections+"/"+trigger.Connection.ID+"/deliveries?outcome=rejected", nil)
		if status != http.StatusOK {
			t.Fatalf("filtering deliveries = %d: %s", status, body)
		}
		var filtered deliveryListBody
		decodeInto(t, body, &filtered)
		if len(filtered.Items) == 0 {
			t.Fatal("filtering to rejections found none, and one was refused earlier")
		}
		for _, delivery := range filtered.Items {
			if delivery.Outcome != "rejected" {
				t.Errorf("outcome=rejected returned a %q delivery", delivery.Outcome)
			}
		}

		// An outcome nobody serves is refused rather than narrowing to nothing: an empty page is
		// what "there were none" looks like, so a misspelling would read as a clean Connection.
		if status, _ = plane.call(t, http.MethodGet,
			connections+"/"+trigger.Connection.ID+"/deliveries?outcome=rejcted",
			nil); status != http.StatusBadRequest {
			t.Errorf("a misspelled outcome = %d, want 400", status)
		}
	})

	t.Run("deleting a connection with a history is refused and says what depends on it", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet,
			connections+"/"+trigger.Connection.ID+"/dependents", nil)
		if status != http.StatusOK {
			t.Fatalf("reading dependents = %d: %s", status, body)
		}
		var reported dependentsBody
		decodeInto(t, body, &reported)
		if !reported.Dependents.Any {
			t.Fatalf("nothing depends on a connection that has delivered: %+v", reported.Dependents)
		}

		status, body = plane.call(t, http.MethodDelete, connections+"/"+trigger.Connection.ID, nil)
		if status != http.StatusConflict {
			t.Fatalf("deleting a connection with a history = %d, want 409: %s", status, body)
		}
		var refusal dependentsBody
		decodeInto(t, body, &refusal)
		if refusal.Error == "" {
			t.Error("the refusal says nothing an operator could act on")
		}
		if refusal.Dependents.Signals == 0 && refusal.Dependents.Deliveries == 0 {
			t.Errorf("the refusal names no dependent: %+v", refusal.Dependents)
		}
		if !strings.Contains(strings.ToLower(refusal.Error), "disable") {
			t.Errorf("the refusal does not point at disabling, which is what loses nothing: %q",
				refusal.Error)
		}
	})

	t.Run("deleting a connection nothing depends on succeeds", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId": environment.ID,
			"integration":   "alertmanager",
			"name":          "Created By Mistake",
			"role":          "trigger",
			"locality":      "control_plane",
		})
		if status != http.StatusCreated {
			t.Fatalf("creating = %d: %s", status, body)
		}
		var mistake createdConnectionBody
		decodeInto(t, body, &mistake)

		status, body = plane.call(t, http.MethodDelete, connections+"/"+mistake.Connection.ID, nil)
		if status != http.StatusNoContent {
			t.Fatalf("deleting an unused connection = %d, want 204: %s", status, body)
		}
		if status, _ := plane.call(t, http.MethodGet,
			connections+"/"+mistake.Connection.ID, nil); status != http.StatusNotFound {
			t.Errorf("reading a deleted connection = %d, want 404", status)
		}
	})

	// The credential test comes last so it runs against a Connection with as much history as
	// this suite produces.
	t.Run("no read path on the whole surface returns the credential", func(t *testing.T) {
		plane.assertSecretUnreadable(t, trigger.Secret, trigger.Connection.ID)
	})
}

// detail reads one Connection's detail route.
func (p *connectionPlane) detail(t *testing.T, url string) detailBody {
	t.Helper()

	status, body := p.call(t, http.MethodGet, url, nil)
	if status != http.StatusOK {
		t.Fatalf("reading %s = %d: %s", url, status, body)
	}
	var detail detailBody
	decodeInto(t, body, &detail)
	return detail
}

// assertSecretUnreadable walks EVERY GET route the operator surface declares and asserts none of
// them returns the secret.
//
// It iterates the route table rather than naming routes, which is the whole point: a test that
// listed the reads it knew about would keep passing when somebody added the one that leaked. The
// table is the API's index, so walking it covers the read side by construction — including
// routes added after this test was written.
func (p *connectionPlane) assertSecretUnreadable(t *testing.T, secret, connectionID string) {
	t.Helper()

	if secret == "" {
		t.Fatal("no secret to look for; this assertion would pass vacuously")
	}

	// Values for the path variables. Anything not named here gets an identifier that resolves to
	// nothing, which answers 404 — harmless, because what is under test is that no route ever
	// puts the secret in a body.
	substitutions := map[string]string{
		"{organization}": surfaceOrg,
		"{connection}":   connectionID,
		"{registration}": p.relay.registration.String(),
	}

	reached := 0
	for _, route := range (operator.Handlers{}).Routes() {
		if route.Method() != http.MethodGet || route.Access() != authz.AccessPrivileged {
			continue
		}
		path := route.Pattern()
		for variable, value := range substitutions {
			path = strings.ReplaceAll(path, variable, value)
		}
		// Whatever is left is a variable this test has no real value for. A fresh identifier is
		// the honest filler: it addresses nothing, so the route answers 404.
		for strings.Contains(path, "{") {
			opening := strings.Index(path, "{")
			closing := strings.Index(path[opening:], "}") + opening
			path = path[:opening] + uuid.NewString() + path[closing+1:]
		}

		status, body := p.call(t, http.MethodGet, "http://"+p.operator+path, nil)
		if strings.Contains(body, secret) {
			t.Errorf("GET %s returned the trigger secret; it is written once and no path may "+
				"read it back", route.Pattern())
		}
		if status == http.StatusOK {
			reached++
		}
	}
	if reached == 0 {
		t.Fatal("no read route answered 200; this assertion would be passing vacuously")
	}
}
