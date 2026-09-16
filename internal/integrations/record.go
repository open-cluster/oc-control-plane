package integrations

import (
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusVerified Status = "verified"
	StatusFailed   Status = "failed"
)

func (s Status) String() string {
	return string(s)
}

var (
	ErrUnknown           = errors.New("integration unknown")
	ErrCrossTenant       = errors.New("integration names something outside its organization")
	ErrInUse             = errors.New("integration has records depending on it; disable it instead")
	ErrBadCursor         = errors.New("after is not a page position from a previous response")
	ErrInstallationTaken = errors.New(
		"another integration in this deployment already owns that provider installation")
	ErrInvalidInstallation = errors.New("installation cannot be recorded")
)

type Integration struct {
	ID       uuid.UUID
	OrgID    string
	Provider Provider
	Name     string
	// Configuration is the provider-specific non-secret settings, shaped by the type's
	// schema. It never holds a credential.
	Configuration map[string]any
	// RelayID is the installation that serves this Integration, and the zero UUID when
	// none does.
	RelayID uuid.UUID
	// WebhookSecretDigest is the SHA-256 of the shared secret an inbound source presents.
	// Empty for a type that receives no webhooks. The secret itself exists only at
	// creation and rotation and is never read back.
	WebhookSecretDigest []byte
	// CredentialSealed is the outbound credential, sealed under the deployment's key.
	// Nil for a type that presents none. It is opened only to be presented to the
	// provider — verification and tool calls — and no view renders it.
	CredentialSealed   []byte
	Status             Status
	VerifiedAt         time.Time
	VerificationGrants []string
	Disabled           bool
	CreatedAt          time.Time
	Installation       *Installation
}

// CredentialBinding is what an Integration's sealed credential is bound to: the row's
// own identity. One derivation, used by everything that seals or opens, so a blob moved
// onto another row refuses to open there.
func CredentialBinding(id uuid.UUID) []byte { return id[:] }

// NewIntegration is what an operator asked for. It travels as one value because the fields
// constrain each other: a relay-served type needs a Relay, a webhook-receiving type needs a
// secret digest, and validating them apart would let an invalid combination reach the
// database and come back as a constraint violation rather than an answer.
type NewIntegration struct {
	// ID is minted by the handler BEFORE anything is sealed, because the sealed
	// credential is bound to the row's identity and the binding must exist first.
	ID                  uuid.UUID
	Provider            Provider
	Name                string
	Configuration       map[string]any
	RelayID             uuid.UUID
	WebhookSecretDigest []byte
	CredentialSealed    []byte
	// Verification, when non-nil, is what the pre-creation probe established: the
	// Integration is born verified, in the same transaction that records it. Nil means it
	// is born configured, with nothing having checked it.
	Verification *Verification
	// Installation, when non-nil, is the vendor-side identity an inbound event resolves
	// through, written in the SAME transaction as the row. Nil for every type that
	// receives no events and for a credential pasted into the configuration form, which
	// names no installation to route to.
	Installation *Installation
}

// Installation is the durable provider-side identity bound to an Integration. It is used
// for reconnect matching, inbound routing, and provider Tool credentials.
type Installation struct {
	Key InstallationKey `json:"key"`
	// ProviderActorID is the provider identity OpenCluster acts as. Slack uses its bot
	// User ID to discard self-authored events before they can form a reply loop.
	ProviderActorID string `json:"providerActorId,omitempty"`
}

// InstallationKey is the provider-owned ordered tuple persisted as text[]. GitHub uses
// [installation_id]. Slack uses [app_id, team_id], or [app_id, enterprise_id, team_id]
// when the installation belongs to a Slack Enterprise. It is deployment-unique together
// with Provider because an inbound provider event names no OpenCluster organization.
type InstallationKey []string

// Complete reports whether the tuple is non-empty and contains no empty element. Each
// provider adapter owns its required arity and order.
func (k InstallationKey) Complete() bool {
	if len(k) == 0 {
		return false
	}
	return !slices.Contains(k, "")
}

// Revision is what a PATCH may change. Nil means "leave it alone", which is different from
// "set it empty".
type Revision struct {
	Name          *string
	Configuration map[string]any
}

// Page is a position in a listing.
type Page struct {
	Limit int
	After string
}

// Query is what a caller may narrow a listing by. Every field is applied by the database.
type Query struct {
	Page       Page
	Sort       string
	Descending bool
	// Provider narrows to one Integration Type; empty means all.
	Provider Provider
	// Relay narrows to the Integrations one Relay serves, which is what disabling it would
	// cost.
	Relay uuid.UUID
	// Search matches the name, because that is what the operator gave it and therefore the
	// only part they will remember during an incident.
	Search string
	// Disabled narrows to Integrations an operator has turned off, or the ones they have
	// not. Nil means "I did not ask".
	Disabled *bool
}

type List struct {
	Integrations []Integration
	Next         string
}

// ProviderCount is how many Integrations of one provider an organization has configured.
type ProviderCount struct {
	Provider Provider
	Count    int
}
