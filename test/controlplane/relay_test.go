package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestRelayRegistration(t *testing.T) {
	const organization = surfaceOrg

	relayAddress := freeAddress(t)
	var databaseDSN string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayListenAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		databaseDSN = cfg.DatabaseDSN
	})

	token := "bootstrap-token-for-the-registration-test"
	issueBootstrapToken(t, databaseDSN, organization, token)

	client := relayv1.NewRelayRegistrationServiceClient(dialRelay(t, relayAddress))
	request := &relayv1.RegisterRequest{
		ProtocolVersion:    1,
		RelayVersion:       "0.1.0-test",
		ClusterFingerprint: "kube-system-uid-under-test",
		Capabilities: []*relayv1.CapabilityDescriptor{
			{CapabilityId: "kubernetes.workload.runtime", CapabilityVersion: 1},
		},
	}

	t.Run("an unsupported protocol does not spend the bootstrap token", func(t *testing.T) {
		unsupported := &relayv1.RegisterRequest{
			RelayVersion: request.RelayVersion, ClusterFingerprint: request.ClusterFingerprint,
			Capabilities: request.Capabilities,
		}
		if _, err := register(t, client, organization, token, unsupported); err == nil {
			t.Fatal("an unsupported protocol was registered")
		} else {
			requireFailedPrecondition(t, err)
		}
	})

	var credential string
	t.Run("a valid token yields an identity", func(t *testing.T) {
		response, err := register(t, client, organization, token, request)
		if err != nil {
			t.Fatalf("registration was refused: %v", err)
		}
		if response.GetOrgId() != organization {
			t.Errorf("registered into %q, want %q", response.GetOrgId(), organization)
		}
		if response.GetRegistrationId() == "" {
			t.Error("no registration identity was issued")
		}
		if response.GetCredential() == "" {
			t.Fatal("no credential was issued; the relay could never authenticate a session")
		}
		if len(response.GetSpkiPins()) == 0 {
			t.Error("no pins were returned; the relay would have no trust anchor for its next connection")
		}
		credential = response.GetCredential()
	})

	var refusals []string
	t.Run("the same token cannot be spent twice", func(t *testing.T) {
		_, err := register(t, client, organization, token, request)
		refusals = append(refusals, requireFailedPrecondition(t, err))
	})

	t.Run("an invented token is refused", func(t *testing.T) {
		_, err := register(t, client, organization, "a-token-that-was-never-issued", request)
		refusals = append(refusals, requireFailedPrecondition(t, err))
	})

	t.Run("refusals are indistinguishable", func(t *testing.T) {
		if len(refusals) == 2 && refusals[0] != refusals[1] {
			t.Errorf("a spent token says %q and an unknown one says %q; the difference "+
				"tells an attacker which tokens exist", refusals[0], refusals[1])
		}
	})

	t.Run("the credential reaches no log line", func(t *testing.T) {
		if credential != "" && strings.Contains(plane.logs.String(), credential) {
			t.Error("the issued credential appears in the logs")
		}
	})
}

func register(
	t *testing.T,
	client relayv1.RelayRegistrationServiceClient,
	organization, token string,
	request *relayv1.RegisterRequest,
) (*relayv1.RegisterResponse, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx,
		"opencluster-org-id", organization,
		"opencluster-bootstrap-token", token)
	return client.Register(ctx, request)
}

func requireFailedPrecondition(t *testing.T, err error) string {
	t.Helper()

	if err == nil {
		t.Fatal("the attempt was accepted; it must be refused")
	}
	reported, ok := status.FromError(err)
	if !ok || reported.Code() != codes.FailedPrecondition {
		t.Fatalf("refused with %v, want FailedPrecondition — a relay treats that as "+
			"terminal and must not retry", err)
	}
	return reported.Message()
}

func issueBootstrapToken(t *testing.T, dsn, organization, token string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	digest := sha256.Sum256([]byte(token))
	ensureTestOrganization(t, dsn, organization)
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("opening bootstrap fixture connection: %v", err)
	}
	defer func() { _ = connection.Close(ctx) }()
	if _, err = connection.Exec(ctx, `INSERT INTO relay_bootstrap_token
		(bootstrap_digest, org_id, expires_at) VALUES ($1, $2, $3)`, digest[:],
		namedOrganization(t, organization), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("issuing the bootstrap token: %v", err)
	}
}

func openDatabase(t *testing.T, dsn string) *storage.Database {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	database, err := storage.OpenDatabase(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

func dialRelay(t *testing.T, address string) *grpc.ClientConn {
	t.Helper()

	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling the relay endpoint: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

var handedOut = struct {
	sync.Mutex
	taken map[string]bool
}{taken: map[string]bool{}}

func freeAddress(t *testing.T) string {
	t.Helper()

	for attempt := 0; attempt < 20; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserving a port: %v", err)
		}
		address := listener.Addr().String()
		if err = listener.Close(); err != nil {
			t.Fatalf("releasing the reserved port: %v", err)
		}

		handedOut.Lock()
		fresh := !handedOut.taken[address]
		handedOut.taken[address] = true
		handedOut.Unlock()
		if fresh {
			return address
		}
	}
	t.Fatal("no unused loopback port came back in twenty attempts")
	return ""
}
