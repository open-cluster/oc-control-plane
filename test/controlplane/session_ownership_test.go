package controlplane

import (
	"net/http"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/session"
)

func TestLogoutRevokesSessionWithoutMembershipAndClearsCookie(t *testing.T) {
	plane := startIdentityPlane(t)
	base := "http://" + plane.operator + "/api/v1"
	created := plane.call(t, http.MethodPost, base+"/auth/local/bootstrap", map[string]any{
		"email": "admin@example.test", "displayName": "Admin",
		"password": "initial administrator password",
	}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", created.status, created.body)
	}
	cookie := sessionCookie(t, created)
	ended := plane.call(t, http.MethodDelete, base+"/session", nil, asSession(cookie))
	if ended.status != http.StatusOK {
		t.Fatalf("logout = %d: %s", ended.status, ended.body)
	}
	assertSessionCookieCleared(t, ended)
	stale := plane.call(t, http.MethodGet, base+"/session", nil, asSession(cookie))
	if stale.status != http.StatusUnauthorized {
		t.Fatalf("logged-out session = %d: %s", stale.status, stale.body)
	}
	for _, credential := range []string{cookie, ""} {
		repeated := plane.call(t, http.MethodDelete, base+"/session", nil, asSession(credential))
		if repeated.status != http.StatusOK {
			t.Fatalf("repeated logout = %d: %s", repeated.status, repeated.body)
		}
		assertSessionCookieCleared(t, repeated)
	}
	untrusted := plane.call(t, http.MethodDelete, base+"/session", nil,
		func(request *http.Request) { request.Header.Set("Origin", "https://attacker.example") })
	if untrusted.status != http.StatusForbidden {
		t.Fatalf("cross-origin logout = %d: %s", untrusted.status, untrusted.body)
	}
}

func assertSessionCookieCleared(t *testing.T, response answer) {
	t.Helper()
	for _, cookie := range response.cookies {
		if cookie.Name == session.CookieName && cookie.Value == "" && cookie.MaxAge < 0 {
			return
		}
	}
	t.Fatalf("no session deletion cookie: %+v", response.cookies)
}

func TestLogoutReportsDatabaseFailureAndClearsCookie(t *testing.T) {
	plane := startIdentityPlane(t)
	cookie := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin", "initial administrator password")
	plane.database.closeGate()
	defer plane.database.openGate()
	response := plane.call(t, http.MethodDelete, "http://"+plane.operator+"/api/v1/session", nil, asSession(cookie))
	if response.status != http.StatusServiceUnavailable {
		t.Fatalf("logout during outage = %d: %s", response.status, response.body)
	}
	assertSessionCookieCleared(t, response)
	plane.database.openGate()
	readSession(t, plane, cookie)
}
