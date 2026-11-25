package controlplane

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestSnapshotStreamBoundsLegacyBacklogForSlowReaders(t *testing.T) {
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
				cfg.HTTPListenAddress = address
				dsn = cfg.DatabaseDSN
			}, app.Options{Model: &blockingModel{}})
			plane := &integrationPlane{controlPlane: running, api: address, intake: address}
			_, turn := plane.openConversation(t, "large replay", "investigate checkout")
			status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/investigations/"+turn+"/cancel", nil)
			if status != http.StatusOK {
				t.Fatalf("cancel = %d: %s", status, body)
			}
			seedEventBacklog(t, dsn, turn)
			if proxy {
				plane.api = startEventProxy(t, address)
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
		FROM generate_series(100, 20099) AS sequence`, turn, surfaceOrg)
	if err != nil {
		t.Fatal(err)
	}
}

func assertSlowEventReader(t *testing.T, plane *integrationPlane, turn string) {
	t.Helper()
	// Negotiate the bounded receive window during the handshake instead of
	// shrinking an already negotiated window and stalling the final drain.
	dialer := net.Dialer{
		Control: func(_, _ string, raw syscall.RawConn) error {
			var socketErr error
			if err := raw.Control(func(fd uintptr) {
				socketErr = setSlowReaderReceiveBuffer(fd)
			}); err != nil {
				return err
			}
			return socketErr
		},
	}
	connection, err := dialer.Dial("tcp", mustTCPAddress(t, plane.api).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
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
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d", response.StatusCode)
	}
	time.Sleep(time.Second)
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil || len(body) == 4096 || !strings.Contains(string(body), `"status":"cancelled"`) {
		t.Fatalf("slow reader received more than the current snapshot: bytes=%d, error=%v", len(body), err)
	}
	snapshots := assertSerializedSnapshots(t, string(body))
	if len(snapshots) != 1 {
		t.Fatalf("legacy backlog leaked into %d snapshots", len(snapshots))
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
