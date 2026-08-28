package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

type sessionBody struct {
	Principal struct {
		DisplayName string `json:"displayName"`
	} `json:"principal"`
	Organizations []struct {
		Organization string `json:"organizationId"`
		Role         string `json:"role"`
	} `json:"organizations"`
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

func TestLocalAuthenticationBootstrapsOneAdminAndSignsIn(t *testing.T) {
	plane := startIdentityPlane(t)
	wrongOrganization := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"organization": identityNeighbour,
		"email":        "intruder@example.test", "displayName": "Intruder",
		"password": "correct horse battery staple",
	}, asBootstrap)
	if wrongOrganization.status != http.StatusUnauthorized {
		t.Fatalf("wrong-Organization bootstrap = %d: %s", wrongOrganization.status, wrongOrganization.body)
	}

	created := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"organization": identityOrg,
		"email":        "ada@example.test",
		"displayName":  "Ada Lovelace",
		"password":     "correct horse battery staple",
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
	if who.Principal.DisplayName != "Ada Lovelace" || len(who.Organizations) != 1 ||
		who.Organizations[0].Organization != identityOrg ||
		who.Organizations[0].Role != "admin" {
		t.Fatalf("bootstrap session = %+v", who)
	}

	again := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"organization": identityOrg,
		"email":        "grace@example.test",
		"displayName":  "Grace Hopper",
		"password":     "another correct horse battery staple",
	}, asBootstrap)
	if again.status != http.StatusConflict || !strings.Contains(again.body, "already") {
		t.Fatalf("second bootstrap = %d: %s", again.status, again.body)
	}

	signedIn := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{
			"organization": identityOrg,
			"email":        "ADA@example.test",
			"password":     "correct horse battery staple",
		})
	if signedIn.status != http.StatusOK {
		t.Fatalf("local sign-in = %d: %s", signedIn.status, signedIn.body)
	}
	readSession(t, plane, sessionCookie(t, signedIn))

	refused := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{
			"organization": identityOrg,
			"email":        "ada@example.test",
			"password":     "wrong password",
		})
	if refused.status != http.StatusForbidden || strings.Contains(refused.body, "password") {
		t.Fatalf("wrong password = %d: %s", refused.status, refused.body)
	}
}

func TestAnAuthenticatedUserCanCreateAndSelectAnOrganization(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapURL := "http://" + plane.operator + "/api/v1/auth/local/bootstrap"
	created := plane.call(t, http.MethodPost, bootstrapURL, map[string]any{
		"organization": identityOrg,
		"email":        "admin@example.test", "displayName": "Admin",
		"password": "initial administrator password",
	}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", created.status, created.body)
	}
	admin := sessionCookie(t, created)
	organizationsURL := "http://" + plane.operator + "/api/v1/organizations"

	createdOrganization := plane.call(t, http.MethodPost, organizationsURL,
		map[string]any{"organization": "second-org"}, asSession(admin))
	if createdOrganization.status != http.StatusCreated {
		t.Fatalf("creating an Organization = %d: %s",
			createdOrganization.status, createdOrganization.body)
	}

	listed := plane.call(t, http.MethodGet, organizationsURL, nil, asSession(admin))
	if listed.status != http.StatusOK ||
		!strings.Contains(listed.body, `"id":"org-a"`) ||
		!strings.Contains(listed.body, `"id":"second-org"`) {
		t.Fatalf("organizations = %d: %s", listed.status, listed.body)
	}

	permissions := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/permissions", nil,
		asSession(admin), inOrganization("second-org"))
	if permissions.status != http.StatusOK ||
		!strings.Contains(permissions.body, `"role":"admin"`) ||
		!strings.Contains(permissions.body, `"permissions"`) {
		t.Fatalf("permissions = %d: %s", permissions.status, permissions.body)
	}
}

