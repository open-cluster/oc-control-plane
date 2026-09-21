package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

type sessionBody struct {
	Principal struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"principal"`
	Organization struct {
		ID           string `json:"id"`
		Organization string `json:"organizationId"`
		DisplayName  string `json:"displayName"`
		Role         string `json:"role"`
	} `json:"organization"`
}

func readSession(t *testing.T, plane *identityPlane, cookie string) sessionBody {
	t.Helper()
	answer := plane.call(t, http.MethodGet, "http://"+plane.operator+"/api/v1/session", nil, asSession(cookie))
	if answer.status != http.StatusOK {
		t.Fatalf("session = %d: %s", answer.status, answer.body)
	}
	var body sessionBody
	decodeAnswer(t, answer, &body)
	return body
}

func bootstrapIdentityAdmin(
	t *testing.T, plane *identityPlane, email, displayName, password string,
) string {
	t.Helper()
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organizationName": "Operations", "email": email,
			"displayName": displayName, "password": password,
		}, asBootstrap)
	if bootstrapped.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", bootstrapped.status, bootstrapped.body)
	}
	cookie := sessionCookie(t, bootstrapped)
	seedTestOrganization(t, plane.dsn, identityOrg, email)
	return cookie
}

func TestLocalBootstrapCreatesOrganizationAndAdmin(t *testing.T) {
	plane := startIdentityPlane(t)

	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organizationName": "Platform Team",
			"email":            "ada@example.test",
			"displayName":      "Ada Lovelace",
			"password":         "correct horse battery staple",
		}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", created.status, created.body)
	}

	who := readSession(t, plane, sessionCookie(t, created))
	if who.Principal.DisplayName != "Ada Lovelace" ||
		who.Organization.DisplayName != "Platform Team" || who.Organization.Role != "admin" {
		t.Fatalf("bootstrap session = %+v", who)
	}
}

func TestSessionLifetimeIsReadOnlyDeploymentPolicy(t *testing.T) {
	const lifetime = 37 * time.Minute
	plane := startIdentityPlane(t, func(cfg *config.Config) { cfg.SessionLifetime = lifetime })
	before := time.Now().UTC()
	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organizationName": "Operations",
			"email":            "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	token := sessionCookie(t, created)
	var expires time.Time
	for _, cookie := range created.cookies {
		if cookie.Name == session.CookieName {
			expires = cookie.Expires
		}
	}
	if expires.Before(before.Add(lifetime-time.Second)) || expires.After(time.Now().UTC().Add(lifetime+time.Second)) {
		t.Fatalf("session expires at %v for configured lifetime %v", expires, lifetime)
	}

	seedTestOrganization(t, plane.dsn, identityOrg, "admin@example.test")
	url := plane.base(identityOrg) + "/policy"
	refused := plane.call(t, http.MethodPut, url, map[string]any{
		"sessionLifetimeSeconds": int(lifetime.Seconds()), "auditRetentionDays": 45,
	}, asSession(token))
	if refused.status != http.StatusBadRequest {
		t.Fatalf("writable session lifetime = %d: %s", refused.status, refused.body)
	}
	updated := plane.call(t, http.MethodPut, url,
		map[string]any{"auditRetentionDays": 45}, asSession(token))
	if updated.status != http.StatusOK {
		t.Fatalf("audit retention update = %d: %s", updated.status, updated.body)
	}
	var policy struct {
		SessionLifetimeSeconds int `json:"sessionLifetimeSeconds"`
		AuditRetentionDays     int `json:"auditRetentionDays"`
	}
	decodeAnswer(t, updated, &policy)
	if policy.SessionLifetimeSeconds != int(lifetime.Seconds()) || policy.AuditRetentionDays != 45 {
		t.Fatalf("effective policy = %+v", policy)
	}
}

func TestAdminCreatesLocalUserWithoutIdentityProviderChoice(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organizationName": "Operations",
			"email":            "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)
	seedTestOrganization(t, plane.dsn, identityOrg, "admin@example.test")

	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/local-users", map[string]any{
			"email": "member@example.test", "displayName": "Member",
			"role": "viewer", "password": "member password long enough",
		}, asSession(admin), inOrganization(identityOrg))
	if created.status != http.StatusCreated {
		t.Fatalf("creating local User = %d: %s", created.status, created.body)
	}
	var member struct {
		UserID string `json:"userId"`
		Role   string `json:"role"`
	}
	decodeAnswer(t, created, &member)
	if _, err := uuid.Parse(member.UserID); err != nil || member.Role != "viewer" {
		t.Fatalf("created local User membership = %+v, user id error = %v", member, err)
	}
}

func TestMembershipIsTheOrganizationUserRelation(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organizationName": "Operations",
			"email":            "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)
	seedTestOrganization(t, plane.dsn, identityOrg, "admin@example.test")
	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/local-users", map[string]any{
			"email": "member@example.test", "role": "viewer",
			"password": "member password long enough",
		}, asSession(admin), inOrganization(identityOrg))
	var member struct {
		UserID string `json:"userId"`
	}
	decodeAnswer(t, created, &member)
	signedIn := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/sign-in", map[string]any{
			"email": "member@example.test", "password": "member password long enough",
		})
	memberSession := sessionCookie(t, signedIn)
	changed := plane.call(t, http.MethodPatch,
		"http://"+plane.operator+"/api/v1/members/"+member.UserID,
		map[string]any{"role": "editor"}, asSession(admin), inOrganization(identityOrg))
	if changed.status != http.StatusOK || !strings.Contains(changed.body, `"role":"editor"`) {
		t.Fatalf("changing membership = %d: %s", changed.status, changed.body)
	}
	if who := readSession(t, plane, memberSession); who.Organization.Role != "editor" {
		t.Fatalf("session retained stale Role: %+v", who.Organization)
	}
	removed := plane.call(t, http.MethodDelete,
		"http://"+plane.operator+"/api/v1/members/"+member.UserID,
		nil, asSession(admin), inOrganization(identityOrg))
	if removed.status != http.StatusNoContent {
		t.Fatalf("removing membership = %d: %s", removed.status, removed.body)
	}
	denied := plane.call(t, http.MethodGet, "http://"+plane.operator+"/api/v1/session", nil,
		asSession(memberSession))
	if denied.status != http.StatusUnauthorized {
		t.Fatalf("removed membership still authenticated = %d: %s", denied.status, denied.body)
	}
	refused := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{"email": "member@example.test", "password": "member password long enough"})
	if refused.status != http.StatusForbidden {
		t.Fatalf("membership-free User signed in = %d: %s", refused.status, refused.body)
	}
	connection, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	var users int
	if err = connection.QueryRow(context.Background(), `SELECT count(*) FROM app_user WHERE user_id=$1`, member.UserID).Scan(&users); err != nil || users != 1 {
		t.Fatalf("durable User after membership removal = %d: %v", users, err)
	}
}

func TestOrganizationKeepsAnAdmin(t *testing.T) {
	plane := startIdentityPlane(t)
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	adminID := readSession(t, plane, admin).Principal.ID
	memberURL := "http://" + plane.operator + "/api/v1/members/" + adminID

	for _, change := range []struct {
		name   string
		method string
		body   any
	}{
		{name: "role change", method: http.MethodPatch, body: map[string]any{"role": "viewer"}},
		{name: "removal", method: http.MethodDelete},
	} {
		t.Run(change.name, func(t *testing.T) {
			answer := plane.call(t, change.method, memberURL, change.body,
				asSession(admin), inOrganization(identityOrg))
			if answer.status != http.StatusConflict {
				t.Fatalf("last Admin change = %d: %s", answer.status, answer.body)
			}
		})
	}
}

func TestSessionListsOrganizationMetadataAndRole(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organizationName": "Operations",
			"email":            "admin@example.test", "password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)
	seedTestOrganization(t, plane.dsn, identityOrg, "admin@example.test")

	who := readSession(t, plane, admin)
	if who.Organization.Organization != identityOrg || who.Organization.DisplayName != "Operations" {
		t.Fatalf("session Organization = %+v", who.Organization)
	}
}

func TestLocalAuthenticationBootstrapsOneAdminAndSignsIn(t *testing.T) {
	plane := startIdentityPlane(t)
	created := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"organizationName": "Operations",
		"email":            "ada@example.test",
		"displayName":      "Ada Lovelace",
		"password":         "correct horse battery staple",
	}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", created.status, created.body)
	}
	bootstrapCookie := sessionCookie(t, created)
	retiredBootstrap := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/members", nil, asBootstrap, inOrganization(identityOrg))
	if retiredBootstrap.status != http.StatusUnauthorized {
		t.Fatalf("bootstrap token after first Admin = %d: %s", retiredBootstrap.status, retiredBootstrap.body)
	}

	who := readSession(t, plane, bootstrapCookie)
	if who.Principal.DisplayName != "Ada Lovelace" || who.Organization.Role != "admin" {
		t.Fatalf("bootstrap session = %+v", who)
	}
	seedTestOrganization(t, plane.dsn, identityOrg, "ada@example.test")

	again := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"organizationName": "Operations",
		"email":            "grace@example.test",
		"displayName":      "Grace Hopper",
		"password":         "another correct horse battery staple",
	}, asBootstrap)
	if again.status != http.StatusConflict || !strings.Contains(again.body, "already") {
		t.Fatalf("second bootstrap = %d: %s", again.status, again.body)
	}

	signedIn := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{
			"email": "ADA@example.test", "password": "correct horse battery staple",
		})
	if signedIn.status != http.StatusOK {
		t.Fatalf("local sign-in = %d: %s", signedIn.status, signedIn.body)
	}
	readSession(t, plane, sessionCookie(t, signedIn))

	refused := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{
			"email": "ada@example.test", "password": "wrong password",
		})
	if refused.status != http.StatusForbidden || strings.Contains(refused.body, "password") {
		t.Fatalf("wrong password = %d: %s", refused.status, refused.body)
	}
}

func TestAuthenticatedOrganizationManagementIsNotExposed(t *testing.T) {
	plane := startIdentityPlane(t)
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	organizationsURL := "http://" + plane.operator + "/api/v1/organizations"
	for _, answer := range []answer{
		plane.call(t, http.MethodGet, organizationsURL, nil, asSession(admin)),
		plane.call(t, http.MethodPost, organizationsURL,
			map[string]any{"displayName": "Second"}, asSession(admin)),
	} {
		if answer.status != http.StatusNotFound {
			t.Fatalf("Organization management = %d: %s", answer.status, answer.body)
		}
	}
}

func TestLocalUserCreationRejectsIdentityProviderChoice(t *testing.T) {
	plane := startIdentityPlane(t)
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	localUsersURL := "http://" + plane.operator + "/api/v1/local-users"
	member := map[string]any{
		"email": "member@example.test", "displayName": "Member",
		"role": "viewer", "password": "member password long enough", "identityKind": "oidc",
	}
	providerChoice := plane.call(t, http.MethodPost, localUsersURL, member,
		asSession(admin), inOrganization(identityOrg))
	if providerChoice.status != http.StatusBadRequest {
		t.Fatalf("local User with identityKind = %d: %s", providerChoice.status, providerChoice.body)
	}
	delete(member, "identityKind")
	local := plane.call(t, http.MethodPost, localUsersURL, member,
		asSession(admin), inOrganization(identityOrg))
	if local.status != http.StatusCreated {
		t.Fatalf("local member = %d: %s", local.status, local.body)
	}
}

func TestUsersManageOnlyTheirOwnGlobalSessions(t *testing.T) {
	plane := startIdentityPlane(t)
	base := "http://" + plane.operator + "/api/v1"
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin", "initial administrator password")
	created := plane.call(t, http.MethodPost, base+"/local-users", map[string]any{
		"email": "member@example.test", "role": "viewer", "password": "member password long enough",
	}, asSession(admin), inOrganization(identityOrg))
	if created.status != http.StatusCreated {
		t.Fatalf("create member = %d: %s", created.status, created.body)
	}
	login := plane.call(t, http.MethodPost, base+"/auth/local/sign-in", map[string]any{
		"email": "member@example.test", "password": "member password long enough",
	})
	member := sessionCookie(t, login)
	type listedSession struct {
		ID              string          `json:"id"`
		ClientUserAgent string          `json:"clientUserAgent"`
		RemoteAddr      string          `json:"remoteAddr"`
		UserAgent       json.RawMessage `json:"userAgent"`
		Address         json.RawMessage `json:"address"`
	}
	list := func(cookie string) []listedSession {
		response := plane.call(t, http.MethodGet, base+"/sessions", nil, asSession(cookie))
		if response.status != http.StatusOK {
			t.Fatalf("list = %d: %s", response.status, response.body)
		}
		var body struct {
			Sessions []listedSession `json:"sessions"`
		}
		decodeInto(t, response.body, &body)
		return body.Sessions
	}
	owned := list(member)
	if len(owned) != 1 {
		t.Fatalf("member sessions = %+v", owned)
	}
	if owned[0].ClientUserAgent == "" || owned[0].RemoteAddr == "" ||
		owned[0].UserAgent != nil || owned[0].Address != nil {
		t.Fatalf("member session client metadata = %+v", owned[0])
	}
	admins := list(admin)
	if len(admins) != 1 || admins[0].ID == owned[0].ID {
		t.Fatalf("admin sessions = %+v", admins)
	}
	refused := plane.call(t, http.MethodDelete, base+"/sessions/"+owned[0].ID, nil, asSession(admin))
	if refused.status != http.StatusNotFound {
		t.Fatalf("admin revoking another User = %d: %s", refused.status, refused.body)
	}
	readSession(t, plane, member)
	revoked := plane.call(t, http.MethodDelete, base+"/sessions/"+owned[0].ID, nil, asSession(member))
	if revoked.status != http.StatusNoContent {
		t.Fatalf("own revocation = %d: %s", revoked.status, revoked.body)
	}
	assertSessionCookieCleared(t, revoked)
	who := plane.call(t, http.MethodGet, base+"/session", nil, asSession(member))
	if who.status != http.StatusUnauthorized {
		t.Fatalf("revoked session = %d: %s", who.status, who.body)
	}
	readSession(t, plane, admin)
}
func TestSessionDescribesTheVerifiedSelectionAndBrowserSecurity(t *testing.T) {
	plane := startIdentityPlane(t)
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	who := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/session", nil,
		asSession(admin), inOrganization(identityOrg))
	if who.status != http.StatusOK {
		t.Fatalf("session = %d: %s", who.status, who.body)
	}
	for _, fact := range []string{
		`"organization":{"organizationId":"` + identityOrg + `","displayName":"Operations","role":"admin"`,
		`"authenticationMethod":"local"`,
		`"csrf":{"mode":"origin","requiredForUnsafeMethods":true}`,
	} {
		if !strings.Contains(who.body, fact) {
			t.Errorf("session omits %s: %s", fact, who.body)
		}
	}
}

func TestLocalBootstrapRefusesWhenCredentialIsRetired(t *testing.T) {
	plane := startIdentityPlane(t, func(cfg *config.Config) { cfg.BootstrapTokenDigest = nil })
	answer := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"organizationName": "Operations",
		"email":            "viewer@example.test", "displayName": "Viewer",
		"password": "correct horse battery staple",
	}, asBootstrap)
	if answer.status != http.StatusUnauthorized {
		t.Fatalf("disabled bootstrap = %d: %s", answer.status, answer.body)
	}
}

func TestLocalSignInBoundsParallelPasswordChecks(t *testing.T) {
	plane := startIdentityPlane(t)
	const attempts = 24
	statuses := make(chan int, attempts)
	start := make(chan struct{})
	var waiting sync.WaitGroup
	for index := 0; index < attempts; index++ {
		waiting.Add(1)
		go func(index int) {
			defer waiting.Done()
			<-start
			answer := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
				map[string]any{"email": "unknown@example.test", "password": "invalid password value"})
			statuses <- answer.status
		}(index)
	}
	close(start)
	waiting.Wait()
	close(statuses)
	limited := 0
	for status := range statuses {
		if status == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("parallel password checks were not bounded")
	}
}

func TestDeploymentOIDCUsesSubjectAndDatabaseMembership(t *testing.T) {
	issuer := newMockIssuer(t)
	plane := startIdentityPlane(t, func(cfg *config.Config) {
		cfg.OIDCIssuer = issuer.url()
		cfg.OIDCClientID = "oc-console"
		cfg.OIDCClientSecret = "test-client-secret"
	})
	bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	connection, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatalf("connect to identity database: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	oidcUser := uuid.New()
	if _, err = connection.Exec(context.Background(), `
		INSERT INTO app_user
			(user_id,issuer,subject,email,display_name)
		VALUES ($1,$2,'operator-1','ada@example.test','Ada Lovelace')`,
		oidcUser, issuer.url()); err != nil {
		t.Fatalf("seed OIDC User: %v", err)
	}
	if _, err = connection.Exec(context.Background(), `
		INSERT INTO organization_membership
			(org_id,user_id,role)
		VALUES ($1,$2,'editor')`, identityOrg, oidcUser); err != nil {
		t.Fatalf("seed OIDC membership: %v", err)
	}
	startURL := "http://" + plane.operator + "/api/v1/auth/oidc/start"
	started := plane.call(t, http.MethodGet, startURL, nil)
	if started.status != http.StatusFound {
		t.Fatalf("OIDC start = %d: %s", started.status, started.body)
	}
	atIssuer := plane.call(t, http.MethodGet, started.location, nil)
	if atIssuer.status != http.StatusFound {
		t.Fatalf("issuer = %d: %s", atIssuer.status, atIssuer.body)
	}
	completed := plane.call(t, http.MethodGet, atIssuer.location, nil)
	if completed.status != http.StatusFound {
		t.Fatalf("OIDC callback = %d: %s", completed.status, completed.body)
	}
	who := readSession(t, plane, sessionCookie(t, completed))
	if who.Organization.Role != "editor" {
		t.Fatalf("OIDC session = %+v", who)
	}
	var flows int
	if err = connection.QueryRow(context.Background(), `SELECT count(*) FROM oidc_sign_in_flow`).Scan(&flows); err != nil || flows != 0 {
		t.Fatalf("completed OIDC flows retained = %d (%v)", flows, err)
	}

	issuer.assert(t, "sub", "not-preprovisioned")
	started = plane.call(t, http.MethodGet, startURL, nil)
	atIssuer = plane.call(t, http.MethodGet, started.location, nil)
	refused := plane.call(t, http.MethodGet, atIssuer.location, nil)
	if refused.status != http.StatusForbidden {
		t.Fatalf("unprovisioned subject = %d: %s", refused.status, refused.body)
	}
}
