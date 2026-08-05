package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/intake"
)

// Environments and Connections are the boundary evidence may never cross, so what is asserted
// here is what an operator could observe: a scope exists the moment their organization does, a
// Connection created through the API can actually receive a delivery, the secret is shown once
// and never again, and a request combining one tenant's Environment with another's Connection
// is refused.
//
// The seam is the assembled process — real HTTP, a real database — because that is the seam
// every slice here has used and because a boundary proven against a double is a boundary
// proven against nothing.

const (
	surfaceToken   = "operator-token-for-the-connection-surface"
	surfaceOrg     = "org-a"
	neighbourOrg   = "org-neighbour"
	suppliedSecret = "a-supplied-secret-that-is-long-enough-to-pass"
)

// connectionPlane is a control plane with the operator surface and intake both listening, plus
// an enrolled relay for a Connection to bind to.
type connectionPlane struct {
	*controlPlane
	operator string
	intake   string
	relay    relayCredentials
	dsn      string
}

func startConnectionPlane(t *testing.T) *connectionPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	relayAddress := freeAddress(t)
	intakeAddress := freeAddress(t)
	var dsn string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		cfg.IntakeAddress = intakeAddress
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		digest := sha256.Sum256([]byte(surfaceToken))
		cfg.OperatorTokenDigest = digest[:]
		// The bootstrap credential is bound to ONE organization, which is the difference
		// between it and the ambient root token it replaces. A request naming the neighbour
		// below is now refused by the authorization middleware before it reaches a query — the
		// cross-tenant assertions in these tests assert that refusal rather than a scoped query.
		cfg.OperatorTokenOrganization = surfaceOrg
		// The neighbour shares this placement deliberately. An organization with no placement
		// fails before any query runs, which would leave the cross-tenant assertions passing
		// against an implementation with no scoping at all.
		cfg.Assignments[neighbourOrg] = "shared"
		dsn = cfg.Placements["shared"]
	})

	connection := dialRelay(t, relayAddress)
	return &connectionPlane{
		controlPlane: plane,
		operator:     operatorAddress,
		intake:       intakeAddress,
		relay:        registerRelay(t, connection, dsn, surfaceOrg),
		dsn:          dsn,
	}
}

func (p *connectionPlane) base(organization string) string {
	return "http://" + p.operator + "/operator/v1/organizations/" + organization
}

// call sends an authenticated operator request with an optional JSON body.
func (p *connectionPlane) call(t *testing.T, method, url string, body any) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+surfaceToken)
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response from %s: %v", url, err)
	}
	return response.StatusCode, string(answer)
}

// defaultEnvironment reads the organization's scopes, which is also what creates the Default
// the first time anyone looks.
func (p *connectionPlane) defaultEnvironment(t *testing.T, organization string) environmentBody {
	t.Helper()

	status, body := p.call(t, http.MethodGet, p.base(organization)+"/environments", nil)
	if status != http.StatusOK {
		t.Fatalf("listing environments = %d: %s", status, body)
	}
	var listed environmentListBody
	decodeInto(t, body, &listed)
	for _, environment := range listed.Environments {
		if environment.IsDefault {
			return environment
		}
	}
	t.Fatalf("no default environment in %s", body)
	return environmentBody{}
}

// neighbourEnvironment arranges the neighbour tenant's Default Environment through the STORE
// rather than the surface.
//
// It has to. The bootstrap credential is bound to one organization now, so the surface answers
// 404 for the neighbour — which is the property under test and cannot also be the setup for it.
// Reaching past the surface to arrange another tenant's state is exactly what makes the
// assertion that follows meaningful: the row genuinely exists, and the refusal is the boundary
// rather than an absence.
func (p *connectionPlane) neighbourEnvironment(t *testing.T, organization string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	placements := openPlacement(t, p.dsn)
	named := namedOrganization(t, organization)
	environment, err := placements.EnsureDefaultEnvironment(ctx, ownerOf(t, named), named)
	if err != nil {
		t.Fatalf("arranging the neighbour's default environment: %v", err)
	}
	return environment.ID.String()
}

