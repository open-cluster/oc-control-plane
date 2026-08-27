package integrations

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// Store is everything the Integration domain needs from durable state.
//
// It is declared here rather than in the persistence package because the domain owns
// its vocabulary and persistence depends on it. Writes take the principal as well as the
// organization: the middleware has already checked the membership, and taking it here means
// the actor reaches the audit row the write commits alongside itself.
type Store interface {
	// CreateIntegration records one configured installation. The Relay reference is not
	// read first and then written against: the composite foreign key means the insert
	// itself fails when it belongs to another organization.
	CreateIntegration(ctx context.Context, who authz.Principal, org tenancy.Organization,
		wanted NewIntegration) (Integration, error)
	// IntegrationByID resolves an Integration from its opaque identifier alone, across
	// every database, and returns the organization it belongs to. It is the ONE read that
	// takes no organization: an inbound delivery names its Integration and nothing else,
	// and the row that is found is itself the authority for the tenant.
	IntegrationByID(ctx context.Context, id uuid.UUID) (Integration, error)
	// Integration reads one, scoped to the tenant.
	Integration(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Integration, error)
	// IntegrationConfiguredAs resolves the Integration of one type in this organization
	// whose configuration is exactly the one given, and reports ErrUnknown when none is.
	// It is what makes connecting the same installation twice a re-verification rather
	// than a second record of the same thing — including the case where the customer
	// changed an existing installation and the provider sent them back here.
	IntegrationConfiguredAs(ctx context.Context, org tenancy.Organization, typeID TypeID,
		configuration map[string]any) (Integration, error)
	// QueryIntegrations reports a page of a tenant's Integrations, narrowed, ordered and
	// paged by the database.
	QueryIntegrations(ctx context.Context, who authz.Principal, org tenancy.Organization,
		query Query) (List, error)
	// CountIntegrationsByType reports how many Integrations of each type a tenant has,
	// for the catalog.
	CountIntegrationsByType(ctx context.Context, who authz.Principal,
		org tenancy.Organization) ([]TypeCount, error)
	// ReviseIntegration changes what a PATCH may change and increments nothing secret.
	ReviseIntegration(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID, revision Revision) (Integration, error)
	// SetIntegrationDisabled turns an Integration off or back on without deleting it.
	SetIntegrationDisabled(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID, disabled bool) error
	// DeleteIntegration removes one that nothing depends on, and refuses with ErrInUse
	// when alertEvents, jobs or ledger entries reference it.
	DeleteIntegration(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID) error
	// RotateIntegrationWebhookSecret replaces the digest without disturbing identity, so a
	// suspected disclosure does not mean recreating the Integration.
	RotateIntegrationWebhookSecret(ctx context.Context, who authz.Principal,
		org tenancy.Organization, id uuid.UUID, digest []byte, fingerprint string) error
	// RecordCredentialUnseal writes the audit event for one credential unseal: which
	// integration's credential was opened, and what for. Recorded BEFORE the credential
	// is used; a use that cannot be recorded does not happen.
	RecordCredentialUnseal(ctx context.Context, org tenancy.Organization, id uuid.UUID,
		purpose string) error
	// ReplaceIntegrationCredential swaps the sealed outbound credential, applies the
	// revision it travelled with, records what the probe of the new one established, and
	// refreshes the routing record when the flow brought one — all in one transaction, so
	// a refusal anywhere leaves nothing half-applied. Only an Integration already holding
	// a credential accepts one: a replacement, never an acquisition.
	//
	// installed is nil for a type that routes no inbound events, and for a credential
	// pasted into the configuration form, which names no installation.
	ReplaceIntegrationCredential(ctx context.Context, who authz.Principal,
		org tenancy.Organization, id uuid.UUID, revision Revision, sealed []byte,
		fingerprint string, verification Verification, installed *Installation) (Integration, error)
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
