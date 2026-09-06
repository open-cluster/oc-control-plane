package controlplane

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestLocalUserChangesPasswordAndRevokesAllSessions(t *testing.T) {
	plane := startIdentityPlane(t)
	base := "http://" + plane.operator + "/api/v1"
	oldPassword := "initial administrator password"
	newPassword := "replacement administrator password"
	first := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin", oldPassword)
	login := func(password string) answer {
		return plane.call(t, http.MethodPost, base+"/auth/local/sign-in", map[string]any{
			"organization": identityOrg, "email": "admin@example.test", "password": password,
		})
	}
	other := plane.call(t, http.MethodPost, base+"/organizations", map[string]any{
		"displayName": "Second", "requestedSlug": "second-org",
	}, asSession(first))
	if other.status != http.StatusCreated {
		t.Fatalf("second Organization = %d: %s", other.status, other.body)
	}
	otherLogin := plane.call(t, http.MethodPost, base+"/auth/local/sign-in", map[string]any{
		"organization": "second-org", "email": "admin@example.test", "password": oldPassword,
	})
	second := sessionCookie(t, otherLogin)
	for _, invalid := range []struct {
		body   map[string]any
		status int
	}{
		{map[string]any{"currentPassword": "incorrect", "newPassword": newPassword}, http.StatusForbidden},
		{map[string]any{"currentPassword": oldPassword, "newPassword": "short"}, http.StatusBadRequest},
		{map[string]any{"currentPassword": oldPassword, "newPassword": newPassword, "userId": "someone-else"}, http.StatusBadRequest},
	} {
		refused := plane.call(t, http.MethodPut, base+"/auth/local/password", invalid.body, asSession(first))
		if refused.status != invalid.status {
			t.Fatalf("invalid change = %d: %s", refused.status, refused.body)
		}
		readSession(t, plane, first)
	}
	changed := plane.call(t, http.MethodPut, base+"/auth/local/password", map[string]any{
		"currentPassword": oldPassword, "newPassword": newPassword,
	}, asSession(first))
	if changed.status != http.StatusNoContent {
		t.Fatalf("password change = %d: %s", changed.status, changed.body)
	}
	assertSessionCookieCleared(t, changed)
	for _, cookie := range []string{first, second} {
		who := plane.call(t, http.MethodGet, base+"/session", nil, asSession(cookie))
		if who.status != http.StatusUnauthorized {
			t.Fatalf("old session = %d: %s", who.status, who.body)
		}
	}
	if response := login(oldPassword); response.status != http.StatusForbidden {
		t.Fatalf("old password = %d", response.status)
	}
	if response := login(newPassword); response.status != http.StatusOK {
		t.Fatalf("new password = %d: %s", response.status, response.body)
	}
}

func TestRecoveryCLIAfterBootstrapRetirement(t *testing.T) {
	plane := startIdentityPlane(t)
	cookie := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin", "initial administrator password")
	response := plane.call(t, http.MethodGet, "http://"+plane.operator+"/api/v1/session", nil, asSession(cookie))
	var who struct {
		Principal struct {
			ID string `json:"id"`
		} `json:"principal"`
	}
	decodeAnswer(t, response, &who)
	if who.Principal.ID == "" {
		t.Fatalf("missing User ID: %s", response.body)
	}
	plane.shutdown()
	directory := t.TempDir()
	writeSecret := func(name, value string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "run", "../../cmd/controlplane", "recover-local-password", "--user", who.Principal.ID)
	command.Env = append(os.Environ(),
		config.EnvConfigFile+"=", config.EnvOperatorTokenFile+"=",
		config.EnvDatabaseDSNFile+"="+writeSecret("dsn", plane.dsn),
		config.EnvSealingKeyFile+"=", config.EnvModelProvider+"=anthropic",
		config.EnvModelName+"=", config.EnvModelKeyFile+"=")
	const recoveredPassword = "recovered administrator password"
	command.Stdin = strings.NewReader(recoveredPassword + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recovery CLI failed: %v: %s", err, output)
	}
	if strings.Contains(string(output), recoveredPassword) {
		t.Fatal("recovery CLI exposed the password")
	}
	restarted := startIdentityPlane(t, func(cfg *config.Config) {
		cfg.DatabaseDSN = plane.dsn
		cfg.OperatorTokenDigest = nil
	})
	base := "http://" + restarted.operator + "/api/v1"
	stale := restarted.call(t, http.MethodGet, base+"/session", nil, asSession(cookie))
	if stale.status != http.StatusUnauthorized {
		t.Fatalf("recovery retained old session: %d", stale.status)
	}
	login := restarted.call(t, http.MethodPost, base+"/auth/local/sign-in", map[string]any{
		"organization": identityOrg, "email": "admin@example.test", "password": recoveredPassword,
	})
	if login.status != http.StatusOK {
		t.Fatalf("sign-in after recovery = %d: %s", login.status, login.body)
	}
	bootstrap := restarted.call(t, http.MethodPost, base+"/auth/local/bootstrap", map[string]any{
		"email": "another@example.test", "password": recoveredPassword,
	}, asBootstrap)
	if bootstrap.status != http.StatusUnauthorized {
		t.Fatalf("retired bootstrap = %d: %s", bootstrap.status, bootstrap.body)
	}
}