// deliveries numbers each delivery so every body is distinct. Intake deduplicates on the body
// digest, so a helper that sent the same bytes twice would report the second as already
// accepted — which is correct behaviour and would make every assertion about the credential
// read as a test of the deduplication instead.
var deliveries atomic.Int64

// deliverThrough posts a distinct Alertmanager payload to a Connection and reports the status.
func (p *connectionPlane) deliverThrough(t *testing.T, connection, secret string) int {
	t.Helper()

	url := fmt.Sprintf("http://%s/intake/v1/connections/%s/signals", p.intake, connection)
	body := firing(fmt.Sprintf("fp-%s-%d", connection, deliveries.Add(1)),
		time.Now().UTC().Truncate(time.Second))
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("building the delivery: %v", err)
	}
	request.Header.Set(intake.TokenHeader, secret)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("delivering: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

func decodeInto(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
}

// These mirror what the surfaces send. They are spelled out rather than decoded into a map so
// that a renamed field breaks here, where the contract is asserted.
type environmentListBody struct {
	Environments []environmentBody `json:"environments"`
	Next         string            `json:"next"`
}

type environmentBody struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

type connectionBody struct {
	ID                  string            `json:"id"`
	EnvironmentID       string            `json:"environmentId"`
	Integration         string            `json:"integration"`
	Name                string            `json:"name"`
	Role                string            `json:"role"`
	Locality            string            `json:"locality"`
	RelayRegistrationID string            `json:"relayRegistrationId"`
	Labels              map[string]string `json:"labels"`
	DisabledAt          *time.Time        `json:"disabledAt"`
}

type createdConnectionBody struct {
	Connection connectionBody `json:"connection"`
	Secret     string         `json:"secret"`
}

type connectionListBody struct {
	Connections []connectionBody `json:"connections"`
}

func TestEnvironmentSurface(t *testing.T) {
	plane := startConnectionPlane(t)
	base := plane.base(surfaceOrg)

	// A new organization has exactly one Environment, named Default and marked as such. It is
	// created on first read because an Organization arrives from an identity provider rather
	// than being created here, so there is no organization-creating transaction to join.
	status, body := plane.call(t, http.MethodGet, base+"/environments", nil)
	if status != http.StatusOK {
		t.Fatalf("listing environments = %d: %s", status, body)
	}
	var initial environmentListBody
	decodeInto(t, body, &initial)
	if len(initial.Environments) != 1 {
		t.Fatalf("a new organization has %d environments, want exactly one",
			len(initial.Environments))
	}

	original := plane.defaultEnvironment(t, surfaceOrg)
	if original.Name != "Default" || !original.IsDefault {
		t.Fatalf("the one environment is %q (default=%v), want Default and marked as such",
			original.Name, original.IsDefault)
	}

	t.Run("the default can be renamed and its identity survives", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPatch,
			base+"/environments/"+original.ID, map[string]string{"name": "Production"})
		if status != http.StatusOK {
			t.Fatalf("renaming = %d: %s", status, body)
		}
		var renamed environmentBody
		decodeInto(t, body, &renamed)
		if renamed.ID != original.ID {
			t.Fatalf("the identity changed on rename: %s became %s", original.ID, renamed.ID)
		}
		if !renamed.IsDefault {
			t.Fatal("renaming the default must not stop it being the default")
		}
	})

	t.Run("the default cannot be deleted", func(t *testing.T) {
		status, body := plane.call(t, http.MethodDelete, base+"/environments/"+original.ID, nil)
		if status != http.StatusConflict {
			t.Fatalf("deleting the default = %d, want 409: %s", status, body)
		}
	})

	var second environmentBody
	t.Run("another environment can be created and listed", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			base+"/environments", map[string]string{"name": "Staging"})
		if status != http.StatusCreated {
			t.Fatalf("creating = %d: %s", status, body)
		}
		decodeInto(t, body, &second)
		if second.IsDefault {
			t.Fatal("only the environment created with the organization is the default")
		}

		status, body = plane.call(t, http.MethodGet, base+"/environments", nil)
		if status != http.StatusOK {
			t.Fatalf("listing = %d: %s", status, body)
		}
		var listed environmentListBody
		decodeInto(t, body, &listed)
		if len(listed.Environments) != 2 {
			t.Fatalf("listed %d environments, want 2: %s", len(listed.Environments), body)
		}
	})

	t.Run("a duplicate name is refused, and the neighbour is not reachable at all",
		func(t *testing.T) {
			status, _ := plane.call(t, http.MethodPost,
				base+"/environments", map[string]string{"name": "Staging"})
			if status != http.StatusConflict {
				t.Fatalf("a duplicate name in one organization = %d, want 409", status)
			}

			// Uniqueness is per tenant, and this credential can no longer demonstrate that from
			// the outside: it is bound to ONE organization, so naming the neighbour answers 404
			// exactly as naming an organization nobody has does. That the same name IS allowed
			// in another tenant is asserted where a test can act as both — see
			// TestBoundary_OneRelayServesConnectionsInTwoEnvironments and its neighbours in
			// internal/storage.
			status, body := plane.call(t, http.MethodPost,
				plane.base(neighbourOrg)+"/environments", map[string]string{"name": "Staging"})
			if status != http.StatusNotFound {
				t.Fatalf("the neighbour answered %d, want 404 — a credential bound to one "+
					"organization must not reach another: %s", status, body)
			}
		})

	t.Run("an environment holding connections cannot be deleted, and can once emptied",
		func(t *testing.T) {
			status, body := plane.call(t, http.MethodPost, base+"/connections", map[string]any{
				"environmentId": second.ID,
				"integration":   "kubernetes",
				"name":          "the staging cluster",
				"role":          "evidence",
				"locality":      "relay",
				// A relay registration in the same organization, which is the only kind the
				// composite foreign key admits.
				"relayRegistrationId": plane.relay.registration.String(),
			})
			if status != http.StatusCreated {
				t.Fatalf("creating a connection = %d: %s", status, body)
			}
			var created createdConnectionBody
			decodeInto(t, body, &created)

			status, body = plane.call(t, http.MethodDelete, base+"/environments/"+second.ID, nil)
			if status != http.StatusConflict {
				t.Fatalf("deleting an environment that still groups connections = %d, want 409: %s",
					status, body)
			}

			// Removing the Connection is not part of this surface yet — disabling is not
			// deleting — so the row is removed directly to prove the refusal was about the
			// Connection existing rather than about the environment.
			removeConnection(t, plane.dsn, created.Connection.ID)

			status, body = plane.call(t, http.MethodDelete, base+"/environments/"+second.ID, nil)
			if status != http.StatusNoContent {
				t.Fatalf("deleting an emptied environment = %d, want 204: %s", status, body)
			}
		})
}

