package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-cluster/oc-control-plane/internal/app"
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
	answer := plane.call(t, http.MethodGet, "http://"+plane.operator+"/operator/v1/session", nil, asSession(cookie))
	if answer.status != http.StatusOK {
		t.Fatalf("session = %d: %s", answer.status, answer.body)
	}
	var body sessionBody
	decodeAnswer(t, answer, &body)
	return body
}

func TestLocalAuthenticationBootstrapsOneAdminAndSignsIn(t *testing.T) {
	plane := startIdentityPlane(t)
	wrongOrganization := plane.call(t, http.MethodPost, plane.base(identityNeighbour)+"/bootstrap", map[string]any{
		"email": "intruder@example.test", "displayName": "Intruder",
		"password": "correct horse battery staple",
	}, asBootstrap)
	if wrongOrganization.status != http.StatusUnauthorized {
		t.Fatalf("wrong-Organization bootstrap = %d: %s", wrongOrganization.status, wrongOrganization.body)
	}

	created := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/bootstrap", map[string]any{
		"email":       "ada@example.test",
		"displayName": "Ada Lovelace",
		"password":    "correct horse battery staple",
	}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("bootstrap = %d: %s", created.status, created.body)
	}
	bootstrapCookie := sessionCookie(t, created)
	retiredBootstrap := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/members", nil, asBootstrap)
	if retiredBootstrap.status != http.StatusUnauthorized {
		t.Fatalf("bootstrap token after first Admin = %d: %s", retiredBootstrap.status, retiredBootstrap.body)
	}

	who := readSession(t, plane, bootstrapCookie)
	if who.Principal.DisplayName != "Ada Lovelace" || len(who.Organizations) != 1 ||
		who.Organizations[0].Organization != identityOrg ||
		who.Organizations[0].Role != "admin" {
		t.Fatalf("bootstrap session = %+v", who)
	}

	again := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/bootstrap", map[string]any{
		"email":       "grace@example.test",
		"displayName": "Grace Hopper",
		"password":    "another correct horse battery staple",
	}, asBootstrap)
	if again.status != http.StatusConflict || !strings.Contains(again.body, "already") {
		t.Fatalf("second bootstrap = %d: %s", again.status, again.body)
	}

	signedIn := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/sign-in/local",
		map[string]any{
			"email":    "ADA@example.test",
			"password": "correct horse battery staple",
		})
	if signedIn.status != http.StatusOK {
		t.Fatalf("local sign-in = %d: %s", signedIn.status, signedIn.body)
	}
	readSession(t, plane, sessionCookie(t, signedIn))

	refused := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/sign-in/local",
		map[string]any{
			"email":    "ada@example.test",
			"password": "wrong password",
		})
	if refused.status != http.StatusForbidden || strings.Contains(refused.body, "password") {
		t.Fatalf("wrong password = %d: %s", refused.status, refused.body)
	}
}

func TestLocalBootstrapRequiresAnAdminScopedCredential(t *testing.T) {
	plane := startIdentityPlane(t, func(cfg *config.Config) { cfg.OperatorTokenRole = "viewer" })
	answer := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/bootstrap", map[string]any{
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
			answer := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/sign-in/local",
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
		cfg.AuthenticationMode = "local+oidc"
		cfg.OIDCIssuer = issuer.url()
		cfg.OIDCClientID = "oc-console"
		cfg.OIDCClientSecret = "test-client-secret"
	})
	boot := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/bootstrap", map[string]any{"email": "admin@example.test", "displayName": "Admin", "password": "initial administrator password"}, asBootstrap)
	admin := sessionCookie(t, boot)
	member := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/members/oidc", map[string]any{"subject": "operator-1", "email": "ada@example.test", "displayName": "Ada Lovelace", "role": "editor"}, asSession(admin))
	if member.status != http.StatusCreated {
		t.Fatalf("OIDC member = %d: %s", member.status, member.body)
	}
	started := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/sign-in/oidc", nil)
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
	started = plane.call(t, http.MethodGet, plane.base(identityOrg)+"/sign-in/oidc", nil)
	atIssuer = plane.call(t, http.MethodGet, started.location, nil)
	refused := plane.call(t, http.MethodGet, atIssuer.location, nil)
	if refused.status != http.StatusForbidden {
		t.Fatalf("unprovisioned subject = %d: %s", refused.status, refused.body)
	}
}

