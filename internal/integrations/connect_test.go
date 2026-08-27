package integrations

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/secrets"
)

// storeUnderSeal is a Store that must not be reached. Its methods are the embedded
// interface's, which is nil, so any call panics — which is the assertion: a flow refused
// before it starts must record nothing, and "recorded nothing" is stronger stated as
// "could not have recorded anything".
type storeUnderSeal struct{ Store }

// connectingPrincipal is a member who may create integrations in the named organization.
func connectingPrincipal(t *testing.T, organization string) authz.Principal {
	t.Helper()
	org, err := tenancy.NewOrganization(organization)
	if err != nil {
		t.Fatalf("building an organization: %v", err)
	}
	principal, err := authz.NewPrincipal(authz.KindUser, "user-1", "Ada",
		[]authz.Membership{{Organization: org, Role: authz.Admin}})
	if err != nil {
		t.Fatalf("building a principal: %v", err)
	}
	return principal
}

// sealingDefinition is a type whose installation flow comes back holding a credential,
// which is the case that cannot proceed without somewhere to seal it.
func sealingDefinition(authorized *bool) Definition {
	return Definition{
		ID: 99, Key: "stub", Name: "Stub", Category: CategoryCollaboration,
		Config: []Field{{
			Name: "token", Title: "Token", Description: "a token",
			Type: FieldString, Required: true, Secret: true,
		}},
		Probe: func(context.Context, ProbeInput) Verification {
			return Verification{Status: StatusActive}
		},
		Connect: &Connect{
			SealsCredential: true,
			Authorize: func(state, callback string) (string, error) {
				*authorized = true
				return "https://vendor.example/install?state=" + state, nil
			},
			Redeem: func(context.Context, ConnectReturn) (ConnectBinding, error) {
				return ConnectBinding{Name: "Stub", Credential: "a-token"}, nil
			},
		},
	}
}

// startConnectAgainst drives the start leg as an authenticated member of the tenant.
func startConnectAgainst(t *testing.T, handlers Handlers) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/organizations/acme/integrations/stub/connect", nil)
	request.SetPathValue("organization", "acme")
	request.SetPathValue("type", "stub")
	request = request.WithContext(
		authz.WithPrincipal(request.Context(), connectingPrincipal(t, "acme")))

	recorder := httptest.NewRecorder()
	handlers.startConnect(recorder, request)
	return recorder
}

// A deployment with nowhere to seal must refuse at the START of the flow.
//
// The order is the whole point. Refusing on the way back means the customer has already
// chosen a workspace and granted real permissions in somebody else's product, and what
// they get for it is an error and no integration — with a live credential in this
// process's memory that it cannot store. The only honest moment to say "this deployment
// cannot hold a credential" is before the browser leaves.
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

// The same deployment still connects a type whose flow returns no credential. GitHub's
// runtime credential is minted from the deployment's own App, so a missing sealing key is
// nothing to do with it, and refusing it would be refusing for a reason that is not true.
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

// recordingConnectStore accepts the one write the start leg makes and nothing else.
type recordingConnectStore struct{ Store }

func (recordingConnectStore) StartConnectFlow(
	context.Context, tenancy.Organization, ConnectFlow, string,
) error {
	return nil
}

// capturingStore is enough of a Store to drive one callback to its write, and it keeps
// what was written so the test can assert on what reached durable state.
type capturingStore struct {
	Store
	flow                ConnectFlow
	created             NewIntegration
	replaced            []byte
	replacedFingerprint string
	// reinstalled is the routing record the reconnect carried into the SAME write as the
	// credential. A reconnect that replaced one and not the other would be a live
	// credential with stale routing.
	reinstalled *Installation
	reVerified  bool
	existing    bool
}

func (s *capturingStore) RedeemConnectFlow(context.Context, string) (ConnectFlow, error) {
	return s.flow, nil
}

func (s *capturingStore) IntegrationConfiguredAs(
	context.Context, tenancy.Organization, TypeID, map[string]any,
) (Integration, error) {
	if s.existing {
		return Integration{ID: uuid.New(), Type: 99}, nil
	}
	return Integration{}, ErrUnknown
}

func (s *capturingStore) CreateIntegration(
	_ context.Context, _ authz.Principal, _ tenancy.Organization, wanted NewIntegration,
) (Integration, error) {
	s.created = wanted
	return Integration{
		ID: wanted.ID, Type: wanted.Type, Name: wanted.Name,
		Status: StatusActive, CredentialSealed: wanted.CredentialSealed,
	}, nil
}

// completeConnectAgainst drives the return leg as the principal that started the flow.
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

