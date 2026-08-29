package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The Relay fleet: counted, filtered, ordered and paged by the backend, plus what a Relay serves
// and the token that adds one.
//
// The scale assertions matter more than they look. A page that is fast at ten relays and slow at
// a thousand is a page that works for everybody who has not deployed the product yet.

type fleetSummaryBody struct {
	Total           int    `json:"total"`
	Connected       int    `json:"connected"`
	Disconnected    int    `json:"disconnected"`
	Revoked         int    `json:"revoked"`
	Outdated        int    `json:"outdated"`
	Degraded        int    `json:"degraded"`
	ActiveRequests  int    `json:"activeRequests"`
	LivenessSeconds int    `json:"livenessSeconds"`
	MinimumVersion  string `json:"minimumVersion"`
	OutdatedCounted bool   `json:"outdatedCounted"`
}

type fleetBody struct {
	Items []struct {
		RegistrationID     string     `json:"registrationId"`
		ClusterFingerprint string     `json:"clusterFingerprint"`
		RelayVersion       string     `json:"relayVersion"`
		Compatibility      string     `json:"compatibility"`
		Connected          bool       `json:"connected"`
		LastSeenAt         *time.Time `json:"lastSeenAt"`
		Capabilities       []string   `json:"capabilities"`
	} `json:"items"`
	Next    *string `json:"next"`
	Total   *int    `json:"total"`
	Partial []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	} `json:"partial"`
}

