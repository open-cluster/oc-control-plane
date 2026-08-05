package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
)

// The control plane can tell that a relay identity is being taken over by two parties, and
// until there was something to read it that finding sat in a column. What is asserted here is
// that an operator can now see it and act on it, and that nobody else can.
//
// The token is the whole access control on a surface that reads across tenants, so the
// refusals matter as much as the reads: they are asserted to be indistinguishable from each
// other, and the token is asserted to appear in no log line.
//
// Both directions are asserted deliberately, and together they are what makes the check
// load-bearing without having to break it to find out. A comparison that always passed would
// serve the roster to a caller presenting nothing, and the first case would fail; one that
// always failed would refuse the right token, and the reads would fail. Neither can be true of
// a suite that is green.
func TestOperatorSurface(t *testing.T) {
	const organization = "org-a"

	// Long enough that the configuration accepts it, which is itself the point: a token short
	// enough to guess is the same as no token on a cross-tenant surface.
	const token = "operator-token-for-the-surface-under-test"

	operatorAddress := freeAddress(t)
	relayAddress := freeAddress(t)
	var placementDSN string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		cfg.OperatorAddress = operatorAddress
		digest := sha256.Sum256([]byte(token))
		cfg.OperatorTokenDigest = digest[:]
		// The credential names the one tenant it reaches. That binding is the whole difference
		// between it and the shared token it replaces, and the last case in this test asserts
		// that a second organization is not reachable with it.
		cfg.OperatorTokenOrganization = organization
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	connection := dialRelay(t, relayAddress)
	relay := registerRelay(t, connection, placementDSN, organization)
	placements := openPlacement(t, placementDSN)
	owner := namedOrganization(t, organization)

	base := "http://" + operatorAddress + "/operator/v1/organizations/" + organization

	var refusals []string
	t.Run("nothing is served without the token", func(t *testing.T) {
		status, body := operatorRequest(t, http.MethodGet, base+"/relays", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated read returned %d, want 401 — this surface reads "+
				"across every tenant the instance serves", status)
		}
		refusals = append(refusals, body)
	})

	t.Run("nor with the wrong one", func(t *testing.T) {
		status, body := operatorRequest(t, http.MethodGet, base+"/relays",
			"a-token-that-was-never-issued-and-is-long-enough")
		if status != http.StatusUnauthorized {
			t.Fatalf("a wrong token returned %d, want 401", status)
		}
		refusals = append(refusals, body)
	})

	t.Run("refusals are indistinguishable", func(t *testing.T) {
		if len(refusals) == 2 && refusals[0] != refusals[1] {
			t.Errorf("a missing token says %q and a wrong one says %q; the difference tells a "+
				"caller which half of a guess was right", refusals[0], refusals[1])
		}
	})

	t.Run("a relay appears with nothing wrong with it", func(t *testing.T) {
		roster := readRoster(t, base+"/relays", token)

		found := relayIn(roster, relay.registration.String())
		if found == nil {
			t.Fatalf("the registered relay %v is not in the roster", relay.registration)
		}
		if found.SessionConflict != nil {
			t.Errorf("a relay nothing has happened to reports a conflict at %v",
				found.SessionConflict.DetectedAt)
		}
	})

	// More than one relay, because a page size that quietly defaults to one is invisible to any
	// test with a single row in it — and it would mean an operator scanning for contested
	// identities sees the first and none of the rest.
	t.Run("asking for no particular size returns the list", func(t *testing.T) {
		registered := []string{relay.registration.String()}
		for range 3 {
			registered = append(registered,
				registerRelay(t, connection, placementDSN, organization).registration.String())
		}

		roster := readRoster(t, base+"/relays", token)
		if len(roster.Relays) != len(registered) {
			t.Fatalf("an unqualified read returned %d of %d relays; a default page that shows "+
				"one row hides every finding after the first",
				len(roster.Relays), len(registered))
		}
		if roster.Next != "" {
			t.Errorf("a complete list offered a next page at %q", roster.Next)
		}
		for _, registration := range registered {
			if relayIn(roster, registration) == nil {
				t.Errorf("relay %s is missing from the roster", registration)
			}
		}
	})

	t.Run("a page that leaves relays out says how to reach them", func(t *testing.T) {
		first := readRoster(t, base+"/relays?limit=2", token)
		if len(first.Relays) != 2 {
			t.Fatalf("asked for two relays, got %d", len(first.Relays))
		}
		if first.Next == "" {
			t.Fatal("a truncated page offered no way forward; an operator would take it for " +
				"the whole list")
		}

		second := readRoster(t, base+"/relays?limit=2&after="+first.Next, token)
		if len(second.Relays) == 0 {
			t.Fatal("the next page is empty")
		}
		for _, seen := range first.Relays {
			if relayIn(second, seen.RegistrationID) != nil {
				t.Errorf("relay %s appears on both pages", seen.RegistrationID)
			}
		}
	})

	t.Run("a resume point that came from nowhere is refused", func(t *testing.T) {
		status, _ := operatorRequest(t, http.MethodGet, base+"/relays?after=not-a-cursor", token)
		if status != http.StatusBadRequest {
			t.Errorf("an invented cursor returned %d, want 400 — starting over silently would "+
				"show the first page again and read as the last", status)
		}
	})

	t.Run("a contested identity is surfaced as one", func(t *testing.T) {
		// Recorded through the same storage function the session service uses, rather than by
		// writing the row: what is under test here is that the finding surfaces, not how it
		// comes to be — that is covered where the detection lives.
		if err := placements.RecordSessionConflict(
			context.Background(), owner, relay.registration, 2); err != nil {
			t.Fatalf("recording a session conflict: %v", err)
		}

		found := relayIn(readRoster(t, base+"/relays", token), relay.registration.String())
		if found == nil || found.SessionConflict == nil {
			t.Fatal("a contested relay identity does not surface; the one party that can see " +
				"this pattern would still be reporting it to nobody")
		}
		if found.SessionConflict.DistinctHosts != 2 {
			t.Errorf("reported %d hosts, want 2", found.SessionConflict.DistinctHosts)
		}
		if !found.SessionConflict.MultipleHosts {
			t.Error("two hosts holding one credential was not stated as such; an operator " +
				"scanning a list should not have to know which number is the signal")
		}
	})

	t.Run("an operator can withdraw the mark", func(t *testing.T) {
		status, _ := operatorRequest(t, http.MethodPost,
			base+"/relays/"+relay.registration.String()+"/clear-conflict", token)
		if status != http.StatusNoContent {
			t.Fatalf("clearing returned %d, want 204", status)
		}

		found := relayIn(readRoster(t, base+"/relays", token), relay.registration.String())
		if found == nil {
			t.Fatal("the relay disappeared from the roster")
		}
		if found.SessionConflict != nil {
			t.Error("the mark survived being withdrawn; nothing else clears it, so it would " +
				"stay on a relay that was dealt with")
		}
	})

	// Withdrawing destroys the current finding, which is what makes the trail the point rather
	// than a formality: without it the second occurrence reads exactly like the first, and
	// "this has happened before" is the thing an operator most needs to know.
	t.Run("the history survives the withdrawal that erased the finding", func(t *testing.T) {
		trail := readTrail(t, base+"/relays/"+relay.registration.String()+"/session-conflicts", token)
		if len(trail.Events) != 2 {
			t.Fatalf("the trail holds %d events, want the detection and the withdrawal", len(trail.Events))
		}

		// Newest first: the withdrawal, then what it withdrew.
		withdrawn, detected := trail.Events[0], trail.Events[1]
		if withdrawn.Kind != "withdrawn" || detected.Kind != "detected" {
			t.Fatalf("the trail reads %q then %q, want the withdrawal then the detection",
				withdrawn.Kind, detected.Kind)
		}
		if detected.DistinctHosts != 2 || !detected.MultipleHosts {
			t.Errorf("the detection kept %d hosts, want the 2 it was recorded with — the "+
				"finding is what the trail exists to preserve", detected.DistinctHosts)
		}
		if withdrawn.WithdrawnFrom == "" {
			t.Error("the withdrawal records nowhere it came from; a shared token already " +
				"means it cannot record who, and an act with neither is anonymous")
		}
		if withdrawn.DistinctHosts != 0 {
			t.Errorf("the withdrawal claims to have observed %d hosts; it observed nothing",
				withdrawn.DistinctHosts)
		}
	})

	t.Run("withdrawing what is not marked changes nothing and says nothing", func(t *testing.T) {
		before := readTrail(t, base+"/relays/"+relay.registration.String()+"/session-conflicts", token)

		status, _ := operatorRequest(t, http.MethodPost,
			base+"/relays/"+relay.registration.String()+"/clear-conflict", token)
		if status != http.StatusNoContent {
			t.Fatalf("withdrawing an unmarked relay returned %d, want 204 — the state asked "+
				"for already holds", status)
		}

		after := readTrail(t, base+"/relays/"+relay.registration.String()+"/session-conflicts", token)
		if len(after.Events) != len(before.Events) {
			t.Errorf("an act that changed nothing added %d entries to the history; a trail "+
				"padded with those is a trail nobody reads",
				len(after.Events)-len(before.Events))
		}
	})

	t.Run("a long history pages without losing or repeating an entry", func(t *testing.T) {
		trailURL := base + "/relays/" + relay.registration.String() + "/session-conflicts"

		// Enough entries that a page cannot hold them. Off-by-one is the whole failure mode of
		// a keyset cursor, and it shows up as an entry that is skipped rather than as an error.
		for range 3 {
			if err := placements.RecordSessionConflict(
				context.Background(), owner, relay.registration, 2); err != nil {
				t.Fatalf("recording a session conflict: %v", err)
			}
			status, _ := operatorRequest(t, http.MethodPost,
				base+"/relays/"+relay.registration.String()+"/clear-conflict", token)
			if status != http.StatusNoContent {
				t.Fatalf("withdrawing returned %d", status)
			}
		}

		whole := readTrail(t, trailURL, token)
		if len(whole.Events) < 6 {
			t.Fatalf("the history holds %d entries, want at least the six just made",
				len(whole.Events))
		}

		var paged []conflictEventResponse
		for url, pages := trailURL+"?limit=2", 0; ; pages++ {
			if pages > len(whole.Events) {
				t.Fatal("paging did not end; the cursor is not advancing")
			}
			page := readTrail(t, url, token)
			paged = append(paged, page.Events...)
			if page.Next == "" {
				break
			}
			url = trailURL + "?limit=2&after=" + page.Next
		}

		if len(paged) != len(whole.Events) {
			t.Fatalf("paging yielded %d entries and one read yielded %d; the difference is "+
				"entries an operator would never see", len(paged), len(whole.Events))
		}
		for index := range paged {
			if !paged[index].At.Equal(whole.Events[index].At) ||
				paged[index].Kind != whole.Events[index].Kind {
				t.Fatalf("entry %d differs between paged and whole reads: %v %q against %v %q",
					index, paged[index].At, paged[index].Kind,
					whole.Events[index].At, whole.Events[index].Kind)
			}
		}
	})

	t.Run("a relay that does not exist is not found", func(t *testing.T) {
		status, _ := operatorRequest(t, http.MethodPost,
			base+"/relays/1ef7e1cf-0000-4000-8000-000000000000/clear-conflict", token)
		if status != http.StatusNotFound {
			t.Errorf("clearing an unknown relay returned %d, want 404", status)
		}
	})

	t.Run("an organization this deployment does not serve is not found", func(t *testing.T) {
		status, _ := operatorRequest(t, http.MethodGet,
			"http://"+operatorAddress+"/operator/v1/organizations/org-nowhere/relays", token)
		if status != http.StatusNotFound {
			t.Errorf("an unserved organization returned %d, want 404", status)
		}
	})

	t.Run("the token reaches no log line", func(t *testing.T) {
		if strings.Contains(plane.logs.String(), token) {
			t.Error("the operator token appears in the logs")
		}
	})
}

