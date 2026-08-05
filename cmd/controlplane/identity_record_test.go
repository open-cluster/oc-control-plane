package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/session"
)

// "An audit write that fails rolls back the operation it describes."
//
// That sentence is the whole reason the event shares a transaction with the change, and it is
// the one claim in this slice that cannot be believed from reading the code — a helper that
// LOOKS transactional and commits the change anyway would pass every other test here. So the
// audit write is forced to fail, in the database, and the operation is asserted not to have
// happened.
func TestOperatorIdentity_AnUnrecordableChangeDoesNotHappen(t *testing.T) {
	plane := startIdentityPlane(t)

	placements := openPlacement(t, plane.dsn)
	organization := namedOrganization(t, identityOrg)
	connection, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("reaching the placement: %v", err)
	}

	// A trigger that refuses exactly the event an environment creation writes, and nothing
	// else. Refusing every insert would also break the sign-in path and the refusal recording,
	// and the test would then prove that a broken database breaks things.
	if _, err := connection.Exec(t.Context(), `
		CREATE OR REPLACE FUNCTION refuse_one_action() RETURNS trigger AS $$
		BEGIN
		    IF NEW.action = 'environment.created' THEN
		        RAISE EXCEPTION 'the record refused this event';
		    END IF;
		    RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		CREATE TRIGGER audit_event_refuses_one_action
		    BEFORE INSERT ON audit_event
		    FOR EACH ROW
		EXECUTE FUNCTION refuse_one_action()`); err != nil {
		t.Fatalf("arranging the forced failure: %v", err)
	}

	const name = "A Scope Nobody Could Record"
	answered := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/environments",
		map[string]string{"name": name}, asBootstrap)

	if answered.status == http.StatusCreated {
		t.Fatalf("an unrecordable change was accepted: %s", answered.body)
	}
	// The operator is told which failure this was, because "it worked" about a change nobody
	// can attribute is the answer this whole design refuses to give.
	if answered.status != http.StatusServiceUnavailable {
		t.Errorf("an unrecordable change answered %d, want 503: %s", answered.status, answered.body)
	}

	// And the Environment is not there. This is the assertion that matters: a response that
	// said no while the row committed would be worse than one that said yes.
	var rows int
	if err := connection.QueryRow(t.Context(),
		`SELECT count(*) FROM environment WHERE organization = $1 AND name = $2`,
		identityOrg, name).Scan(&rows); err != nil {
		t.Fatalf("counting environments: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d environments were created despite the record refusing the event; the "+
			"change and the event have to commit together or not at all", rows)
	}

	if _, err := connection.Exec(t.Context(),
		`DROP TRIGGER audit_event_refuses_one_action ON audit_event`); err != nil {
		t.Fatalf("removing the forced failure: %v", err)
	}

	// With the record working again, the same request succeeds — so what failed above was the
	// audit write and not the operation.
	retried := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/environments",
		map[string]string{"name": name}, asBootstrap)
	if retried.status != http.StatusCreated {
		t.Errorf("the same request with the record working = %d: %s",
			retried.status, retried.body)
	}
}

// Story 5: a session that has run out returns the operator to sign-in WITH AN EXPLANATION, so
// they do not read a screen of error states and assume the product is broken.
//
// Expiry and revocation are told apart because they lead to different next actions: sign in
// again, or ask somebody why you cannot. A cookie nobody issued stays the bare refusal, so a
// guess at somebody else's session learns nothing from being told it is not expired.
func TestOperatorIdentity_AnEndedSessionSaysWhy(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "viewer",
		"verifiedDomains": []string{"example.test"},
	})

	placements := openPlacement(t, plane.dsn)
	organization := namedOrganization(t, identityOrg)
	connection, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("reaching the placement: %v", err)
	}

	t.Run("an expired session says it expired", func(t *testing.T) {
		issuer.assert(t, "sub", "expiring-1")
		issuer.assert(t, "email", "expiring@example.test")
		cookie := sessionCookie(t, signIn(t, plane, provider))

		// Moved into the past directly. Waiting out a real lifetime would mean a test that
		// sleeps for the shortest one the product will serve, which is five minutes. Both
		// stamps move, because the row carries a CHECK that a session expires after it was
		// issued — and that constraint is worth keeping rather than working around.
		if _, err := connection.Exec(t.Context(), `
			UPDATE operator_session
			   SET issued_at  = now() - interval '2 hours',
			       expires_at = now() - interval '1 minute'
			 WHERE token_digest = $1`, session.Digest(session.Token(cookie))); err != nil {
			t.Fatalf("expiring the session: %v", err)
		}

		answered := plane.call(t, http.MethodGet,
			plane.base(identityOrg)+"/relays", nil, asSession(cookie))
		if answered.status != http.StatusUnauthorized {
			t.Fatalf("an expired session = %d, want 401: %s", answered.status, answered.body)
		}
		if !strings.Contains(answered.body, "session_expired") {
			t.Errorf("the refusal says %q; an operator returned to sign-in with no explanation "+
				"reads a screen of error states and assumes the product is broken",
				answered.body)
		}
	})

	t.Run("a revoked session says it was revoked", func(t *testing.T) {
		issuer.assert(t, "sub", "revoked-1")
		issuer.assert(t, "email", "revoked@example.test")
		cookie := sessionCookie(t, signIn(t, plane, provider))

		who := readSession(t, plane, cookie)
		revoked := plane.call(t, http.MethodPost,
			plane.base(identityOrg)+"/members/"+who.Principal.ID+"/revoke-sessions",
			nil, asBootstrap)
		if revoked.status != http.StatusOK {
			t.Fatalf("revoking = %d: %s", revoked.status, revoked.body)
		}

		answered := plane.call(t, http.MethodGet,
			plane.base(identityOrg)+"/relays", nil, asSession(cookie))
		if answered.status != http.StatusUnauthorized {
			t.Fatalf("a revoked session = %d, want 401", answered.status)
		}
		if !strings.Contains(answered.body, "session_revoked") {
			t.Errorf("the refusal says %q; an administrator ended this session and the person "+
				"holding it should be told that rather than that it timed out", answered.body)
		}
	})

	// A cookie nobody issued must NOT be told which of the reasons it is not. That would let
	// somebody trying values learn when they had hit a real session.
	t.Run("a cookie nobody issued says nothing", func(t *testing.T) {
		answered := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays", nil,
			asSession("a-value-nobody-was-ever-issued"))
		if answered.status != http.StatusUnauthorized {
			t.Fatalf("an invented cookie = %d, want 401", answered.status)
		}
		for _, told := range []string{"session_expired", "session_revoked"} {
			if strings.Contains(answered.body, told) {
				t.Errorf("an invented cookie was told %q; somebody trying values would learn "+
					"when they had hit a real session", told)
			}
		}
	})
}

