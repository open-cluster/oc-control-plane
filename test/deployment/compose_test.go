package deployment_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestComposePublishesOnlyTheControlPlaneAPI(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("OC_REQUIRE_CONTAINERS") == "1" {
			t.Fatal(err)
		}
		t.Skip("Docker CLI is unavailable")
	}
	command := exec.Command("docker", "compose", "-f", "deploy/compose/compose.yaml", "config", "--no-interpolate", "--format", "json")
	command.Dir = "../.."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("resolving Compose: %v: %s", err, output)
	}
	var configuration struct {
		Services map[string]struct {
			Ports []json.RawMessage `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(output, &configuration); err != nil {
		t.Fatal(err)
	}
	for name := range configuration.Services {
		if name != "postgres" && name != "control-plane" && name != "relay-tls" {
			t.Errorf("unexpected Compose service %q", name)
		}
	}
	api, found := configuration.Services["control-plane"]
	if !found || len(api.Ports) != 1 {
		t.Fatalf("control plane must publish one API port: %+v", api.Ports)
	}
	var port struct {
		HostIP    string `json:"host_ip"`
		Target    int    `json:"target"`
		Published string `json:"published"`
	}
	if err := json.Unmarshal(api.Ports[0], &port); err != nil {
		t.Fatal(err)
	}
	if port.HostIP != "127.0.0.1" || port.Target != 8080 || port.Published != "8080" {
		t.Errorf("API port mapping = %+v, want 127.0.0.1:8080:8080", port)
	}
	if len(configuration.Services["postgres"].Ports) != 0 {
		t.Error("PostgreSQL must not publish a host port")
	}
}
