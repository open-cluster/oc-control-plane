package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

const completionProcessConfig = "OC_TEST_COMPLETION_CONFIG_FILE"

func TestCompletionProcessHelper(t *testing.T) {
	path := os.Getenv(completionProcessConfig)
	if path == "" {
		return
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(contents, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(context.Background(), cfg, os.Stderr, app.Options{Model: concludingModel{}}); err != nil {
		t.Fatal(err)
	}
}

func startCompletionProcess(t *testing.T, cfg config.Config) (string, func()) {
	t.Helper()
	cfg.HTTPAddress = freeAddress(t)
	cfg.OperatorPublicURL = "http://" + cfg.HTTPAddress
	cfg.OperatorTokenDigest = nil
	cfg.ModelProvider, cfg.ModelName, cfg.ModelKey = "zai", "glm-4.7", "scripted-model-key"
	contents, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "process.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestCompletionProcessHelper$")
	command.Env = append(os.Environ(), completionProcessConfig+"="+path)
	logs := &syncBuffer{}
	command.Stdout, command.Stderr = logs, logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	kill := func() { once.Do(func() { _ = command.Process.Kill(); _ = command.Wait() }) }
	t.Cleanup(kill)
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(cfg.OperatorPublicURL + "/healthz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return cfg.HTTPAddress, kill
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("completion process did not listen: %s", logs.String())
	return "", kill
}

func TestCompletionSurvivesProcessInterruption(t *testing.T) {
	var cfg config.Config
	address := freeAddress(t)
	running := startControlPlaneRunning(t, func(value *config.Config) {
		value.HTTPAddress = address
		digest := sha256.Sum256([]byte(surfaceToken))
		value.OperatorTokenDigest = digest[:]
		cfg = *value
	}, app.Options{})
	plane := &integrationPlane{controlPlane: running, operator: address, intake: address}
	plane.shutdown()
	if plane.exitErr != nil {
		t.Fatal(plane.exitErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := pgx.Connect(ctx, cfg.DatabaseDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(context.Background()) }()
	_, err = database.Exec(ctx, `
		CREATE FUNCTION pause_completion() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.type = 6 THEN PERFORM pg_advisory_xact_lock(4867); END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER pause_completion BEFORE INSERT ON investigation_event
		FOR EACH ROW EXECUTE FUNCTION pause_completion();
		SELECT pg_advisory_lock(4867)`)
	if err != nil {
		t.Fatal(err)
	}
	var kill func()
	plane.operator, kill = startCompletionProcess(t, cfg)
	_, interrupted := plane.openConversation(t, "interrupted completion", "investigate checkout")
	paused := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if err := database.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event = 'advisory')`).Scan(&paused); err != nil {
			t.Fatal(err)
		}
		if paused {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !paused {
		t.Fatal("completion never reached the paused terminal transaction")
	}
	kill()
	if _, err := database.Exec(ctx, `SELECT pg_advisory_unlock(4867);
		DROP TRIGGER pause_completion ON investigation_event; DROP FUNCTION pause_completion()`); err != nil {
		t.Fatal(err)
	}
	var status int
	var noEnding, noResult bool
	if err := database.QueryRow(ctx, `SELECT status, concluded_at IS NULL, conclusion = '{}'::jsonb
		FROM investigation WHERE investigation_id = $1 AND org_id = $2`, interrupted, surfaceOrg).
		Scan(&status, &noEnding, &noResult); err != nil {
		t.Fatal(err)
	}
	if status != 1 || !noEnding || !noResult {
		t.Fatal("interrupted transaction left a partial outcome")
	}
	if _, err := database.Exec(ctx, `UPDATE investigation SET lease_expires_at = clock_timestamp() - interval '1 second'
		WHERE investigation_id = $1 AND org_id = $2`, interrupted, surfaceOrg); err != nil {
		t.Fatal(err)
	}
	plane.operator, kill = startCompletionProcess(t, cfg)
	response := openEventStream(t, plane, interrupted, "")
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("waiting for the recovery sweep: %v", err)
	}
	assertOneEnding(t, plane, interrupted, "failed")
	_, completed := plane.openConversation(t, "committed completion", "investigate checkout")
	before := plane.awaitInvestigation(t, completed)
	kill()
	plane.operator, _ = startCompletionProcess(t, cfg)
	code, after := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+completed, nil)
	if code != http.StatusOK || before != after {
		t.Fatalf("restart changed the committed outcome: status=%d, before=%s, after=%s", code, before, after)
	}
	assertOneEnding(t, plane, completed, "concluded")
}

func assertOneEnding(t *testing.T, plane *integrationPlane, turn, ending string) {
	t.Helper()
	status, body := plane.call(t, http.MethodGet, plane.base(surfaceOrg)+"/investigations/"+turn+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("replay = %d: %s", status, body)
	}
	endings := 0
	for _, event := range assertSerializedEvents(t, body) {
		kind := event["type"]
		if kind == "concluded" || kind == "failed" || kind == "cancelled" {
			endings++
			if kind != ending {
				t.Errorf("ending=%v, want %s", kind, ending)
			}
		}
	}
	if endings != 1 {
		t.Fatalf("terminal events=%d, want one", endings)
	}
}