// Just-in-time provisioning is somebody gaining access to a tenant without an administrator
// doing anything. It is the one membership change nobody deliberately made, so it is the one
// most worth being able to find afterwards.
func TestOperatorIdentity_ProvisioningIsOnTheRecordAndOnlyOnce(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "viewer",
		"verifiedDomains": []string{"example.test"},
	})
	issuer.assert(t, "sub", "provisioned-1")
	issuer.assert(t, "email", "provisioned@example.test")

	// Twice. The second sign-in provisions nothing, and must therefore record nothing: a row
	// per sign-in saying nothing happened would bury the one that says somebody got in.
	for attempt := range 2 {
		if completed := signIn(t, plane, provider); completed.status != http.StatusFound {
			t.Fatalf("sign-in %d = %d: %s", attempt+1, completed.status, completed.body)
		}
	}

	answered := plane.call(t, http.MethodGet,
		plane.base(identityOrg)+"/audit-events?limit=100", nil, asBootstrap)
	var trail auditBody
	decodeAnswer(t, answered, &trail)

	provisioned, signedIn := 0, 0
	for _, event := range trail.Events {
		switch event.Action {
		case "user.provisioned":
			provisioned++
			if event.Detail["role"] != "viewer" {
				t.Errorf("the provisioning event records the role %v, want viewer",
					event.Detail["role"])
			}
		case "session.sign-in.completed":
			signedIn++
		}
	}
	if provisioned != 1 {
		t.Errorf("%d provisioning events for one person signing in twice, want 1", provisioned)
	}
	if signedIn != 2 {
		t.Errorf("%d completed sign-ins recorded, want 2", signedIn)
	}
}

// A sign-in is a credential coming into existence. The row and the event that names its holder
// commit together, so a live session nobody can attribute is not a state this reaches.
func TestOperatorIdentity_ASessionAndItsRecordCommitTogether(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"verifiedDomains": []string{"example.test"},
	})
	issuer.assert(t, "sub", "atomic-1")
	issuer.assert(t, "email", "atomic@example.test")

	placements := openPlacement(t, plane.dsn)
	organization := namedOrganization(t, identityOrg)
	connection, err := placements.Pool(organization)
	if err != nil {
		t.Fatalf("reaching the placement: %v", err)
	}
	if _, err := connection.Exec(t.Context(), `
		CREATE OR REPLACE FUNCTION refuse_sign_in_events() RETURNS trigger AS $$
		BEGIN
		    IF NEW.action = 'session.sign-in.completed' THEN
		        RAISE EXCEPTION 'the record refused this event';
		    END IF;
		    RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		CREATE TRIGGER audit_event_refuses_sign_in
		    BEFORE INSERT ON audit_event
		    FOR EACH ROW
		EXECUTE FUNCTION refuse_sign_in_events()`); err != nil {
		t.Fatalf("arranging the forced failure: %v", err)
	}
	t.Cleanup(func() {
		_, _ = connection.Exec(t.Context(),
			`DROP TRIGGER IF EXISTS audit_event_refuses_sign_in ON audit_event`)
	})

	before := countSessions(t, plane)
	completed := signIn(t, plane, provider)
	if completed.status == http.StatusFound {
		t.Errorf("a sign-in nobody could record issued a session: %s", completed.location)
	}
	for _, cookie := range completed.cookies {
		if cookie.Name == session.CookieName && cookie.Value != "" {
			t.Error("a cookie was set for a session whose record failed")
		}
	}
	if after := countSessions(t, plane); after != before {
		t.Errorf("%d session rows survived a failed record, want %d", after, before)
	}
}

func countSessions(t *testing.T, plane *identityPlane) int {
	t.Helper()

	placements := openPlacement(t, plane.dsn)
	connection, err := placements.Pool(namedOrganization(t, identityOrg))
	if err != nil {
		t.Fatalf("reaching the placement: %v", err)
	}
	var rows int
	if err := connection.QueryRow(t.Context(),
		`SELECT count(*) FROM operator_session`).Scan(&rows); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	return rows
}