func TestStartupRefusesActiveLegacyIdentityConfiguration(t *testing.T) {
	plane := startIdentityPlane(t)
	connection, err := pgx.Connect(context.Background(), plane.dsn)
	if err != nil {
		t.Fatalf("connect to identity database: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	groupID := uuid.New()
	_, err = connection.Exec(context.Background(), `INSERT INTO scim_group
		(group_id,org_id,display_name,role) VALUES ($1,$2,'retained history',NULL)`, groupID, identityOrg)
	if err != nil {
		t.Fatalf("seed inert SCIM history: %v", err)
	}

	address := freeAddress(t)
	cfg := config.Config{
		HTTPAddress: address, OperatorAddress: address, DatabaseDSN: plane.dsn,
		ShutdownTimeout: time.Second, ServiceName: "legacy-refusal-test",
		SealingKey: make([]byte, 32), OperatorPublicURL: "http://" + address,
		OperatorConsoleURL: identityConsole, OperatorAllowedOrigins: []string{identityConsole},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	listened := make(chan struct{})
	exited := make(chan error, 1)
	go func() {
		exited <- app.Run(ctx, cfg, io.Discard, app.Options{OnListen: func(net.Addr) {
			close(listened)
			cancel()
		}})
	}()
	select {
	case <-listened:
	case err = <-exited:
		t.Fatalf("inert SCIM history refused startup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("inert SCIM history did not reach the listener")
	}
	if err = <-exited; err != nil {
		t.Fatalf("stop after inert SCIM startup: %v", err)
	}

	if _, err = connection.Exec(context.Background(), `UPDATE scim_group SET role='viewer' WHERE group_id=$1`, groupID); err != nil {
		t.Fatalf("activate SCIM mapping: %v", err)
	}
	err = app.Run(context.Background(), cfg, io.Discard, app.Options{})
	if err == nil || !strings.Contains(err.Error(), "active legacy identity configuration") ||
		!strings.Contains(err.Error(), "539b45e") {
		t.Fatalf("legacy startup refusal = %v", err)
	}

	cfg.LegacyIdentityMigrationComplete = true
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	listened = make(chan struct{})
	exited = make(chan error, 1)
	go func() {
		exited <- app.Run(ctx, cfg, io.Discard, app.Options{OnListen: func(net.Addr) {
			close(listened)
			cancel()
		}})
	}()
	select {
	case <-listened:
	case err = <-exited:
		t.Fatalf("acknowledged legacy migration refused startup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("acknowledged legacy migration did not reach the listener")
	}
	if err = <-exited; err != nil {
		t.Fatalf("stop after acknowledged migration startup: %v", err)
	}
}

func TestLocalMembersAreAdminManagedAndPasswordResetRevokesSessions(t *testing.T) {
	plane := startIdentityPlane(t)
	bootstrapped := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/bootstrap",
		map[string]any{
			"email": "admin@example.test", "displayName": "Admin",
			"password": "initial administrator password",
		}, asBootstrap)
	admin := sessionCookie(t, bootstrapped)

	closed := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/members", map[string]any{
		"email": "grace@example.test", "displayName": "Grace Hopper",
		"role": "editor", "password": "first member password",
	})
	if closed.status != http.StatusUnauthorized {
		t.Fatalf("public registration = %d: %s", closed.status, closed.body)
	}

	created := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/members", map[string]any{
		"email": "grace@example.test", "displayName": "Grace Hopper",
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
	if _, err = connection.Exec(context.Background(), `INSERT INTO organization_membership
		(membership_id,org_id,user_id,role,source,granted_by) VALUES ($1,$2,$3,'viewer',1,'test')`,
		uuid.New(), identityNeighbour, member.UserID); err != nil {
		t.Fatalf("grant neighbor membership: %v", err)
	}

	grace := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/sign-in/local",
		map[string]any{"email": "grace@example.test", "password": "first member password"})
	graceCookie := sessionCookie(t, grace)
	neighbor := plane.call(t, http.MethodPost, plane.base(identityNeighbour)+"/sign-in/local",
		map[string]any{"email": "grace@example.test", "password": "first member password"})
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

	revoked := plane.call(t, http.MethodGet, "http://"+plane.operator+"/operator/v1/session",
		nil, asSession(graceCookie))
	if revoked.status != http.StatusUnauthorized {
		t.Fatalf("session after reset = %d: %s", revoked.status, revoked.body)
	}
	neighborRevoked := plane.call(t, http.MethodGet, "http://"+plane.operator+"/operator/v1/session",
		nil, asSession(neighborCookie))
	if neighborRevoked.status != http.StatusUnauthorized {
		t.Fatalf("neighbor session after reset = %d: %s", neighborRevoked.status, neighborRevoked.body)
	}
	oldPassword := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/sign-in/local",
		map[string]any{"email": "grace@example.test", "password": "first member password"})
	if oldPassword.status != http.StatusForbidden {
		t.Fatalf("old password = %d: %s", oldPassword.status, oldPassword.body)
	}
	newPassword := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/sign-in/local",
		map[string]any{"email": "grace@example.test", "password": "replacement member password"})
	if newPassword.status != http.StatusOK {
		t.Fatalf("new password = %d: %s", newPassword.status, newPassword.body)
	}
}