// A proven return that came back holding a credential must seal it through the ordinary
// path. Before this, ConnectBinding carried no credential at all, so a provider whose
// runtime access needs one had nowhere to put it — and the only ways out were a second
// credential path or an integration that verifies once and can never read again.
func TestACredentialFromAProvenReturnIsSealedOntoTheRecord(t *testing.T) {
	t.Parallel()

	principal := connectingPrincipal(t, "acme")
	sealer, err := seal.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}

	var probed string
	definition := sealingDefinition(new(bool))
	definition.Probe = func(_ context.Context, input ProbeInput) Verification {
		probed = input.Credential
		return Verification{Status: StatusActive, Grants: []string{"channels:read"}}
	}
	catalog, err := NewCatalog(definition)
	if err != nil {
		t.Fatalf("assembling the catalog: %v", err)
	}

	store := &capturingStore{flow: ConnectFlow{
		ID: uuid.New(), Organization: "acme", Type: 99, Principal: principal.ID(),
	}}
	recorder := completeConnectAgainst(t, Handlers{
		Store:     store,
		Catalog:   catalog,
		Logger:    slog.New(slog.DiscardHandler),
		Sealer:    sealer,
		PublicURL: "https://opencluster.example",
	}, principal)

	if recorder.Code != http.StatusOK {
		t.Fatalf("a proven return = %d: %s", recorder.Code, recorder.Body.String())
	}

	// Probed before stored. A credential the provider would refuse must never come to
	// rest, which is the rule the pasted path already keeps.
	if probed != "a-token" {
		t.Errorf("the probe was given %q, want the credential the flow obtained", probed)
	}
	if len(store.created.CredentialSealed) == 0 {
		t.Fatal("nothing was sealed onto the record; the integration would verify once " +
			"and never be able to read again")
	}
	if store.created.CredentialFingerprint == "" {
		t.Error("no credential fingerprint was minted; an operator cannot tell one " +
			"credential from the next after a reconnect")
	}

	// The plaintext reaches durable state only as sealed bytes.
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
	_ context.Context, _ authz.Principal, _ tenancy.Organization, id uuid.UUID,
	_ Revision, sealed []byte, fingerprint string, verification Verification,
	installed *Installation,
) (Integration, error) {
	s.replaced, s.replacedFingerprint = sealed, fingerprint
	s.reinstalled = installed
	return Integration{ID: id, Type: 99, Status: verification.Status}, nil
}

func (s *capturingStore) RecordIntegrationVerification(
	_ context.Context, _ authz.Principal, _ tenancy.Organization, id uuid.UUID,
	verification Verification,
) (Integration, error) {
	s.reVerified = true
	return Integration{ID: id, Type: 99, Status: verification.Status}, nil
}

// Reconnecting a workspace this tenant already has must take the credential the flow just
// obtained, not re-verify the one on the record. Authorizing again issues a new
// credential, and the old one having stopped working is the ordinary reason somebody
// reconnects — so re-verifying the stored one would report the very failure they came to
// fix and drop the working credential on the floor.
func TestReconnectingReplacesTheCredentialRatherThanReverifyingTheOldOne(t *testing.T) {
	t.Parallel()

	principal := connectingPrincipal(t, "acme")
	sealer, err := seal.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}

	definition := sealingDefinition(new(bool))
	definition.Probe = func(_ context.Context, input ProbeInput) Verification {
		if input.Credential != "a-token" {
			return Verification{Status: StatusFailed, Note: "not the fresh credential"}
		}
		return Verification{Status: StatusActive}
	}
	catalog, err := NewCatalog(definition)
	if err != nil {
		t.Fatalf("assembling the catalog: %v", err)
	}

	store := &capturingStore{existing: true, flow: ConnectFlow{
		ID: uuid.New(), Organization: "acme", Type: 99, Principal: principal.ID(),
	}}
	recorder := completeConnectAgainst(t, Handlers{
		Store:     store,
		Catalog:   catalog,
		Logger:    slog.New(slog.DiscardHandler),
		Sealer:    sealer,
		PublicURL: "https://opencluster.example",
	}, principal)

	if recorder.Code != http.StatusOK {
		t.Fatalf("reconnecting = %d: %s", recorder.Code, recorder.Body.String())
	}
	if store.reVerified {
		t.Error("the stored credential was re-verified; the fresh one the customer just " +
			"authorized was discarded")
	}
	if len(store.replaced) == 0 || store.replacedFingerprint == "" {
		t.Fatal("no credential was replaced onto the existing record")
	}
	if bytes.Contains(store.replaced, []byte("a-token")) {
		t.Error("the replaced credential is recoverable from what was stored")
	}
}