// The token file is the only way a token is configured in production, so the reading of it is
// worth exercising rather than assumed: a surface that silently starts with no credential, or
// with a guessable one, is worse than one that refuses to start.
func TestOperatorTokenComesFromAFile(t *testing.T) {
	t.Parallel()

	address := "127.0.0.1:9999"

	t.Run("a token file is required when the surface is enabled", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load(environment(t, map[string]string{
			config.EnvOperatorAddress: address,
		}))
		if err == nil {
			t.Fatal("the operator surface started with no token; it reads across every tenant")
		}
		if !strings.Contains(err.Error(), config.EnvOperatorTokenFile) {
			t.Errorf("the failure does not name %s: %v", config.EnvOperatorTokenFile, err)
		}
	})

	t.Run("a token too short to matter is refused", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load(environment(t, map[string]string{
			config.EnvOperatorAddress:   address,
			config.EnvOperatorTokenFile: secretFile(t, "token", "short"),
		}))
		if err == nil {
			t.Fatal("a guessable token was accepted")
		}
	})

	// The credential is bound to ONE organization, and that binding is required rather than
	// defaulted. A deployment whose scope was inferred would get the ambient root credential
	// this slice exists to replace, and the deployment that would get it is the one that did
	// not think about it.
	t.Run("a token that names no organization is refused", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load(environment(t, map[string]string{
			config.EnvOperatorAddress:   address,
			config.EnvOperatorTokenFile: secretFile(t, "token", "a-token-long-enough-to-be-worth-something"),
		}))
		if err == nil {
			t.Fatal("a credential with no organization was accepted; it would reach every tenant")
		}
		if !strings.Contains(err.Error(), config.EnvOperatorTokenOrganization) {
			t.Errorf("the failure does not name %s: %v",
				config.EnvOperatorTokenOrganization, err)
		}
	})

	t.Run("the process keeps the digest and not the token", func(t *testing.T) {
		t.Parallel()
		const token = "a-token-long-enough-to-be-worth-something"

		cfg, err := config.Load(environment(t, map[string]string{
			config.EnvOperatorAddress:           address,
			config.EnvOperatorTokenFile:         secretFile(t, "token", token),
			config.EnvOperatorTokenOrganization: "org-a",
		}))
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		digest := sha256.Sum256([]byte(token))
		if string(cfg.OperatorTokenDigest) != string(digest[:]) {
			t.Error("the configured digest does not match the token in the file")
		}
		if cfg.OperatorTokenOrganization != "org-a" {
			t.Errorf("the credential is bound to %q, want org-a", cfg.OperatorTokenOrganization)
		}
		// Defaulted rather than required. A deployment with no members yet needs a credential
		// that can create the first one; narrowing it is a one-line change once they have.
		if cfg.OperatorTokenRole != "organization_owner" {
			t.Errorf("the credential holds %q, want the owner by default", cfg.OperatorTokenRole)
		}
	})
}

