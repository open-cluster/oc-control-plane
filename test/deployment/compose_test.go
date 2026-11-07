package deployment_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
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
			Ports       []json.RawMessage `json:"ports"`
			Environment map[string]string `json:"environment"`
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
	if port.HostIP != "127.0.0.1" || port.Target != 8081 || port.Published != "8081" {
		t.Errorf("API port mapping = %+v, want 127.0.0.1:8081:8081", port)
	}
	if api.Environment["OC_SERVER_ADDRESS"] != ":8081" ||
		!strings.Contains(api.Environment["OC_PUBLIC_URL"], "http://localhost:8081") {
		t.Fatalf("backend listener and public defaults disagree with the port mapping: %+v", api.Environment)
	}
	if len(configuration.Services["postgres"].Ports) != 0 {
		t.Error("PostgreSQL must not publish a host port")
	}
	if api.Environment["OC_SETUP_ENABLED"] != "false" || strings.Contains(string(output), "BOOTSTRAP_TOKEN") {
		t.Fatal("normal Compose deployment must disable signup and require no administrator token")
	}
	command = exec.Command("docker", "compose", "-f", "deploy/compose/compose.yaml", "-f", "deploy/compose/setup.yaml", "config", "--no-interpolate", "--format", "json")
	command.Dir = "../.."
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("resolving private setup override: %v: %s", err, output)
	}
	var setup struct {
		Services map[string]struct {
			Environment json.RawMessage `json:"environment"`
		} `json:"services"`
	}
	if err = json.Unmarshal(output, &setup); err != nil {
		t.Fatal(err)
	}
	// Compose versions serialize merged environments as either a map or a list.
	var environment map[string]string
	if err = json.Unmarshal(setup.Services["control-plane"].Environment, &environment); err != nil {
		var entries []string
		if err = json.Unmarshal(setup.Services["control-plane"].Environment, &entries); err != nil {
			t.Fatal(err)
		}
		environment = make(map[string]string)
		for _, entry := range entries {
			key, value, _ := strings.Cut(entry, "=")
			environment[key] = value
		}
	}
	if environment["OC_SETUP_ENABLED"] != "true" {
		t.Fatal("private Compose setup must explicitly enable signup")
	}
}

func TestHelmRequiresAnExplicitPublicOrigin(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm CLI is unavailable")
	}
	command := exec.Command("helm", "template", "opencluster", "deploy/helm/opencluster")
	command.Dir = "../.."
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "server.publicURL") {
		t.Fatalf("chart must require the public origin: %v: %s", err, output)
	}
	command = exec.Command("helm", "template", "opencluster", "deploy/helm/opencluster",
		"--set", "server.publicURL=https://console.example.com")
	command.Dir = "../.."
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("rendering with public origin: %v: %s", err, output)
	}
	for _, expected := range []string{"value: \"https://console.example.com\"", "containerPort: 8081", "port: 8081"} {
		if !strings.Contains(string(output), expected) {
			t.Errorf("rendered deployment is missing %q", expected)
		}
	}
}

func TestHelmSetupBindsLoopbackAndKeepsLocalProbes(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm CLI is unavailable")
	}
	args := []string{"template", "opencluster", "deploy/helm/opencluster", "--set", "server.publicURL=http://localhost:8080", "--set", "setup.enabled=true"}
	command := exec.Command("helm", args...)
	command.Dir = "../.."
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("rendering private setup: %v: %s", err, output)
	}
	for _, expected := range []string{`value: "127.0.0.1:8081"`, "name: OC_SETUP_ENABLED", `value: "true"`, "/bin/busybox", "http://127.0.0.1:8081/readyz", "http://127.0.0.1:8081/healthz"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("private setup manifest is missing %q", expected)
		}
	}
	if strings.Contains(string(output), "BOOTSTRAP_TOKEN") || strings.Contains(string(output), "httpGet:") {
		t.Fatal("private setup must use local exec probes without an administrator token")
	}
	for _, serviceType := range []string{"NodePort", "LoadBalancer"} {
		command = exec.Command("helm", append(args, "--set", "service.type="+serviceType)...)
		command.Dir = "../.."
		if output, err = command.CombinedOutput(); err == nil || !strings.Contains(string(output), "ClusterIP") {
			t.Fatalf("setup accepted public Service %s: %v: %s", serviceType, err, output)
		}
	}
}
