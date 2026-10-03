package integrations

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/seal"
)

type storeUnderSeal struct{ Store }

func connectingPrincipal(t *testing.T, organization string) authz.Principal {
	t.Helper()
	org, err := uuid.Parse(organization)
	if err != nil {
		t.Fatalf("building an organization: %v", err)
	}
	principal, err := authz.NewPrincipal(uuid.New(), uuid.New(), "Ada", "ada@example.test",
		authz.Membership{Organization: org, Role: authz.Admin})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

func sealingDefinition(authorized *bool) Definition {
	return Definition{
		Manifest: Manifest{Key: "stub", Name: "Stub", Category: CategoryCollaboration,
			Config: []Field{{Key: "token", Label: "Token",
				Type: FieldString, Required: true, Secret: true}}},
		Probe: func(context.Context, ProbeInput) Verification {
			return Verification{Status: StatusVerified}
		},
		Connect: &Connect{
			SealsCredential: true,
			Authorize: func(_ context.Context, state, callback string) (string, error) {
				*authorized = true
				return "https://vendor.example/install?state=" + state, nil
			},
			Redeem: func(context.Context, ConnectReturn) (ConnectBinding, error) {
				return ConnectBinding{Name: "Stub", Credential: "a-token", Installation: &Installation{
					Key: InstallationKey{"app", "workspace"},
				}}, nil
			},
		},
	}
}

func startConnectAgainst(t *testing.T, handlers Handlers) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/integration-types/stub/connect", nil)
	request.Header.Set("Origin", "https://console.example.com")
	router, err := authz.Router([]authz.Route{
		{Method: http.MethodPost, Pattern: "/api/v1/integration-types/{type}/connect",
			Permission: authz.IntegrationCreate, Handler: http.HandlerFunc(handlers.startConnect)},
	}, authz.Guard{
		Resolve: func(*http.Request) (authz.Principal, error) {
			return connectingPrincipal(t, "11111111-1111-4111-8111-111111111111"), nil
		},
		Origin: "https://console.example.com",
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("building the authorization router: %v", err)
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestStartConnectRefusesBeforeTheBrowserLeavesWhenNothingCanSeal(t *testing.T) {
	t.Parallel()

	authorized := false
	catalog, err := NewCatalog(sealingDefinition(&authorized))
	if err != nil {
		t.Fatalf("assembling the catalog: %v", err)
	}

	recorder := startConnectAgainst(t, Handlers{
		Store:     storeUnderSeal{},
		Catalog:   catalog,
		Logger:    slog.New(slog.DiscardHandler),
		Sealer:    seal.Sealer{},
		PublicURL: "https://opencluster.example",
	})

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("starting a connect with no sealing key = %d, want 503: %s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "sealing key") {
		t.Errorf("the refusal %q does not say what is missing", recorder.Body.String())
	}
	if authorized {
		t.Error("an authorization URL was built for a deployment that cannot store what " +
			"comes back; the customer would have granted permissions for nothing")
	}
}

func TestStartConnectWithoutASealingKeyStillServesATypeThatSealsNothing(t *testing.T) {
	t.Parallel()

	authorized := false
	definition := sealingDefinition(&authorized)
	definition.Connect.SealsCredential = false
	definition.Config = nil
	catalog, err := NewCatalog(definition)
	if err != nil {
		t.Fatalf("assembling the catalog: %v", err)
	}

	recorder := startConnectAgainst(t, Handlers{
		Store:     recordingConnectStore{},
		Catalog:   catalog,
		Logger:    slog.New(slog.DiscardHandler),
		Sealer:    seal.Sealer{},
		PublicURL: "https://opencluster.example",
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("starting a credential-free connect = %d, want 200: %s",
			recorder.Code, recorder.Body.String())
	}
	if !authorized {
		t.Error("no authorization URL was built for a flow that needs no sealing key")
	}
}

type recordingConnectStore struct{ Store }

func (recordingConnectStore) StartConnectFlow(
	context.Context, uuid.UUID, ConnectFlow, string,
) error {
	return nil
}

type capturingStore struct {
	Store
	flow        ConnectFlow
	created     NewIntegration
	replaced    []byte
	reinstalled *Installation
	reVerified  bool
	existing    bool
}

func (s *capturingStore) RedeemConnectFlow(context.Context, string) (ConnectFlow, error) {
	return s.flow, nil
}

func (s *capturingStore) IntegrationByInstallation(
	context.Context, Provider, InstallationKey,
) (Integration, Installation, error) {
	if s.existing {
		return Integration{ID: uuid.New(), OrgID: "11111111-1111-4111-8111-111111111111", Provider: "stub"}, Installation{}, nil
	}
	return Integration{}, Installation{}, ErrUnknown
}

func (s *capturingStore) CreateIntegration(
	_ context.Context, _ authz.Principal, _ uuid.UUID, wanted NewIntegration,
) (Integration, error) {
	s.created = wanted
	return Integration{
		ID: wanted.ID, Provider: wanted.Provider, Name: wanted.Name,
		Status: StatusVerified, CredentialSealed: wanted.CredentialSealed,
	}, nil
}

func completeConnectAgainst(
	t *testing.T, handlers Handlers, principal authz.Principal,
) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, CallbackPath+"?state=a-state&code=a-code", nil)
	request = request.WithContext(authz.WithPrincipal(request.Context(), principal))

	recorder := httptest.NewRecorder()
	handlers.completeConnect(recorder, request)
	return recorder
}

func TestACredentialFromAProvenReturnIsSealedOntoTheRecord(t *testing.T) {
	t.Parallel()

	principal := connectingPrincipal(t, "11111111-1111-4111-8111-111111111111")
	sealer, err := seal.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}

	var (
		probed             string
		probedInstallation *Installation
	)
	definition := sealingDefinition(new(bool))
	definition.Probe = func(_ context.Context, input ProbeInput) Verification {
		probed = input.Credential
		probedInstallation = input.Integration.Installation
		return Verification{Status: StatusVerified, Grants: []string{"channels:read"}}
	}
	catalog, err := NewCatalog(definition)
	if err != nil {
		t.Fatalf("assembling the catalog: %v", err)
	}

	store := &capturingStore{flow: ConnectFlow{
		Organization: "11111111-1111-4111-8111-111111111111", Provider: "stub", Principal: principal.UserID().String(),
	}}
	recorder := completeConnectAgainst(t, Handlers{
		Store:     store,
		Catalog:   catalog,
		Logger:    slog.New(slog.DiscardHandler),
		Sealer:    sealer,
		PublicURL: "https://opencluster.example",
	}, principal)

	if recorder.Code != http.StatusFound {
		t.Fatalf("a proven return = %d: %s", recorder.Code, recorder.Body.String())
	}

	if probed != "a-token" {
		t.Errorf("the probe was given %q, want the credential the flow obtained", probed)
	}
	if probedInstallation == nil || !reflect.DeepEqual(
		probedInstallation.Key, InstallationKey{"app", "workspace"}) {
		t.Fatalf("the probe did not receive the established installation: %#v", probedInstallation)
	}
	if len(store.created.CredentialSealed) == 0 {
		t.Fatal("nothing was sealed onto the record; the integration would verify once " +
			"and never be able to read again")
	}
	if bytes.Contains(store.created.CredentialSealed, []byte("a-token")) {
		t.Fatal("the credential is recoverable from what was stored")
	}
	opened, err := sealer.Open(store.created.CredentialSealed,
		CredentialBinding(store.created.ID))
	if err != nil || opened != "a-token" {
		t.Errorf("the sealed credential does not open to what the flow obtained: %q, %v",
			opened, err)
	}
}

func (s *capturingStore) ReplaceIntegrationCredential(
	_ context.Context, _ authz.Principal, _ uuid.UUID, id uuid.UUID,
	_ Revision, sealed []byte, verification Verification,
	installed *Installation,
) (Integration, error) {
	s.replaced = sealed
	s.reinstalled = installed
	return Integration{ID: id, Provider: "stub", Status: verification.Status}, nil
}

func (s *capturingStore) RecordIntegrationVerification(
	_ context.Context, _ authz.Principal, _ uuid.UUID, id uuid.UUID,
	verification Verification,
) (Integration, error) {
	s.reVerified = true
	return Integration{ID: id, Provider: "stub", Status: verification.Status}, nil
}

func TestReconnectingReplacesTheCredentialRatherThanReverifyingTheOldOne(t *testing.T) {
	t.Parallel()

	principal := connectingPrincipal(t, "11111111-1111-4111-8111-111111111111")
	sealer, err := seal.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}

	definition := sealingDefinition(new(bool))
	definition.Probe = func(_ context.Context, input ProbeInput) Verification {
		if input.Credential != "a-token" {
			return Verification{Status: StatusFailed, Note: "not the fresh credential"}
		}
		return Verification{Status: StatusVerified}
	}
	catalog, err := NewCatalog(definition)
	if err != nil {
		t.Fatalf("assembling the catalog: %v", err)
	}

	store := &capturingStore{existing: true, flow: ConnectFlow{
		Organization: "11111111-1111-4111-8111-111111111111", Provider: "stub", Principal: principal.UserID().String(),
	}}
	recorder := completeConnectAgainst(t, Handlers{
		Store:     store,
		Catalog:   catalog,
		Logger:    slog.New(slog.DiscardHandler),
		Sealer:    sealer,
		PublicURL: "https://opencluster.example",
	}, principal)

	if recorder.Code != http.StatusFound {
		t.Fatalf("reconnecting = %d: %s", recorder.Code, recorder.Body.String())
	}
	if store.reVerified {
		t.Error("the stored credential was re-verified; the fresh one the customer just " +
			"authorized was discarded")
	}
	if len(store.replaced) == 0 {
		t.Fatal("no credential was replaced onto the existing record")
	}
	if bytes.Contains(store.replaced, []byte("a-token")) {
		t.Error("the replaced credential is recoverable from what was stored")
	}
}
