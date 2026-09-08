package controlplane

import (
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestEventStreamBoundsSlowReaders(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		name := "listener"
		if proxy {
			name = "proxy"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			address := freeAddress(t)
			var dsn string
			running := startControlPlaneRunning(t, func(cfg *config.Config) {
				cfg.HTTPAddress = address
				digest := sha256.Sum256([]byte(surfaceToken))
				cfg.OperatorTokenDigest = digest[:]
				dsn = cfg.DatabaseDSN
			}, app.Options{Agent: &blockingAgentMain{}})
			plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
			_, turn := plane.openConversation(t, "large replay", "investigate checkout")
			seedEventBacklog(t, dsn, turn)
			status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
			if status != http.StatusOK {
				t.Fatalf("cancel = %d: %s", status, body)
			}
			if proxy {
				plane.operator = startEventProxy(t, address)
			}
			assertSlowEventReader(t, plane, turn)
		})
	}
}

func seedEventBacklog(t *testing.T, dsn, turn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	_, err = connection.Exec(ctx, `INSERT INTO investigation_event
		(investigation_id, org_id, sequence, at, type, payload)
		SELECT $1, $2, sequence, clock_timestamp(), 2, jsonb_build_object('text', repeat('x', 512))
		FROM generate_series(1, 20000) AS sequence`, turn, surfaceOrg)
	if err != nil {
		t.Fatal(err)
	}
}

func assertSlowEventReader(t *testing.T, plane *integrationPlane, turn string) {
	t.Helper()
	connection, err := net.DialTCP("tcp", nil, mustTCPAddress(t, plane.operator))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return connection, nil
	}}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: session.CookieName, Value: plane.sessionCookie})
	selectOrganizationFromURL(request)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d", response.StatusCode)
	}
	time.Sleep(15 * time.Second)
	if err := connection.SetReadBuffer(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err == nil || strings.Contains(string(body), "event: cancelled") {
		t.Fatalf("slow reader remained connected: bytes=%d, error=%v", len(body), err)
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("server did not close the slow reader: %v", err)
	}
	// A normal reconnect can still reach the durable ending after the slow connection closes.
	replay := openEventStream(t, plane, turn, "?after=20000")
	defer func() { _ = replay.Body.Close() }()
	ending, err := io.ReadAll(replay.Body)
	if err != nil || !strings.Contains(string(ending), "event: cancelled") {
		t.Fatalf("reconnect lost the terminal event: %q, error=%v", ending, err)
	}
}

func mustTCPAddress(t *testing.T, address string) *net.TCPAddr {
	t.Helper()
	resolved, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
