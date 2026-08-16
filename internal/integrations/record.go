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

// Refusals a mutation can produce. Declared here because the capability owns its
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
	Status              Status
	LastVerifiedAt      time.Time
	VerifyNote          string
	DisabledAt          time.Time
	CreatedBy           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Disabled reports whether this Integration has been turned off.
func (i Integration) Disabled() bool { return !i.DisabledAt.IsZero() }

// NewIntegration is what an operator asked for. It travels as one value because the fields
// constrain each other: a relay-served type needs a Relay, a webhook-receiving type needs a
// secret digest, and validating them apart would let an invalid combination reach the
// database and come back as a constraint violation rather than an answer.
type NewIntegration struct {
	Type          TypeID
	Name          string
	Configuration map[string]any
	Labels        map[string]string
	RelayID       uuid.UUID
	// WebhookSecretDigest and Fingerprint are set by the handler for a webhook-receiving
	// type; the secret itself is returned to the operator once and never stored.
	WebhookSecretDigest      []byte
	WebhookSecretFingerprint string
	CreatedBy                string
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