func TestMemberCreationRequiresAnExplicitIdentityKind(t *testing.T) {
	plane := startIdentityPlane(t)
	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organization": identityOrg,
			"email":        "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, created)
	membersURL := "http://" + plane.operator + "/api/v1/members"
	member := map[string]any{
		"email": "member@example.test", "displayName": "Member",
		"role": "viewer", "password": "member password long enough",
	}

	missingKind := plane.call(t, http.MethodPost, membersURL, member,
		asSession(admin), inOrganization(identityOrg))
	if missingKind.status != http.StatusBadRequest {
		t.Fatalf("member without identityKind = %d: %s", missingKind.status, missingKind.body)
	}

	member["identityKind"] = "oidc"
	member["subject"] = "member-subject"
	withoutOIDC := plane.call(t, http.MethodPost, membersURL, member,
		asSession(admin), inOrganization(identityOrg))
	if withoutOIDC.status != http.StatusConflict {
		t.Fatalf("OIDC member without configured OIDC = %d: %s", withoutOIDC.status, withoutOIDC.body)
	}

	member["identityKind"] = "local"
	local := plane.call(t, http.MethodPost, membersURL, member,
		asSession(admin), inOrganization(identityOrg))
	if local.status != http.StatusCreated {
		t.Fatalf("local member = %d: %s", local.status, local.body)
	}
}

func TestMemberStateCanBeDeactivatedAndRevokesAccess(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrap := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organization": identityOrg,
			"email":        "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrap)
	membersURL := "http://" + plane.operator + "/api/v1/members"
	created := plane.call(t, http.MethodPost, membersURL, map[string]any{
		"identityKind": "local", "email": "member@example.test", "displayName": "Member",
		"role": "viewer", "password": "member password long enough",
	}, asSession(admin), inOrganization(identityOrg))
	var member struct {
		UserID string `json:"userId"`
	}
	decodeInto(t, created.body, &member)
	signedIn := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/sign-in", map[string]any{
			"organization": identityOrg,
			"email":        "member@example.test", "password": "member password long enough",
		})
	memberSession := sessionCookie(t, signedIn)

	inactive := false
	changed := plane.call(t, http.MethodPatch, membersURL+"/"+member.UserID,
		map[string]any{"active": inactive}, asSession(admin), inOrganization(identityOrg))
	if changed.status != http.StatusOK || !strings.Contains(changed.body, `"active":false`) {
		t.Fatalf("deactivating membership = %d: %s", changed.status, changed.body)
	}
	who := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/session", nil, asSession(memberSession))
	if who.status != http.StatusUnauthorized {
		t.Fatalf("deactivated member session remained usable: %d: %s", who.status, who.body)
	}
}

