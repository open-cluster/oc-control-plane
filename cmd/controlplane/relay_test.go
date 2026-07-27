package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// The endpoint serves plaintext HTTP/2. TLS terminates at the edge, which is where the
// Relay's key pinning applies; a test that dialled TLS here would be testing a topology
// that does not ship.
//
// What is asserted is what a Relay or an operator could observe: an identity was issued, a
// second attempt with the same token was refused, an invented token was refused
// identically, and the credential appeared in no log line. How consumption is implemented is
// deliberately not asserted, so the implementation can change without rewriting this.
func TestRelayRegistration(t *testing.T) {
	// The organization the harness assigns a placement to. An unassigned one is refused
	// exactly like a bad token, which is deliberate — the refusal must not reveal which
	// organizations exist — and makes a wrong name here look like a registration defect.
	const organization = "org-a"

	relayAddress := freeAddress(t)
	var placementDSN string
	plane := startControlPlane(t, func(cfg *config.Config) {
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		for _, dsn := range cfg.Placements {
			placementDSN = dsn
		}
	})

	token := "bootstrap-token-for-the-registration-test"
	issueBootstrapToken(t, placementDSN, organization, token)

	client := relayv1.NewRelayRegistrationServiceClient(dialRelay(t, relayAddress))
	request := &relayv1.RegisterRequest{
		ProtocolVersion:    1,
		RelayVersion:       "0.1.0-test",
		ClusterFingerprint: "kube-system-uid-under-test",
		Capabilities: []*relayv1.CapabilityDescriptor{
			{CapabilityId: "kubernetes.workload.runtime", CapabilityVersion: 1},
		},
	}

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

// requireFailedPrecondition asserts the terminal refusal status and returns its message, so
// the caller can compare one refusal against another.
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

// issueBootstrapToken seeds a token the way an operator eventually will, through the same
// storage function rather than by writing the row from the test.
func issueBootstrapToken(t *testing.T, dsn, organization, token string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	digest := sha256.Sum256([]byte(token))
	if err := openPlacement(t, dsn).IssueBootstrapToken(
		ctx, namedOrganization(t, organization), digest[:], time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("issuing the bootstrap token: %v", err)
	}
}

// openPlacement connects to the same database the running control plane uses, so a test can
// act as the parts of the system that are not built yet — the operator issuing a token, the
// planner enqueueing work — through their real storage functions rather than by writing rows.
func openPlacement(t *testing.T, dsn string) *storage.Placements {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	placements, err := storage.OpenPlacements(ctx, storage.Layout{
		Placements:       map[string]string{"seed": dsn},
		DefaultPlacement: "seed",
	})
	if err != nil {
		t.Fatalf("opening the placement: %v", err)
	}
	t.Cleanup(placements.Close)
	return placements
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

// freeAddress reserves a port and releases it, because the relay endpoint is configured by
// address rather than discovered after binding. The gap is small and the alternative — a
// fixed port — makes the suite fail whenever anything else on the machine holds it.
func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return address
}