// environment builds a lookup over a minimal valid configuration plus the overrides under
// test, so each case fails for the reason it is about rather than for a missing placement.
func environment(t *testing.T, overrides map[string]string) func(string) (string, bool) {
	t.Helper()
	dsn := secretFile(t, "dsn", "postgres://user:password@127.0.0.1:5432/controlplane")

	return func(key string) (string, bool) {
		if value, ok := overrides[key]; ok {
			return value, true
		}
		switch key {
		case config.EnvHTTPAddress:
			return "127.0.0.1:8080", true
		case config.EnvPlacements:
			return "shared=" + dsn, true
		case config.EnvDefaultPlacement:
			return "shared", true
		default:
			return "", false
		}
	}
}

func secretFile(t *testing.T, name, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// operatorRequest makes one call and returns its status and body.
func operatorRequest(t *testing.T, method, url, token string) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response from %s: %v", url, err)
	}
	return response.StatusCode, string(body)
}

// These mirror what the operator surface sends. They are spelled out rather than decoded into
// a map so that a renamed field breaks here, where the contract is asserted, instead of in
// whatever reads this months later and quietly stops seeing a finding.
type rosterResponse struct {
	Relays []relayResponse `json:"relays"`
	Next   string          `json:"next"`
}

type relayResponse struct {
	RegistrationID  string            `json:"registrationId"`
	RelayVersion    string            `json:"relayVersion"`
	SessionConflict *conflictResponse `json:"sessionConflict"`
}

