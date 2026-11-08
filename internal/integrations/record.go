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
	ErrConflict           = errors.New("integration access changed; reload and try again")
	ErrCredentialRequired = errors.New("changed credential-use settings require a replacement credential")
	ErrUnknown            = errors.New("integration unknown")
	ErrCrossTenant        = errors.New("integration names something outside its organization")
	ErrDisconnected       = errors.New("integration is disconnected; create a new integration to connect again")
	ErrBadCursor          = errors.New("after is not a page position from a previous response")
	ErrInstallationTaken  = errors.New(
		"another integration in this deployment already owns that provider installation")
	ErrInvalidInstallation = errors.New("installation cannot be recorded")
)

type Integration struct {
	ID                  uuid.UUID
	OrgID               string
	Provider            Provider
	Name                string
	Configuration       map[string]any
	RelayID             uuid.UUID
	WebhookSecretDigest []byte
	CredentialSealed    []byte
	Status              Status
	VerifiedAt          time.Time
	VerificationGrants  []string
	Disconnected        bool
	CreatedAt           time.Time
	Installation        *Installation
}

func CredentialBinding(id uuid.UUID) []byte { return id[:] }

type NewIntegration struct {
	ID                  uuid.UUID
	Provider            Provider
	Name                string
	Configuration       map[string]any
	RelayID             uuid.UUID
	WebhookSecretDigest []byte
	CredentialSealed    []byte
	Verification        *Verification
	Installation        *Installation
}

type Installation struct {
	Key             InstallationKey `json:"key"`
	ProviderActorID string          `json:"providerActorId,omitempty"`
}

type InstallationKey []string

func (k InstallationKey) Complete() bool {
	if len(k) == 0 {
		return false
	}
	return !slices.Contains(k, "")
}

type IntegrationUpdate struct {
	Name          *string        `json:"name"`
	Configuration map[string]any `json:"configuration"`
}

type Page struct {
	Limit int
	After string
}

type Query struct {
	Page       Page
	Sort       string
	Descending bool
	Provider   Provider
	Relay      uuid.UUID
	Search     string
}

type List struct {
	Integrations []Integration
	Next         string
}

type ProviderCount struct {
	Provider Provider
	Count    int
}