func TestAnAdminCanRevokeOneNamedSession(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrap := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organization": identityOrg,
			"email":        "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrap)
	sessionsURL := "http://" + plane.operator + "/api/v1/sessions"
	list := func(organization string) []struct {
		ID string `json:"id"`
	} {
		answer := plane.call(t, http.MethodGet, sessionsURL, nil,
			asSession(admin), inOrganization(organization))
		if answer.status != http.StatusOK {
			t.Fatalf("listing sessions = %d: %s", answer.status, answer.body)
		}
		var body struct {
			Sessions []struct {
				ID string `json:"id"`
			} `json:"sessions"`
		}
		decodeInto(t, answer.body, &body)
		return body.Sessions
	}
	before := list(identityOrg)

	signedIn := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/sign-in", map[string]any{
			"organization": identityOrg,
			"email":        "admin@example.test", "password": "initial administrator password",
		})
	second := sessionCookie(t, signedIn)
	after := list(identityOrg)
	known := make(map[string]bool, len(before))
	for _, live := range before {
		known[live.ID] = true
	}
	var target string
	for _, live := range after {
		if !known[live.ID] {
			target = live.ID
		}
	}
	if target == "" {
		t.Fatalf("new session is absent: before=%v after=%v", before, after)
	}

	createdOrganization := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/organizations", map[string]any{
			"organization": "other-org",
		}, asSession(admin))
	if createdOrganization.status != http.StatusCreated {
		t.Fatalf("creating second organization = %d: %s",
			createdOrganization.status, createdOrganization.body)
	}
	otherSignIn := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/sign-in", map[string]any{
			"organization": "other-org", "email": "admin@example.test",
			"password": "initial administrator password",
		})
	otherCookie := sessionCookie(t, otherSignIn)
	otherSessions := list("other-org")
	var otherTarget string
	for _, live := range otherSessions {
		if live.ID != "" {
			otherTarget = live.ID
		}
	}
	if otherTarget == "" {
		t.Fatalf("second Organization session is absent: %v", otherSessions)
	}
	crossOrganization := plane.call(t, http.MethodDelete, sessionsURL+"/"+otherTarget, nil,
		asSession(admin), inOrganization(identityOrg))
	if crossOrganization.status != http.StatusNotFound {
		t.Fatalf("cross-Organization revocation = %d: %s",
			crossOrganization.status, crossOrganization.body)
	}
	otherWho := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/session", nil, asSession(otherCookie))
	if otherWho.status != http.StatusOK {
		t.Fatalf("cross-Organization target was revoked: %d: %s", otherWho.status, otherWho.body)
	}

	revoked := plane.call(t, http.MethodDelete, sessionsURL+"/"+target, nil,
		asSession(admin), inOrganization(identityOrg))
	if revoked.status != http.StatusNoContent {
		t.Fatalf("revoking session = %d: %s", revoked.status, revoked.body)
	}
	connection, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatalf("connect to identity database: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	var retained bool
	if err = connection.QueryRow(context.Background(), `
		SELECT revoked_at IS NOT NULL FROM operator_session WHERE session_id = $1`,
		target).Scan(&retained); err != nil || !retained {
		t.Fatalf("administratively revoked session was not retained: retained=%v error=%v",
			retained, err)
	}
	var revocationEvents int
	if err = connection.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_event
		 WHERE target_id = $1 AND action = 'session.revoked'`, target).Scan(&revocationEvents); err != nil || revocationEvents != 1 {
		t.Fatalf("administrative revocation audit count = %d: %v", revocationEvents, err)
	}
	who := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/session", nil, asSession(second))
	if who.status != http.StatusUnauthorized {
		t.Fatalf("revoked session remained usable: %d: %s", who.status, who.body)
	}
}

func TestSessionDescribesTheVerifiedSelectionAndBrowserSecurity(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrap := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organization": identityOrg,
			"email":        "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrap)
	who := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/session", nil,
		asSession(admin), inOrganization(identityOrg))
	if who.status != http.StatusOK {
		t.Fatalf("session = %d: %s", who.status, who.body)
	}
	for _, fact := range []string{
		`"activeOrganization":{"organizationId":"org-a","role":"admin"}`,
		`"authenticationMethod":"local"`,
		`"csrf":{"mode":"origin","requiredForUnsafeMethods":true}`,
	} {
		if !strings.Contains(who.body, fact) {
			t.Errorf("session omits %s: %s", fact, who.body)
		}
	}
}

func TestLocalBootstrapRequiresAnAdminScopedCredential(t *testing.T) {
	plane := startIdentityPlane(t, func(cfg *config.Config) { cfg.OperatorTokenRole = "viewer" })
	answer := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"organization": identityOrg, "email": "viewer@example.test", "displayName": "Viewer",
		"password": "correct horse battery staple",
	}, asBootstrap)
	if answer.status != http.StatusUnauthorized {
		t.Fatalf("viewer bootstrap = %d: %s", answer.status, answer.body)
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
				map[string]any{"organization": identityOrg, "email": "unknown@example.test", "password": "invalid password value"})
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
		cfg.AuthenticationMode = "local+oidc"
		cfg.OIDCIssuer = issuer.url()
		cfg.OIDCClientID = "oc-console"
		cfg.OIDCClientSecret = "test-client-secret"
	})
	boot := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"organization": identityOrg, "email": "admin@example.test",
			"displayName": "Admin", "password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, boot)
	member := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/members", map[string]any{
			"identityKind": "oidc", "subject": "operator-1", "email": "ada@example.test",
			"displayName": "Ada Lovelace", "role": "editor",
		}, asSession(admin), inOrganization(identityOrg))
	if member.status != http.StatusCreated {
		t.Fatalf("OIDC member = %d: %s", member.status, member.body)
	}
	startURL := "http://" + plane.operator + "/api/v1/auth/oidc/start?organization=" + identityOrg
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
	if len(who.Organizations) != 1 || who.Organizations[0].Role != "editor" {
		t.Fatalf("OIDC session = %+v", who)
	}
	connection, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatalf("connect to identity database: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	var flows int
	if err = connection.QueryRow(context.Background(), `SELECT count(*) FROM deployment_sign_in_flow`).Scan(&flows); err != nil || flows != 0 {
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

func TestLocalMembersAreAdminManagedAndPasswordResetRevokesSessions(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap",
		map[string]any{
			"organization": identityOrg, "email": "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)

	closed := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/members", map[string]any{
		"identityKind": "local", "email": "grace@example.test", "displayName": "Grace Hopper",
		"role": "editor", "password": "first member password",
	})
	if closed.status != http.StatusUnauthorized {
		t.Fatalf("public registration = %d: %s", closed.status, closed.body)
	}

	created := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/members", map[string]any{
		"identityKind": "local", "email": "grace@example.test", "displayName": "Grace Hopper",
		"role": "editor", "password": "first member password",
	}, asSession(admin))
	if created.status != http.StatusCreated {
		t.Fatalf("create local member = %d: %s", created.status, created.body)
	}
	var member struct {
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal([]byte(created.body), &member); err != nil || member.UserID == "" {
		t.Fatalf("created member = %s (%v)", created.body, err)
	}
	connection, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatalf("connect to identity database: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	if _, err = connection.Exec(context.Background(), `INSERT INTO organization (org_id,created_by) VALUES ($1,'test')`,
		identityNeighbour); err != nil {
		t.Fatalf("create neighbor organization: %v", err)
	}
	if _, err = connection.Exec(context.Background(), `INSERT INTO organization_membership
		(membership_id,org_id,user_id,role,source,granted_by) VALUES ($1,$2,$3,'viewer',1,'test')`,
		uuid.New(), identityNeighbour, member.UserID); err != nil {
		t.Fatalf("grant neighbor membership: %v", err)
	}

	grace := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{"organization": identityOrg, "email": "grace@example.test", "password": "first member password"})
	graceCookie := sessionCookie(t, grace)
	neighbor := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{"organization": identityNeighbour, "email": "grace@example.test", "password": "first member password"})
	neighborCookie := sessionCookie(t, neighbor)

	csrf := plane.call(t, http.MethodPut,
		plane.base(identityOrg)+"/members/"+member.UserID+"/password",
		map[string]any{"password": "replacement member password"},
		asSession(admin), withoutOrigin)
	if csrf.status != http.StatusForbidden {
		t.Fatalf("password reset without origin = %d: %s", csrf.status, csrf.body)
	}

	reset := plane.call(t, http.MethodPut,
		plane.base(identityOrg)+"/members/"+member.UserID+"/password",
		map[string]any{"password": "replacement member password"}, asSession(admin))
	if reset.status != http.StatusNoContent {
		t.Fatalf("password reset = %d: %s", reset.status, reset.body)
	}

	revoked := plane.call(t, http.MethodGet, "http://"+plane.operator+"/api/v1/session",
		nil, asSession(graceCookie))
	if revoked.status != http.StatusUnauthorized {
		t.Fatalf("session after reset = %d: %s", revoked.status, revoked.body)
	}
	neighborRevoked := plane.call(t, http.MethodGet, "http://"+plane.operator+"/api/v1/session",
		nil, asSession(neighborCookie))
	if neighborRevoked.status != http.StatusUnauthorized {
		t.Fatalf("neighbor session after reset = %d: %s", neighborRevoked.status, neighborRevoked.body)
	}
	oldPassword := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{"organization": identityOrg, "email": "grace@example.test", "password": "first member password"})
	if oldPassword.status != http.StatusForbidden {
		t.Fatalf("old password = %d: %s", oldPassword.status, oldPassword.body)
	}
	newPassword := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
		map[string]any{"organization": identityOrg, "email": "grace@example.test", "password": "replacement member password"})
	if newPassword.status != http.StatusOK {
		t.Fatalf("new password = %d: %s", newPassword.status, newPassword.body)
	}
}