type conflictResponse struct {
	DetectedAt    time.Time `json:"detectedAt"`
	DistinctHosts int       `json:"distinctHosts"`
	MultipleHosts bool      `json:"multipleHosts"`
}

func readRoster(t *testing.T, url, token string) rosterResponse {
	t.Helper()

	status, body := operatorRequest(t, http.MethodGet, url, token)
	if status != http.StatusOK {
		t.Fatalf("reading the roster returned %d: %s", status, body)
	}
	var roster rosterResponse
	if err := json.Unmarshal([]byte(body), &roster); err != nil {
		t.Fatalf("decoding the roster: %v (%s)", err, body)
	}
	return roster
}

type trailResponse struct {
	Events []conflictEventResponse `json:"events"`
	Next   string                  `json:"next"`
}

type conflictEventResponse struct {
	Kind          string    `json:"kind"`
	At            time.Time `json:"at"`
	DistinctHosts int       `json:"distinctHosts"`
	MultipleHosts bool      `json:"multipleHosts"`
	WithdrawnFrom string    `json:"withdrawnFrom"`
}

func readTrail(t *testing.T, url, token string) trailResponse {
	t.Helper()

	status, body := operatorRequest(t, http.MethodGet, url, token)
	if status != http.StatusOK {
		t.Fatalf("reading the trail returned %d: %s", status, body)
	}
	var trail trailResponse
	if err := json.Unmarshal([]byte(body), &trail); err != nil {
		t.Fatalf("decoding the trail: %v (%s)", err, body)
	}
	return trail
}

func relayIn(roster rosterResponse, registration string) *relayResponse {
	for index := range roster.Relays {
		if roster.Relays[index].RegistrationID == registration {
			return &roster.Relays[index]
		}
	}
	return nil
}
