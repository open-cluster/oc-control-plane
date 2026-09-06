package controlplane

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func startEventProxy(t *testing.T, backend string) string {
	t.Helper()
	configuration, err := os.ReadFile("../../deploy/compose/frontend-nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(backend)
	if err != nil {
		t.Fatal(err)
	}
	hostPort, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	configuration = []byte(strings.ReplaceAll(string(configuration), "control-plane:8080",
		net.JoinHostPort(testcontainers.HostInternal, port)))
	path := filepath.Join(t.TempDir(), "nginx.conf")
	if err := os.WriteFile(path, configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:           "nginxinc/nginx-unprivileged:1.27-alpine",
			ExposedPorts:    []string{"8080/tcp"},
			HostAccessPorts: []int{hostPort},
			Files: []testcontainers.ContainerFile{{
				HostFilePath: path, ContainerFilePath: "/etc/nginx/nginx.conf", FileMode: 0o644,
			}},
			WaitingFor: wait.ForListeningPort("8080/tcp").WithStartupTimeout(time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Error(err)
		}
	})
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := container.MappedPort(ctx, "8080/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort(host, mapped.Port())
}