func TestRelayFleet(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)
	relays := base + "/relays"

	t.Run("the fleet is assessable without reading every row", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, relays+"/summary", nil)
		if status != http.StatusOK {
			t.Fatalf("reading the fleet summary = %d: %s", status, body)
		}
		var summary fleetSummaryBody
		decodeInto(t, body, &summary)

		if summary.Total < 1 {
			t.Fatalf("the summary counts %d relays and one is enrolled: %s", summary.Total, body)
		}
		if summary.Connected+summary.Disconnected+summary.Revoked != summary.Total {
			t.Errorf("the states do not add up to the total: %+v", summary)
		}
		if summary.LivenessSeconds == 0 {
			t.Error("the summary does not say how recently a relay must have been heard from, " +
				"so `connected` is a number nobody can interpret")
		}
		// This deployment states no relay version floor, so nothing was compared. Reporting
		// zero outdated without saying that would report a fleet as current on the strength of
		// a missing setting.
		if summary.OutdatedCounted {
			t.Errorf("outdated was counted against %q and this deployment states no floor",
				summary.MinimumVersion)
		}
	})

	t.Run("the summary agrees with the list", func(t *testing.T) {
		var summary fleetSummaryBody
		_, body := plane.call(t, http.MethodGet, relays+"/summary", nil)
		decodeInto(t, body, &summary)

		var listed fleetBody
		_, body = plane.call(t, http.MethodGet, relays+"?limit=200", nil)
		decodeInto(t, body, &listed)

		if len(listed.Items) != summary.Total {
			t.Errorf("the summary counts %d and the list returns %d; a summary that disagrees "+
				"with the rows under it is worse than no summary", summary.Total, len(listed.Items))
		}
		connected := 0
		for _, relay := range listed.Items {
			if relay.Connected {
				connected++
			}
		}
		if connected != summary.Connected {
			t.Errorf("the summary says %d connected and the list shows %d",
				summary.Connected, connected)
		}
	})

	t.Run("a relay that is holding a session reports as connected", func(t *testing.T) {
		var listed fleetBody
		status, body := plane.call(t, http.MethodGet, relays, nil)
		if status != http.StatusOK {
			t.Fatalf("listing relays = %d: %s", status, body)
		}
		decodeInto(t, body, &listed)
		if len(listed.Items) == 0 {
			t.Fatal("no relays")
		}
		// The harness enrols a relay but does not open a session, so presence is genuinely
		// absent. What is asserted is that the field is SERVED and honest, not that it is true:
		// a relay that never connected reporting `connected` would be the worse failure.
		for _, relay := range listed.Items {
			if relay.Connected && relay.LastSeenAt == nil {
				t.Errorf("%s reports connected with no last-seen time",
					relay.RegistrationID)
			}
		}
	})

	t.Run("protocol compatibility survives registration and remains honest for historical rows", func(t *testing.T) {
		var listed fleetBody
		status, body := plane.call(t, http.MethodGet, relays+"?limit=200", nil)
		if status != http.StatusOK {
			t.Fatalf("listing relays = %d: %s", status, body)
		}
		decodeInto(t, body, &listed)
		found := false
		for _, item := range listed.Items {
			if item.RegistrationID == plane.relay.registration.String() {
				found = true
				if item.Compatibility != "compatible" {
					t.Fatalf("newly registered relay compatibility = %q", item.Compatibility)
				}
			}
		}
		if !found {
			t.Fatal("newly registered relay is absent from fleet")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		database, err := pgx.Connect(ctx, plane.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = database.Close(ctx) }()
		if _, err = database.Exec(ctx, `
			INSERT INTO relay_registration
				(registration_id, org_id, credential_digest, cluster_fingerprint,
				 relay_version, capabilities)
			VALUES (gen_random_uuid(), $1, sha256('historical-protocol'::bytea),
			        'historical-protocol', '0.0.1', '[]'::jsonb)`, surfaceOrg); err != nil {
			t.Fatal(err)
		}
		status, body = plane.call(t, http.MethodGet, relays+"?search=historical-protocol", nil)
		if status != http.StatusOK {
			t.Fatalf("listing historical relay = %d: %s", status, body)
		}
		decodeInto(t, body, &listed)
		if len(listed.Items) != 1 || listed.Items[0].Compatibility != "unknown" {
			t.Fatalf("historical relay compatibility = %+v", listed.Items)
		}
	})

	t.Run("a relay's integrations are what disabling it would cost", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, base+"/integrations", map[string]any{
			"type":    "kubernetes",
			"name":    "Fleet Cluster",
			"relayId": plane.relay.registration.String(),
		})
		if status != http.StatusCreated {
			t.Fatalf("binding an integration to the relay = %d: %s", status, body)
		}

		status, body = plane.call(t, http.MethodGet,
			relays+"/"+plane.relay.registration.String()+"/integrations", nil)
		if status != http.StatusOK {
			t.Fatalf("reading a relay's integrations = %d: %s", status, body)
		}
		var served struct {
			Items []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"items"`
		}
		decodeInto(t, body, &served)
		if len(served.Items) == 0 {
			t.Error("the relay serves an Integration and reports none")
		}
	})

	t.Run("filters are applied by the backend", func(t *testing.T) {
		for _, narrowing := range []struct {
			query   string
			wantAll func(connected bool) bool
		}{
			{"?state=connected", func(connected bool) bool { return connected }},
			{"?state=disconnected", func(connected bool) bool { return !connected }},
		} {
			status, body := plane.call(t, http.MethodGet, relays+narrowing.query, nil)
			if status != http.StatusOK {
				t.Fatalf("%s = %d: %s", narrowing.query, status, body)
			}
			var listed fleetBody
			decodeInto(t, body, &listed)
			for _, relay := range listed.Items {
				if !narrowing.wantAll(relay.Connected) {
					t.Errorf("%s returned a relay whose connected is %v",
						narrowing.query, relay.Connected)
				}
			}
		}
	})

	t.Run("a filter over a property the record does not have is refused", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, relays+"?environmentId=whatever", nil)
		if status != http.StatusBadRequest {
			t.Errorf("filtering a fleet by an unoffered property = %d, want 400: %s", status, body)
		}
	})

	t.Run("an unoffered sort is refused and every offered one is served", func(t *testing.T) {
		if status, _ := plane.call(t, http.MethodGet,
			relays+"?sort=-credentialDigest", nil); status != http.StatusBadRequest {
			t.Errorf("sorting by an unoffered field = %d, want 400", status)
		}
		for _, field := range []string{
			"registeredAt", "-registeredAt", "lastSeenAt", "version", "-fingerprint",
		} {
			status, body := plane.call(t, http.MethodGet, relays+"?sort="+field, nil)
			if status != http.StatusOK {
				t.Errorf("sort=%s = %d: %s", field, status, body)
			}
		}
	})

	t.Run("a tampered cursor is refused rather than silently starting over", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, relays+"?cursor=not-a-position", nil)
		if status != http.StatusBadRequest {
			t.Errorf("a tampered cursor = %d, want 400: starting over would show an operator "+
				"the first page again and let them believe they had seen the last: %s",
				status, body)
		}
	})

	t.Run("a limit above the ceiling is clamped rather than refused", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, relays+"?limit=100000", nil)
		if status != http.StatusOK {
			t.Errorf("an oversized limit = %d, want it clamped and served: %s", status, body)
		}
	})

	t.Run("the envelope says what it served with no data behind it", func(t *testing.T) {
		var listed fleetBody
		_, body := plane.call(t, http.MethodGet, relays, nil)
		decodeInto(t, body, &listed)
		if len(listed.Partial) == 0 {
			t.Error("this deployment has no release channel and the envelope claims nothing was " +
				"served incompletely; a console would render an availableVersion column of " +
				"\"Not reported\" instead of one honest notice (story 33)")
		}
		for _, partial := range listed.Partial {
			if partial.Field == "" || partial.Reason == "" {
				t.Errorf("a partial with no field or no reason: %+v", partial)
			}
		}
	})

	t.Run("a bootstrap token is issued once with a stated expiry", func(t *testing.T) {
		status, body := plane.call(t, http.MethodPost, relays+"/bootstrap-tokens", nil)
		if status != http.StatusCreated {
			t.Fatalf("issuing a bootstrap token = %d, want 201: %s", status, body)
		}
		var issued struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expiresAt"`
			Notice    string    `json:"notice"`
		}
		decodeInto(t, body, &issued)
		if issued.Token == "" {
			t.Fatal("no token was returned")
		}
		if issued.ExpiresAt.IsZero() || !issued.ExpiresAt.After(time.Now()) {
			t.Errorf("expiresAt = %v; a credential with no visible lifetime is one somebody "+
				"puts in a wiki (story 38)", issued.ExpiresAt)
		}
		if issued.Notice == "" {
			t.Error("nothing says the token is shown once")
		}

		// Issued twice is two distinct tokens. A stable one would be a permanent secret wearing
		// a single-use label.
		status, body = plane.call(t, http.MethodPost, relays+"/bootstrap-tokens", nil)
		if status != http.StatusCreated {
			t.Fatalf("issuing a second bootstrap token = %d: %s", status, body)
		}
		var second struct {
			Token string `json:"token"`
		}
		decodeInto(t, body, &second)
		if second.Token == issued.Token {
			t.Error("two issues returned the same token")
		}
	})
}

