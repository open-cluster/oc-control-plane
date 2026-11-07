package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

const signupBody = `{"organizationName":"Platform Team","email":"admin@example.test","displayName":"Admin","password":"a strong administrator password"}`

func TestTokenlessSignupThroughSameOriginProxyIssuesUsableCookie(t *testing.T) {
	var proxy http.Handler
	frontend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(frontend.Close)
	plane := startIdentityPlane(t, func(cfg *config.Config) {
		cfg.PublicURL = frontend.URL
	})
	backend, err := url.Parse("http://" + plane.api)
	if err != nil {
		t.Fatal(err)
	}
	proxy = httputil.NewSingleHostReverseProxy(backend)
	client := frontend.Client()
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, frontend.URL+"/api/v1/auth/local/bootstrap", strings.NewReader(signupBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", frontend.URL)
	created, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = created.Body.Close() }()
	raw, err := io.ReadAll(created.Body)
	if err != nil {
		t.Fatal(err)
	}
	if created.StatusCode != http.StatusCreated || strings.TrimSpace(string(raw)) != `{"bootstrapped":true}` {
		t.Fatalf("signup = %d: %s", created.StatusCode, raw)
	}
	cookies := created.Cookies()
	if len(cookies) != 1 || cookies[0].Name != session.CookieName || cookies[0].Value == "" ||
		!cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Path != "/" || cookies[0].Domain != "" ||
		cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("signup cookie = %+v", cookies)
	}
	current, err := client.Get(frontend.URL + "/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Body.Close() }()
	var identity sessionBody
	if err = json.NewDecoder(current.Body).Decode(&identity); err != nil {
		t.Fatal(err)
	}
	if current.StatusCode != http.StatusOK || identity.User.Email != "admin@example.test" ||
		identity.Organization.DisplayName != "Platform Team" || identity.Organization.Role != "admin" {
		t.Fatalf("immediate session = %d: %+v", current.StatusCode, identity)
	}
}

func TestLocalSignupRequiresConfiguredOrigin(t *testing.T) {
	plane := startIdentityPlane(t)
	for _, origin := range []string{"", "null", "https://untrusted.example.test"} {
		response := plane.call(t, http.MethodPost, "http://"+plane.api+"/api/v1/auth/local/bootstrap",
			json.RawMessage(signupBody), func(request *http.Request) {
				request.Header.Set("Origin", origin)
				request.Header.Set("X-Forwarded-For", "127.0.0.1")
				request.Header.Set("X-Forwarded-Host", "localhost")
			})
		if response.status != http.StatusForbidden || len(response.cookies) != 0 {
			t.Fatalf("signup with Origin %q = %d: %s", origin, response.status, response.body)
		}
	}
}

func TestLocalSignupRejectsTrailingOrOversizedJSON(t *testing.T) {
	plane := startIdentityPlane(t)
	for _, body := range []string{signupBody + ` {}`, signupBody + strings.Repeat(" ", 64*1024)} {
		request, err := http.NewRequest(http.MethodPost, "http://"+plane.api+"/api/v1/auth/local/bootstrap", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", "http://"+plane.api)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || len(response.Cookies()) != 0 {
			t.Fatalf("invalid signup JSON = %d", response.StatusCode)
		}
	}
}

func TestLocalSignupBoundsParallelPasswordHashing(t *testing.T) {
	plane := startIdentityPlane(t)
	const attempts = 16
	statuses := make(chan int, attempts)
	start := make(chan struct{})
	var waiting sync.WaitGroup
	for range attempts {
		waiting.Go(func() {
			<-start
			answer := plane.call(t, http.MethodPost, "http://"+plane.api+"/api/v1/auth/local/bootstrap", json.RawMessage(signupBody))
			statuses <- answer.status
		})
	}
	close(start)
	waiting.Wait()
	close(statuses)
	var created int
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests, http.StatusConflict:
		default:
			t.Fatalf("concurrent signup = %d", status)
		}
	}
	if created != 1 {
		t.Fatalf("concurrent signup created=%d, want one", created)
	}
}

func TestLocalSignupAuditFailureRollsBackAndCanBeRetried(t *testing.T) {
	plane := startIdentityPlane(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `CREATE FUNCTION refuse_signup_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected signup audit failure'; END $$;
		CREATE TRIGGER refuse_signup_audit BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION refuse_signup_audit()`); err != nil {
		t.Fatal(err)
	}
	failed := plane.call(t, http.MethodPost, "http://"+plane.api+"/api/v1/auth/local/bootstrap", json.RawMessage(signupBody))
	if failed.status != http.StatusServiceUnavailable || len(failed.cookies) != 0 {
		t.Fatalf("unrecorded signup = %d: %s", failed.status, failed.body)
	}
	var rows int
	if err = connection.QueryRow(ctx, `SELECT (SELECT count(*) FROM organization) +
		(SELECT count(*) FROM app_user) + (SELECT count(*) FROM local_password) +
		(SELECT count(*) FROM organization_membership) + (SELECT count(*) FROM session) +
		(SELECT count(*) FROM audit_event)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed signup left %d identity/audit rows: %v", rows, err)
	}
	if _, err = connection.Exec(ctx, `DROP TRIGGER refuse_signup_audit ON audit_event`); err != nil {
		t.Fatal(err)
	}
	created := plane.call(t, http.MethodPost, "http://"+plane.api+"/api/v1/auth/local/bootstrap", json.RawMessage(signupBody))
	if created.status != http.StatusCreated {
		t.Fatalf("signup retry = %d: %s", created.status, created.body)
	}
	readSession(t, plane, sessionCookie(t, created))
	plane.shutdown()
	restarted := startIdentityPlane(t, func(cfg *config.Config) { cfg.DatabaseDSN = plane.dsn })
	again := restarted.call(t, http.MethodPost, "http://"+restarted.api+"/api/v1/auth/local/bootstrap", json.RawMessage(signupBody))
	if again.status != http.StatusConflict || len(again.cookies) != 0 {
		t.Fatalf("signup after restart with setup enabled = %d: %s", again.status, again.body)
	}
}

func TestLocalSignupDatabaseOutageIssuesNoCookie(t *testing.T) {
	plane := startIdentityPlane(t)
	plane.database.closeGate()
	failed := plane.call(t, http.MethodPost, "http://"+plane.api+"/api/v1/auth/local/bootstrap", json.RawMessage(signupBody))
	if failed.status < 500 || len(failed.cookies) != 0 {
		t.Fatalf("signup during database outage = %d: %s", failed.status, failed.body)
	}
	plane.database.openGate()
	for deadline := time.Now().Add(5 * time.Second); ; {
		retried := plane.call(t, http.MethodPost, "http://"+plane.api+"/api/v1/auth/local/bootstrap", json.RawMessage(signupBody))
		if retried.status == http.StatusCreated {
			readSession(t, plane, sessionCookie(t, retried))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("signup after database recovery = %d: %s", retried.status, retried.body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
