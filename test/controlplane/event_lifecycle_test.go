package controlplane

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

func TestLiveEventStreamClosesWhenSessionAccessEnds(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  string
	}{
		{"expiry", `UPDATE session SET expires_at=now() WHERE credential_digest=$1`},
		{"membership removal", `DELETE FROM organization_membership
			WHERE user_id=(SELECT user_id FROM session WHERE credential_digest=$1)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plane, _ := agentPlane(t, &blockingAgentMain{})
			_, turn := plane.openConversation(t, "access change", "investigate checkout")
			response := openEventStream(t, plane, turn, "")
			defer func() { _ = response.Body.Close() }()
			conn, err := pgx.Connect(t.Context(), plane.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close(context.Background()) }()
			digest := session.Digest(session.Token(plane.sessionCookie))
			if tag, err := conn.Exec(t.Context(), test.sql, digest); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("ending access: %v, %v", tag, err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, response.Body)
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(51 * time.Second):
				t.Fatal("stream stayed open after access ended")
			}
			var status int16
			if err := conn.QueryRow(t.Context(), `SELECT status FROM investigation WHERE investigation_id=$1`, turn).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != int16(investigation.StatusRunning) {
				t.Fatalf("Investigation status = %d after stream closure", status)
			}
			if test.name == "expiry" {
				identity := &identityPlane{api: plane.api}
				signedIn := identity.call(t, http.MethodPost, plane.base(surfaceOrg)+"/auth/local/sign-in", map[string]string{
					"email":    "admin@example.test",
					"password": "temporary integration test administrator password",
				})
				plane.sessionCookie = sessionCookie(t, signedIn)
				code, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
				if code != http.StatusOK {
					t.Fatalf("cancel after signing in = %d: %s", code, body)
				}
				// The blocking agent emits no events before cancellation.
				replay := openEventStream(t, plane, turn, "", http.Header{"Last-Event-Id": {"0"}})
				defer func() { _ = replay.Body.Close() }()
				events, err := io.ReadAll(replay.Body)
				if err != nil || !strings.Contains(string(events), "event: cancelled") {
					t.Fatalf("replay after signing in = %q: %v", events, err)
				}
			}
		})
	}
}

func TestEventStreamClosesAfterTerminalCursor(t *testing.T) {
	plane, _ := agentPlane(t, &blockingAgentMain{})
	_, turn := plane.openConversation(t, "replayed stream", "investigate checkout")
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	response := openEventStream(t, plane, turn, "?after=1")
	defer func() { _ = response.Body.Close() }()
	finished := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, response.Body)
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cursor after the terminal event left the stream open")
	}
}

func TestEventStreamClosesDuringGracefulShutdown(t *testing.T) {
	plane, _ := agentPlane(t, &blockingAgentMain{})
	_, turn := plane.openConversation(t, "shutdown stream", "investigate checkout")
	response := openEventStream(t, plane, turn, "")
	defer func() { _ = response.Body.Close() }()
	plane.shutdown()
	if plane.exitErr != nil {
		t.Fatalf("shutdown with a live event stream: %v", plane.exitErr)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("stream was interrupted instead of closing cleanly: %v", err)
	}
}

func TestEventStreamSurvivesOrdinaryWriteTimeout(t *testing.T) {
	t.Parallel()
	plane, _ := agentPlane(t, &blockingAgentMain{})
	assertLiveEventStream(t, plane, plane)
}

func TestEventStreamThroughReverseProxy(t *testing.T) {
	t.Parallel()
	plane, _ := agentPlane(t, &blockingAgentMain{})
	proxied := *plane
	proxied.api = startEventProxy(t, plane.api)
	assertLiveEventStream(t, plane, &proxied)
	_, turn := plane.openConversation(t, "proxy shutdown", "investigate checkout")
	response := openEventStream(t, &proxied, turn, "")
	defer func() { _ = response.Body.Close() }()
	plane.shutdown()
	if plane.exitErr != nil {
		t.Fatalf("shutdown through proxy: %v", plane.exitErr)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("proxy stream did not close cleanly: %v", err)
	}
}

func assertLiveEventStream(t *testing.T, plane, streamed *integrationPlane) {
	t.Helper()
	_, turn := plane.openConversation(t, "live stream", "investigate checkout")
	response := openEventStream(t, streamed, turn, "")
	defer func() { _ = response.Body.Close() }()
	scanner := bufio.NewScanner(response.Body)
	heartbeats := 0
	for scanner.Scan() {
		if scanner.Text() == ": keep-alive" {
			heartbeats++
			if heartbeats == 3 {
				break
			}
		}
	}
	if heartbeats != 3 {
		t.Fatalf("stream ended after %d heartbeats: %v", heartbeats, scanner.Err())
	}
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel = %d: %s", status, body)
	}
	ending := false
	for scanner.Scan() {
		ending = ending || strings.HasPrefix(scanner.Text(), "event: cancelled")
	}
	if scanner.Err() != nil || !ending {
		t.Fatalf("cancelled stream did not close with its ending: ending=%v, error=%v", ending, scanner.Err())
	}
	replay := openEventStream(t, streamed, turn, "", http.Header{"Last-Event-Id": {"1"}})
	defer func() { _ = replay.Body.Close() }()
	bodyBytes, err := io.ReadAll(replay.Body)
	if err != nil || len(bodyBytes) != 0 {
		t.Fatalf("terminal cursor replay = %q, error=%v", bodyBytes, err)
	}
}

func openEventStream(t *testing.T, plane *integrationPlane, turn, query string, headers ...http.Header) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		plane.base(surfaceOrg)+"/investigations/"+turn+"/events"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: session.CookieName, Value: plane.sessionCookie})
	for _, supplied := range headers {
		for name, values := range supplied {
			request.Header[name] = values
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		t.Fatalf("stream status = %d", response.StatusCode)
	}
	return response
}