func TestConnectionSurface(t *testing.T) {
	plane := startConnectionPlane(t)
	base := plane.base(surfaceOrg)
	environment := plane.defaultEnvironment(t, surfaceOrg)
	// Organization-wide, with the Environment as a filter rather than a path segment.
	connections := base + "/connections"

	t.Run("the integrations this build has are something the product states", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet,
			base+"/integrations", nil)
		if status != http.StatusOK {
			t.Fatalf("listing integrations = %d: %s", status, body)
		}
		for _, want := range []string{"alertmanager", "kubernetes", "trigger", "evidence"} {
			if !containsString(body, want) {
				t.Errorf("the integrations listing does not mention %q: %s", want, body)
			}
		}
	})

	var trigger createdConnectionBody
	t.Run("a trigger connection is created and its secret is shown once", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId": environment.ID,
			"integration":   "alertmanager",
			"name":          "Production Alertmanager",
			"role":          "trigger",
			"locality":      "control_plane",
			"labels":        map[string]string{"team": "platform"},
		})
		if status != http.StatusCreated {
			t.Fatalf("creating = %d: %s", status, body)
		}
		decodeInto(t, body, &trigger)
		if trigger.Secret == "" {
			t.Fatal("a trigger connection must be given a secret to be configured with")
		}
		if trigger.Connection.Labels["team"] != "platform" {
			t.Errorf("labels must survive: %+v", trigger.Connection.Labels)
		}

		// Shown once. No later read exposes it, which is the whole property digest-only
		// storage exists for.
		status, listing := plane.call(t, http.MethodGet, connections, nil)
		if status != http.StatusOK {
			t.Fatalf("listing = %d: %s", status, listing)
		}
		if containsString(listing, trigger.Secret) {
			t.Fatal("a later read exposed the secret")
		}
	})

	t.Run("a connection created through the API accepts a delivery signed with its secret",
		func(t *testing.T) {
			if status := plane.deliverThrough(t, trigger.Connection.ID, trigger.Secret); status !=
				http.StatusAccepted {
				t.Fatalf("a delivery with the issued secret = %d, want 202", status)
			}
			if status := plane.deliverThrough(t, trigger.Connection.ID,
				"a-secret-that-was-never-issued-anywhere"); status != http.StatusUnauthorized {
				t.Fatalf("a delivery with a wrong secret = %d, want 401", status)
			}
		})

	// The same bytes delivered twice are recognised and applied to nothing. That covers both a
	// source retrying because it never saw a response — which has done nothing wrong, and whose
	// answer must let it stop — and a body replayed by someone who captured it.
	t.Run("the same body twice is accepted once and applied once", func(t *testing.T) {
		replayed := firing("fp-replayed", time.Now().UTC().Truncate(time.Second))
		url := fmt.Sprintf("http://%s/intake/v1/connections/%s/signals",
			plane.intake, trigger.Connection.ID)

		if status := postBody(t, url, trigger.Secret, replayed); status != http.StatusAccepted {
			t.Fatalf("the first delivery = %d, want 202", status)
		}
		if status := postBody(t, url, trigger.Secret, replayed); status != http.StatusOK {
			t.Fatalf("the replay = %d, want 200 (already accepted)", status)
		}
		if recorded := countSignals(t, plane.dsn, trigger.Connection.ID, "fp-replayed"); recorded != 1 {
			t.Fatalf("a replayed body produced %d signals, want 1", recorded)
		}
	})

	t.Run("a rotated secret accepts the new value and refuses the old", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			base+"/connections/"+trigger.Connection.ID+"/trigger/rotate-secret", nil)
		if status != http.StatusOK {
			t.Fatalf("rotating = %d: %s", status, body)
		}
		var rotated struct {
			Secret string `json:"secret"`
		}
		decodeInto(t, body, &rotated)
		if rotated.Secret == "" || rotated.Secret == trigger.Secret {
			t.Fatal("a rotation must issue a new secret")
		}

		if status := plane.deliverThrough(t, trigger.Connection.ID, rotated.Secret); status !=
			http.StatusAccepted {
			t.Errorf("a delivery with the rotated secret = %d, want 202", status)
		}
		// No overlap window in this slice: the previous secret stops working the moment the
		// rotation commits, which is a brief outage the operator scheduled.
		if status := plane.deliverThrough(t, trigger.Connection.ID, trigger.Secret); status !=
			http.StatusUnauthorized {
			t.Errorf("a delivery with the superseded secret = %d, want 401", status)
		}
	})

	t.Run("a weak secret is refused at creation", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId": environment.ID,
			"integration":   "alertmanager",
			"name":          "A Weakly Configured Alertmanager",
			"role":          "trigger",
			"locality":      "control_plane",
			"secret":        "short",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("a weak secret = %d, want 400: %s", status, body)
		}
	})

	t.Run("a supplied secret that clears the floor is taken as written", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, connections, map[string]any{
			"environmentId": environment.ID,
			"integration":   "alertmanager",
			"name":          "A Self-Configured Alertmanager",
			"role":          "trigger",
			"locality":      "control_plane",
			"secret":        suppliedSecret,
		})
		if status != http.StatusCreated {
			t.Fatalf("a strong supplied secret = %d, want 201: %s", status, body)
		}
		var created createdConnectionBody
		decodeInto(t, body, &created)
		if status := plane.deliverThrough(t, created.Connection.ID, suppliedSecret); status !=
			http.StatusAccepted {
			t.Errorf("a delivery with the supplied secret = %d, want 202", status)
		}
	})

	t.Run("a disabled connection refuses deliveries and is still listed", func(t *testing.T) {
		enabled := base + "/connections/" + trigger.Connection.ID + "/enabled"

		status, body := plane.call(t, http.MethodPost, enabled, map[string]any{"enabled": false})
		if status != http.StatusNoContent {
			t.Fatalf("disabling = %d: %s", status, body)
		}

		// Idempotent, which is the whole reason the pair became one operation: setting the state
		// it already holds is not an error, so a retry during an incident is safe.
		if again, _ := plane.call(t, http.MethodPost, enabled,
			map[string]any{"enabled": false}); again != http.StatusNoContent {
			t.Errorf("setting the state it already holds = %d, want 204", again)
		}
		// And a body that names no state is refused rather than guessed at.
		if empty, _ := plane.call(t, http.MethodPost, enabled,
			map[string]any{}); empty != http.StatusBadRequest {
			t.Errorf("a body naming no state = %d, want 400", empty)
		}

		status, listing := plane.call(t, http.MethodGet, connections, nil)
		if status != http.StatusOK {
			t.Fatalf("listing = %d: %s", status, listing)
		}
		var listed connectionListBody
		decodeInto(t, listing, &listed)
		var found bool
		for _, one := range listed.Connections {
			if one.ID == trigger.Connection.ID {
				found = true
				if one.DisabledAt == nil {
					t.Error("a disabled connection must say so")
				}
			}
		}
		if !found {
			t.Fatal("disabling is not deleting; the record of what it produced must survive")
		}

		// Re-enabled so the rest of the suite is not affected by the order it ran in.
		if status, body = plane.call(t, http.MethodPost, enabled,
			map[string]any{"enabled": true}); status != http.StatusNoContent {
			t.Fatalf("enabling = %d: %s", status, body)
		}
	})

	t.Run("combinations that could never work are refused", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"relay-local naming no relay": {
				"integration": "kubernetes", "name": "no relay named",
				"role": "evidence", "locality": "relay",
			},
			"central naming a relay": {
				"integration": "kubernetes", "name": "a relay it does not need",
				"role": "evidence", "locality": "control_plane",
				"relayRegistrationId": plane.relay.registration.String(),
			},
			"an integration this build does not have": {
				"integration": "zabbix", "name": "not compiled",
				"role": "evidence", "locality": "control_plane",
			},
			"a role the integration does not serve": {
				"integration": "alertmanager", "name": "alertmanager cannot be read from",
				"role": "evidence", "locality": "control_plane",
			},
			"a role that is not one of the three": {
				"integration": "kubernetes", "name": "a role nobody has",
				"role": "sometimes", "locality": "relay",
				"relayRegistrationId": plane.relay.registration.String(),
			},
		} {
			// Each body names a valid Environment, so what is refused is the combination under
			// test rather than a field somebody forgot — a 400 for the wrong reason would pass
			// this table and prove nothing.
			body["environmentId"] = environment.ID
			if status, answer := plane.call(t, http.MethodPost, connections, body); status !=
				http.StatusBadRequest {
				t.Errorf("%s = %d, want 400: %s", name, status, answer)
			}
		}
	})

	// The sharpest tenancy assertion available on this surface, and it is now sharper than it
	// was. Both organizations sit on the same placement deliberately, so the scoping is the only
	// thing standing in the way — and there are now TWO things standing in the way rather than
	// one: the membership check refuses the neighbour before any query runs, and the composite
	// foreign key refuses a crossed Environment even for a caller who is a member.
	t.Run("one organization's environment cannot hold another's connection", func(t *testing.T) {
		neighbour := plane.neighbourEnvironment(t, neighbourOrg)

		// A member of surfaceOrg naming the NEIGHBOUR'S Environment. The membership check passes
		// — this is their own tenant in the path — so what refuses it is the tenant boundary in
		// the database, which is the half that would still matter if the middleware were wrong.
		status, body := plane.call(t, http.MethodPost,
			plane.base(surfaceOrg)+"/connections", map[string]any{
				"environmentId": neighbour,
				"integration":   "kubernetes", "name": "reaching across a tenant boundary",
				"role": "evidence", "locality": "relay",
				"relayRegistrationId": plane.relay.registration.String(),
			})
		if status != http.StatusNotFound {
			t.Fatalf("naming another organization's environment = %d, want 404: %s", status, body)
		}

		// And the same request addressed AS the neighbour, which this credential is not a member
		// of. It is refused one layer earlier and answers identically, which is the property
		// story 24 asks for: a caller cannot tell the two refusals apart.
		status, foreign := plane.call(t, http.MethodPost,
			plane.base(neighbourOrg)+"/connections", map[string]any{
				"environmentId": neighbour,
				"integration":   "kubernetes", "name": "borrowing another tenant's relay",
				"role": "evidence", "locality": "relay",
				"relayRegistrationId": plane.relay.registration.String(),
			})
		if status != http.StatusNotFound {
			t.Fatalf("naming another organization = %d, want 404: %s", status, foreign)
		}
		// The two bodies differ, and that is correct rather than a leak. Story 24 is about the
		// ORGANIZATION: a tenant the caller is not a member of must be indistinguishable from
		// one that does not exist, which is what the second answer is and what
		// TestOperatorIdentity_AForeignOrganizationLooksLikeOneThatDoesNotExist asserts. The
		// first answer goes to a member of THIS tenant who named a resource identifier they
		// already hold, and it collapses "that environment is not yours" and "that relay is not
		// yours" into one for the same reason.
		if !containsString(foreign, "organization not found") {
			t.Errorf("an unreachable organization says %q; it must say what an organization "+
				"that does not exist says", foreign)
		}
	})

	t.Run("one relay serves connections in two environments", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost,
			base+"/environments", map[string]string{"name": "Another Scope"})
		if status != http.StatusCreated {
			t.Fatalf("creating an environment = %d: %s", status, body)
		}
		var other environmentBody
		decodeInto(t, body, &other)

		for _, target := range []string{environment.ID, other.ID} {
			status, answer := plane.call(t, http.MethodPost, base+"/connections", map[string]any{
				"environmentId": target,
				"integration":   "kubernetes", "name": "cluster in " + target,
				"role": "evidence", "locality": "relay",
				"relayRegistrationId": plane.relay.registration.String(),
			})
			if status != http.StatusCreated {
				t.Fatalf("one relay must serve connections in several environments; %s = %d: %s",
					target, status, answer)
			}
		}
	})
}

func containsString(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// removeConnection deletes a Connection row directly. The operator surface deliberately has no
// delete — disabling is not deleting, because the record of what a source produced must
// survive — so proving that an Environment can be emptied needs this.
func removeConnection(t *testing.T, dsn, id string) {
	t.Helper()
	ctx := context.Background()

	database, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	if _, err = database.Exec(ctx,
		`DELETE FROM connection WHERE connection_id = $1`, id); err != nil {
		t.Fatalf("removing the connection: %v", err)
	}
}

// postBody delivers exact bytes, for the assertions about what a repeated body does.
func postBody(t *testing.T, url, secret, body string) int {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("building the delivery: %v", err)
	}
	request.Header.Set(intake.TokenHeader, secret)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("delivering: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

// countSignals reports how many Signals one Connection recorded for a source key.
func countSignals(t *testing.T, dsn, connection, sourceKey string) int {
	t.Helper()
	ctx := context.Background()

	database, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	var count int
	if err = database.QueryRow(ctx,
		`SELECT count(*) FROM signal WHERE connection_id = $1 AND source_key = $2`,
		connection, sourceKey).Scan(&count); err != nil {
		t.Fatalf("counting signals: %v", err)
	}
	return count
}
