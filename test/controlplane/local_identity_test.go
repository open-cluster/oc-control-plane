package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
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

func bootstrapIdentityAdmin(
	t *testing.T, plane *identityPlane, email, displayName, password string,
) string {
	t.Helper()
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"email": email, "displayName": displayName, "password": password,
		}, asBootstrap)
	if bootstrapped.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", bootstrapped.status, bootstrapped.body)
	}
	cookie := sessionCookie(t, bootstrapped)
	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/organizations", map[string]any{
			"displayName": "Operations", "requestedSlug": identityOrg,
		}, asSession(cookie))
	if created.status != http.StatusCreated {
		t.Fatalf("creating first Organization = %d: %s", created.status, created.body)
	}
	return cookie
}

func TestLocalBootstrapCreatesUserWithoutOrganization(t *testing.T) {
	plane := startIdentityPlane(t)

	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"email":       "ada@example.test",
			"displayName": "Ada Lovelace",
			"password":    "correct horse battery staple",
		}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", created.status, created.body)
	}

	who := readSession(t, plane, sessionCookie(t, created))
	if who.Principal.DisplayName != "Ada Lovelace" || len(who.Organizations) != 0 {
		t.Fatalf("bootstrap session = %+v", who)
	}
}

func TestBootstrappedUserCreatesFirstOrganizationFromDisplayName(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"email": "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	if bootstrapped.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", bootstrapped.status, bootstrapped.body)
	}

	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/organizations",
		map[string]any{"displayName": "Platform Team"},
		asSession(sessionCookie(t, bootstrapped)))
	if created.status != http.StatusCreated {
		t.Fatalf("creating first Organization = %d: %s", created.status, created.body)
	}
	var body struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		Membership  struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"membership"`
	}
	decodeAnswer(t, created, &body)
	if !regexp.MustCompile(`^platform-team-[0-9a-f]{8}$`).MatchString(body.ID) ||
		body.DisplayName != "Platform Team" || body.Membership.Role != "admin" {
		t.Fatalf("created Organization = %+v", body)
	}
	if _, err := uuid.Parse(body.Membership.ID); err != nil {
		t.Fatalf("membership id = %q: %v", body.Membership.ID, err)
	}
}

func TestAdminCreatesLocalUserWithoutIdentityProviderChoice(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"email": "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)
	organization := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/organizations", map[string]any{
			"displayName": "Operations", "requestedSlug": identityOrg,
		}, asSession(admin))
	if organization.status != http.StatusCreated {
		t.Fatalf("creating first Organization = %d: %s", organization.status, organization.body)
	}

	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/local-users", map[string]any{
			"email": "member@example.test", "displayName": "Member",
			"role": "viewer", "password": "member password long enough",
		}, asSession(admin), inOrganization(identityOrg))
	if created.status != http.StatusCreated {
		t.Fatalf("creating local User = %d: %s", created.status, created.body)
	}
	var member struct {
		ID     string `json:"id"`
		UserID string `json:"userId"`
		Role   string `json:"role"`
	}
	decodeAnswer(t, created, &member)
	if _, err := uuid.Parse(member.ID); err != nil || member.UserID == "" || member.Role != "viewer" {
		t.Fatalf("created local User membership = %+v, id error = %v", member, err)
	}
}

func TestMembershipStateIsChangedByMembershipID(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"email": "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)
	organization := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/organizations", map[string]any{
			"displayName": "Operations", "requestedSlug": identityOrg,
		}, asSession(admin))
	if organization.status != http.StatusCreated {
		t.Fatalf("creating first Organization = %d: %s", organization.status, organization.body)
	}
	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/local-users", map[string]any{
			"email": "member@example.test", "role": "viewer",
			"password": "member password long enough",
		}, asSession(admin), inOrganization(identityOrg))
	var member struct {
		ID string `json:"id"`
	}
	decodeAnswer(t, created, &member)

	changed := plane.call(t, http.MethodPatch,
		"http://"+plane.operator+"/api/v1/members/"+member.ID,
		map[string]any{"role": "editor"}, asSession(admin), inOrganization(identityOrg))
	if changed.status != http.StatusOK || !strings.Contains(changed.body, `"role":"editor"`) {
		t.Fatalf("changing membership = %d: %s", changed.status, changed.body)
	}
}

func TestSessionListsOrganizationMetadataAndMembershipID(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
			"email": "admin@example.test", "password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)
	created := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/organizations", map[string]any{
			"displayName": "Operations", "requestedSlug": identityOrg,
		}, asSession(admin))
	if created.status != http.StatusCreated {
		t.Fatalf("creating first Organization = %d: %s", created.status, created.body)
	}
	var organization struct {
		Membership struct {
			ID string `json:"id"`
		} `json:"membership"`
	}
	decodeAnswer(t, created, &organization)

	listed := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/session", nil, asSession(admin))
	if listed.status != http.StatusOK ||
		!strings.Contains(listed.body, `"organizationId":"org-a"`) ||
		!strings.Contains(listed.body, `"displayName":"Operations"`) ||
		!strings.Contains(listed.body, `"id":"`+organization.Membership.ID+`"`) {
		t.Fatalf("session Organizations = %d: %s", listed.status, listed.body)
	}
}

func TestLocalAuthenticationBootstrapsOneAdminAndSignsIn(t *testing.T) {
	plane := startIdentityPlane(t)
	created := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"email":       "ada@example.test",
		"displayName": "Ada Lovelace",
		"password":    "correct horse battery staple",
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
	if who.Principal.DisplayName != "Ada Lovelace" || len(who.Organizations) != 0 {
		t.Fatalf("bootstrap session = %+v", who)
	}
	organization := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/organizations", map[string]any{
			"displayName": "Operations", "requestedSlug": identityOrg,
		}, asSession(bootstrapCookie))
	if organization.status != http.StatusCreated {
		t.Fatalf("creating first Organization = %d: %s", organization.status, organization.body)
	}

	again := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/bootstrap", map[string]any{
		"email":       "grace@example.test",
		"displayName": "Grace Hopper",
		"password":    "another correct horse battery staple",
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
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	organizationsURL := "http://" + plane.operator + "/api/v1/organizations"

	createdOrganization := plane.call(t, http.MethodPost, organizationsURL,
		map[string]any{"displayName": "Second", "requestedSlug": "second-org"}, asSession(admin))
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

func TestMemberStateCanBeDeactivatedAndRevokesAccess(t *testing.T) {
	plane := startIdentityPlane(t)
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	membersURL := "http://" + plane.operator + "/api/v1/members"
	created := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/local-users", map[string]any{
		"email": "member@example.test", "displayName": "Member",
		"role": "viewer", "password": "member password long enough",
	}, asSession(admin), inOrganization(identityOrg))
	var member struct {
		ID string `json:"id"`
	}
	decodeInto(t, created.body, &member)
	signedIn := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/api/v1/auth/local/sign-in", map[string]any{
			"organization": identityOrg,
			"email":        "member@example.test", "password": "member password long enough",
		})
	memberSession := sessionCookie(t, signedIn)

	inactive := false
	changed := plane.call(t, http.MethodPatch, membersURL+"/"+member.ID,
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
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
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
			"displayName": "Other", "requestedSlug": "other-org",
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
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	who := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/api/v1/session", nil,
		asSession(admin), inOrganization(identityOrg))
	if who.status != http.StatusOK {
		t.Fatalf("session = %d: %s", who.status, who.body)
	}
	for _, fact := range []string{
		`"activeOrganization":{"id":"`,
		`"organizationId":"org-a","displayName":"Operations","role":"admin"`,
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
		"email": "viewer@example.test", "displayName": "Viewer",
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
			(user_id,issuer,subject,email,email_verified,display_name)
		VALUES ($1,$2,'operator-1','ada@example.test',TRUE,'Ada Lovelace')`,
		oidcUser, issuer.url()); err != nil {
		t.Fatalf("seed OIDC User: %v", err)
	}
	if _, err = connection.Exec(context.Background(), `
		INSERT INTO organization_membership
			(membership_id,org_id,user_id,role,source,granted_by)
		VALUES ($1,$2,$3,'editor',1,'test')`, uuid.New(), identityOrg, oidcUser); err != nil {
		t.Fatalf("seed OIDC membership: %v", err)
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
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	localUsersURL := "http://" + plane.operator + "/api/v1/local-users"

	closed := plane.call(t, http.MethodPost, localUsersURL, map[string]any{
		"email": "grace@example.test", "displayName": "Grace Hopper",
		"role": "editor", "password": "first member password",
	}, inOrganization(identityOrg))
	if closed.status != http.StatusUnauthorized {
		t.Fatalf("public registration = %d: %s", closed.status, closed.body)
	}

	created := plane.call(t, http.MethodPost, localUsersURL, map[string]any{
		"email": "grace@example.test", "displayName": "Grace Hopper",
		"role": "editor", "password": "first member password",
	}, asSession(admin), inOrganization(identityOrg))
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
	if _, err = connection.Exec(context.Background(), `
		INSERT INTO organization (org_id,display_name,created_by) VALUES ($1,'Neighbour','test')`,
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
		localUsersURL+"/"+member.UserID+"/password",
		map[string]any{"password": "replacement member password"},
		asSession(admin), inOrganization(identityOrg), withoutOrigin)
	if csrf.status != http.StatusForbidden {
		t.Fatalf("password reset without origin = %d: %s", csrf.status, csrf.body)
	}

	reset := plane.call(t, http.MethodPut,
		localUsersURL+"/"+member.UserID+"/password",
		map[string]any{"password": "replacement member password"},
		asSession(admin), inOrganization(identityOrg))
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
