package integrations

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

type Store interface {
	CreateIntegration(ctx context.Context, who authz.Principal, org tenancy.Organization,
		wanted NewIntegration) (Integration, error)
	IntegrationByID(ctx context.Context, id uuid.UUID) (Integration, error)
	// Integration reads one, scoped to the tenant.
	Integration(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Integration, error)
	IntegrationByInstallation(ctx context.Context, provider Provider,
		key InstallationKey) (Integration, Installation, error)
	// QueryIntegrations reports a page of a tenant's Integrations, narrowed, ordered and
	// paged by the database.
	QueryIntegrations(ctx context.Context, who authz.Principal, org tenancy.Organization,
		query Query) (List, error)
	CountIntegrationsByProvider(ctx context.Context, who authz.Principal,
		org tenancy.Organization) ([]ProviderCount, error)
	// ReviseIntegration changes what a PATCH may change and increments nothing secret.
	ReviseIntegration(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID, revision Revision) (Integration, error)
	// SetIntegrationDisabled turns an Integration off or back on without deleting it.
	SetIntegrationDisabled(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID, disabled bool) error
	DeleteIntegration(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID) error
	// RotateIntegrationWebhookSecret replaces the digest without disturbing identity.
	RotateIntegrationWebhookSecret(ctx context.Context, who authz.Principal,
		org tenancy.Organization, id uuid.UUID, digest []byte) error
	// RecordCredentialUnseal writes the audit event for one credential unseal: which
	// integration's credential was opened, and what for.
	RecordCredentialUnseal(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		purpose string) error
	ReplaceIntegrationCredential(ctx context.Context, who authz.Principal,
		org tenancy.Organization, id uuid.UUID, revision Revision, sealed []byte,
		verification Verification, installed *Installation) (Integration, error)
	// RecordIntegrationVerification writes what a verify run established onto the record.
	RecordIntegrationVerification(ctx context.Context, who authz.Principal,
		org tenancy.Organization, id uuid.UUID, verification Verification) (Integration, error)
	// IntegrationRelayStatus reports the bound Relay's presence and advertised
	// Relay Capabilities, for verification. The zero value when none is bound.
	IntegrationRelayStatus(ctx context.Context, org tenancy.Organization,
		relayID uuid.UUID) (RelayStatus, error)
	// LastAcceptedDelivery reports when an integration last accepted a webhook delivery,
	// zero when it never has.
	LastAcceptedDelivery(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) (time.Time, error)

	// StartConnectFlow records an installation flow so the return trip can be checked.
	// Only the state's digest is stored; the state itself travels through the browser.
	// It also clears the flows nobody finished, which is the ordinary case.
	StartConnectFlow(ctx context.Context, org tenancy.Organization, flow ConnectFlow,
		state string) error
	// RedeemConnectFlow consumes one exactly once and returns what it recorded. It takes
	// no organization: the callback carries a state and nothing that names a tenant, and
	// the row that is found is itself the authority for the organization. An unknown, an
	// expired and an already-consumed state are one refusal.
	RedeemConnectFlow(ctx context.Context, state string) (ConnectFlow, error)
}
