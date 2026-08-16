package integrations

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// Store is everything this capability needs from durable state.
//
// It is declared here rather than in the persistence package because the capability owns
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
	// every placement, and returns the organization it belongs to. It is the ONE read that
	// takes no organization: an inbound delivery names its Integration and nothing else,
	// and the row that is found is itself the authority for the tenant.
	IntegrationByID(ctx context.Context, id uuid.UUID) (Integration, error)
	// Integration reads one, scoped to the tenant.
	Integration(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Integration, error)
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
	// when signals, jobs or ledger entries reference it.
	DeleteIntegration(ctx context.Context, who authz.Principal, org tenancy.Organization,
		id uuid.UUID) error
	// RotateIntegrationWebhookSecret replaces the digest without disturbing identity, so a
	// suspected disclosure does not mean recreating the Integration.
	RotateIntegrationWebhookSecret(ctx context.Context, who authz.Principal,
		org tenancy.Organization, id uuid.UUID, digest []byte, fingerprint string) error
	// RecordIntegrationVerification writes what a verify run established onto the record.
	RecordIntegrationVerification(ctx context.Context, who authz.Principal,
		org tenancy.Organization, id uuid.UUID, verification Verification) (Integration, error)
	// IntegrationRelayStatus reports the bound Relay's presence and advertised
	// capabilities, for verification. The zero value when none is bound.
	IntegrationRelayStatus(ctx context.Context, org tenancy.Organization,
		relayID uuid.UUID) (RelayStatus, error)
	// LastAcceptedDelivery reports when an integration last accepted a webhook delivery,
	// zero when it never has.
	LastAcceptedDelivery(ctx context.Context, org tenancy.Organization,
		id uuid.UUID) (time.Time, error)
}
