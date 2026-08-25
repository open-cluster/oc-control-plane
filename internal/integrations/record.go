package integrations

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status is where an Integration has got to, as observed rather than as declared. Persisted
// as the integer in the column; the values are frozen.
//
// `disabled` is deliberately not in this list: DisabledAt says an operator turned it off,
// and collapsing the two would lose whether it was working when they did — which is what
// they need to know when they turn it back on during an incident.
type Status int16

const (
	// StatusConfigured is the state an Integration is created in: it exists and nothing
	// has checked it.
	StatusConfigured Status = iota + 1
	StatusActive
	// StatusDegraded makes partial success visible on the record itself: the far end
	// answered and some of what this type offers is unavailable.
	StatusDegraded
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusConfigured:
		return "configured"
	case StatusActive:
		return "active"
	case StatusDegraded:
		return "degraded"
	case StatusFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

// Refusals a mutation can produce. Declared here because the Integration domain owns its
// vocabulary; persistence returns these.
var (
	// ErrNameTaken reports a name another Integration in the organization holds.
	ErrNameTaken = errors.New("integration name is already used in this organization")
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

// WebhookSecret is what a read may say about the inbound secret, which is never the secret:
// a minted identity so an operator can tell one secret from the next after a rotation.
type WebhookSecret struct {
	Fingerprint string
	CreatedAt   time.Time
	// RotatedAt is zero for a secret that has never been rotated.
	RotatedAt time.Time
}

// Held reports whether this Integration carries a webhook secret at all.
func (w WebhookSecret) Held() bool { return w.Fingerprint != "" }

// Credential is what a read may say about the outbound credential, which is never the
// credential: a minted identity so an operator can tell one pasted token from the next
// after a replacement.
type Credential struct {
	Fingerprint string
	CreatedAt   time.Time
	// RotatedAt is zero for a credential that has never been replaced.
	RotatedAt time.Time
}

// Held reports whether this Integration carries an outbound credential at all.
func (c Credential) Held() bool { return c.Fingerprint != "" }

// Integration is one configured installation belonging to an organization.
type Integration struct {
	ID    uuid.UUID
	OrgID string
	Type  TypeID
	Name  string
	// Configuration is the provider-specific non-secret settings, shaped by the type's
	// schema. It never holds a credential.
	Configuration map[string]any
	Labels        map[string]string
	// RelayID is the installation that serves this Integration, and the zero UUID when
	// none does.
	RelayID uuid.UUID
	// WebhookSecretDigest is the SHA-256 of the shared secret an inbound source presents.
	// Empty for a type that receives no webhooks. The secret itself exists only at
	// creation and rotation and is never read back.
	WebhookSecretDigest []byte
	WebhookSecret       WebhookSecret
	// CredentialSealed is the outbound credential, sealed under the deployment's key.
	// Nil for a type that presents none. It is opened only to be presented to the
	// provider — verification and tool calls — and no view renders it.
	CredentialSealed []byte
	Credential       Credential
	Status           Status
	LastVerifiedAt   time.Time
	VerifyNote       string
	// VerifyGrants are the facts the last verification recorded about the credential
	// (scopes, token kind); tool availability derives from them. Nil when none were
	// recorded.
	VerifyGrants []string
	// VerifyFacts are the non-secret, provider-shaped things the last verification
	// established about what is connected — the account, its type, how far the grant
	// reaches. For display and for support; never consulted by an authorization
	// decision. Nil when none were recorded.
	VerifyFacts map[string]any
	DisabledAt  time.Time
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Disabled reports whether this Integration has been turned off.
func (i Integration) Disabled() bool { return !i.DisabledAt.IsZero() }

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
	ID            uuid.UUID
	Type          TypeID
	Name          string
	Configuration map[string]any
	Labels        map[string]string
	RelayID       uuid.UUID
	// WebhookSecretDigest and Fingerprint are set by the handler for a webhook-receiving
	// type; the secret itself is returned to the operator once and never stored.
	WebhookSecretDigest      []byte
	WebhookSecretFingerprint string
	// CredentialSealed and its fingerprint are set by the handler for a credential-bearing
	// type, after the probe accepted the plaintext and the sealer closed over it.
	CredentialSealed      []byte
	CredentialFingerprint string
	// Verification, when non-nil, is what the pre-creation probe established: the
	// Integration is born verified, in the same transaction that records it. Nil means it
	// is born configured, with nothing having checked it.
	Verification *Verification
	// Installation, when non-nil, is the vendor-side identity an inbound event resolves
	// through, written in the SAME transaction as the row. Nil for every type that
	// receives no events and for a credential pasted into the configuration form, which
	// names no installation to route to.
	Installation *Installation
	CreatedBy    string
}

// AN INSTALLATION IS ROUTING, AND ONLY ROUTING.
//
// It is how an event arriving from a vendor resolves to exactly one Integration and
// therefore exactly one organization. Resolution always runs installation -> integration ->
// organization; no vendor identifier ever looks anything up directly, which is the property
// that stops an identifier from one tenant reaching another's records.
//
// What an operator READS about a connected installation is not here — it is on the
// Integration's verify facts, recorded by the verification. The two are deliberately apart:
// facts are what the last verification established and may go stale, and this is the key
// that decides whose event this is, which may not.
//
// The vocabulary is neutral because the shared connect flow carries it and that flow knows
// no provider. The persistence layer dispatches on the Integration's type to the table that
// holds it.

// Installation is one vendor-side installation, as the routing record holds it.
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
	Agent string
	// Authorizer is who authorized the installation, in the vendor's own identifiers.
	// Recorded for the trail; no decision reads it.
	Authorizer string
	// Grants is what the installation carried when it was made. The authoritative copy for
	// tool availability is the verification's; this is what an operator needs when the two
	// disagree.
	Grants []string
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
	Labels        map[string]string
}

// Page is a position in a listing.
type Page struct {
	Limit int
	After string
}

// Query is what a caller may narrow a listing by. Every field is applied by the database.
type Query struct {
	Page Page
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
