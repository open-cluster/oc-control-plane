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

	t.Run("the process keeps the digest and not the token", func(t *testing.T) {
		t.Parallel()
		const token = "a-token-long-enough-to-be-worth-something"

		cfg, err := config.Load(environment(t, map[string]string{
			config.EnvOperatorAddress:   address,
			config.EnvOperatorTokenFile: secretFile(t, "token", token),
		}))
		if err != nil {
			t.Fatalf("loading: %v", err)
		}
		digest := sha256.Sum256([]byte(token))
		if string(cfg.OperatorTokenDigest) != string(digest[:]) {
			t.Error("the configured digest does not match the token in the file")
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
	More   bool            `json:"more"`
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

func relayIn(roster rosterResponse, registration string) *relayResponse {
	for index := range roster.Relays {
		if roster.Relays[index].RegistrationID == registration {
			return &roster.Relays[index]
		}
	}
	return nil
}
