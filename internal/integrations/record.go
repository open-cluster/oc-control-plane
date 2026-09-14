package integrations

import (
	"errors"
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

// Refusals a mutation can produce. Declared here because the Integration domain owns its
// vocabulary; persistence returns these.
var (
	// ErrUnknown reports an Integration this organization does not have.
	ErrUnknown = errors.New("integration unknown")
	// ErrCrossTenant reports an Integration whose Relay does not belong to the
	// organization the request named. A single error on purpose: which half of a crossed
	// boundary was wrong is not a fact worth returning to whoever tried it.
	ErrCrossTenant = errors.New("integration names something outside its organization")
	// ErrInUse refuses a delete while durable records depend on the Integration. The
	// record of what a source produced must survive, which is why disabling exists.
	ErrInUse = errors.New("integration has records depending on it; disable it instead")
	// ErrBadCursor reports a page position that did not come from a previous response.
	ErrBadCursor = errors.New("after is not a page position from a previous response")
	// ErrWorkspaceTaken refuses a connection to a vendor workspace another Integration is
	// already installed in, anywhere in this deployment.
	//
	// It is a REFUSAL rather than a failure, and the deployment-wide scope is the point:
	// an inbound event resolves workspace to Integration to organization, and a workspace
	// two tenants could both claim would resolve to two answers at exactly the moment the
	// product starts trusting that chain.
	ErrWorkspaceTaken = errors.New(
		"another integration in this deployment is already installed in that workspace")
	// ErrInvalidInstallation reports a routing record that could not be written: a key
	// naming no application or no workspace, or a type this build holds no installation
	// table for. It is a programming error rather than a caller's, and it is refused
	// loudly because the alternative is discovering it at the first inbound event, as
	// silence.
	ErrInvalidInstallation = errors.New("installation cannot be recorded")
)

// Integration is one configured installation belonging to an organization.
type Integration struct {
	ID    uuid.UUID
	OrgID string
	Type  TypeID
	Name  string
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
	Type                TypeID
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

// Installation is durable provider identity used for reconnect matching, inbound routing,
// and provider Tool credentials. Capability evidence remains in VerificationGrants.
type Installation struct {
	// Application is the vendor application the installation was made under. It is part of
	// the key because one deployment may serve more than one registration over its life,
	// and a workspace identifier alone would collide across them.
	Application string
	// Enterprise is the vendor's enterprise or grid identity, empty where there is none.
	// Empty rather than absent: two absent values must compare equal, or the same
	// workspace could be installed twice under rows that look distinct.
	Enterprise string
	// EnterpriseWide reports an installation made across a whole enterprise rather than
	// into one workspace inside it. It is NOT derivable from Enterprise being set: a
	// workspace-scoped install inside a grid carries an enterprise identity and is not
	// enterprise-wide, and treating the two as one would mislabel exactly the case the
	// enterprise fields exist to identify correctly.
	EnterpriseWide bool
	// Workspace is the vendor's own identity for the place the installation lives.
	Workspace string
	// Agent is the identity this product answers AS in that workspace.
	//
	// Load-bearing rather than informational: a message authored by this identity is
	// discarded before anything else looks at it, which is what stops the agent answering
	// itself and looping until a rate limit ends it.
	Agent      string
	Authorizer string
}

// Key is what an inbound event resolves BY.
func (i Installation) Key() InstallationKey {
	return InstallationKey{
		Application: i.Application, Enterprise: i.Enterprise, Workspace: i.Workspace,
	}
}

// InstallationKey is the identity an inbound event is resolved through. It is unique across
// the whole deployment and deliberately not scoped to an organization: the value of the
// chain is that its first hop is single-valued, and a per-tenant uniqueness would let two
// tenants claim one workspace and make it ambiguous exactly when an event arrives.
type InstallationKey struct {
	Application string
	Enterprise  string
	Workspace   string
}

// Complete reports whether this key names an installation at all. An event resolved through
// a partial key would be an event resolved through a wildcard.
func (k InstallationKey) Complete() bool {
	return k.Application != "" && k.Workspace != ""
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
	// Type narrows to one Integration Type; zero means all.
	Type TypeID
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

// List is a page of an organization's Integrations.
type List struct {
	Integrations []Integration
	Next         string
}

// TypeCount is how many Integrations of one type an organization has configured.
type TypeCount struct {
	Type  TypeID
	Count int
}