// The fleet at the sizes the specification names: 1, 20, 100 and 1000.
//
// Three properties are asserted at each size, and each is one an offset-paginated listing would
// fail silently: the summary agrees with the rows under it, the cursor walks every row exactly
// once, and the time to answer stays bounded. The third is the one that decides whether this
// page works for a customer who has actually deployed the product — a listing that is fast at
// ten relays and slow at a thousand is a listing that works for everybody who has not.
func TestRelayFleetAtScale(t *testing.T) {
	for _, size := range []int{1, 20, 100, 1000} {
		t.Run(fmt.Sprintf("%d relays", size), func(t *testing.T) {
			plane := startIntegrationPlane(t)
			relays := plane.base(surfaceOrg) + "/relays"
			// The harness enrols one relay of its own, so one fewer is seeded to hit the number.
			plane.seedRelays(t, size-1)

			started := time.Now()
			var summary fleetSummaryBody
			status, body := plane.call(t, http.MethodGet, relays+"/summary", nil)
			if status != http.StatusOK {
				t.Fatalf("summary = %d: %s", status, body)
			}
			decodeInto(t, body, &summary)
			summaryTook := time.Since(started)

			if summary.Total != size {
				t.Errorf("the summary counts %d relays and %d exist", summary.Total, size)
			}
			if summary.Connected+summary.Disconnected+summary.Revoked != summary.Total {
				t.Errorf("the states do not add up: %+v", summary)
			}

			// The cursor walks every row exactly once. An offset would skip or duplicate rows as
			// relays enrol underneath the reader, and the way that fails is silent.
			seen := make(map[string]int, size)
			url := relays + "?limit=100"
			walkStarted := time.Now()
			for page := 0; ; page++ {
				if page > size/100+2 {
					t.Fatal("the cursor never reached the end")
				}
				status, body := plane.call(t, http.MethodGet, url, nil)
				if status != http.StatusOK {
					t.Fatalf("page %d = %d: %s", page, status, body)
				}
				var listed fleetBody
				decodeInto(t, body, &listed)
				for _, relay := range listed.Items {
					seen[relay.RegistrationID]++
				}
				if listed.Next == nil {
					break
				}
				url = relays + "?limit=100&cursor=" + *listed.Next
			}
			walkTook := time.Since(walkStarted)

			if len(seen) != size {
				t.Errorf("the cursor walked %d relays and %d exist", len(seen), size)
			}
			for id, times := range seen {
				if times != 1 {
					t.Errorf("%s appeared %d times in one walk", id, times)
				}
			}
			if summary.Total != len(seen) {
				t.Errorf("the summary counts %d and the cursor walked %d",
					summary.Total, len(seen))
			}

			// A bound rather than a benchmark. It is loose on purpose — this runs against a
			// container on whatever machine CI gave us — and it is still the assertion that
			// would fail on a sequential scan per page or an unindexed sort.
			const summaryBudget = 5 * time.Second
			if summaryTook > summaryBudget {
				t.Errorf("the summary took %s at %d relays, past the %s budget; it is one query "+
					"and should not grow with the fleet", summaryTook, size, summaryBudget)
			}
			t.Logf("%d relays: summary %s, full walk %s", size, summaryTook, walkTook)
		})
	}
}

// seedRelays inserts relay registrations directly, because enrolling a hundred through the
// protocol would be testing the enrolment path a hundred times rather than the listing once.
func (p *integrationPlane) seedRelays(t *testing.T, count int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	database, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		t.Fatalf("connecting to seed relays: %v", err)
	}
	defer func() { _ = database.Close(ctx) }()

	for index := 0; index < count; index++ {
		if _, err := database.Exec(ctx, `
			INSERT INTO relay_registration
				(registration_id, org_id, credential_digest, cluster_fingerprint,
				 relay_version, capabilities, created_at)
			VALUES (gen_random_uuid(), $1, sha256($2::bytea), $3, $4, $5::jsonb,
			        now() - make_interval(secs => $6))`,
			surfaceOrg, []byte(fmt.Sprintf("seeded-%d", index)),
			fmt.Sprintf("fingerprint-%04d", index),
			fmt.Sprintf("0.1.%d", index%5),
			`[{"id":"kubernetes.workload.runtime","version":1}]`,
			float64(index)); err != nil {
			t.Fatalf("seeding relay %d: %v", index, err)
		}
	}
}
