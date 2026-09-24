package authz

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

const maxIdentifierLength = 256

var ErrInvalidPrincipal = errors.New("invalid principal")
var ErrNotAMember = errors.New("principal holds no membership in this organization")

type Principal struct {
	userID               uuid.UUID
	sessionID            uuid.UUID
	displayName          string
	membership           Membership
	sourceAddress        string
	requestID            string
	email                string
	authenticationMethod string
	expiresAt            time.Time
}

func (p Principal) WithSessionPresentation(
	email, authenticationMethod string, expiresAt time.Time,
) Principal {
	p.email = email
	p.authenticationMethod = authenticationMethod
	p.expiresAt = expiresAt
	return p
}

// Membership is one organization and the role held in it.
type Membership struct {
	Organization uuid.UUID
	DisplayName  string
	Role         Role
}

func NewPrincipal(
	userID, sessionID uuid.UUID, displayName string, membership Membership,
) (Principal, error) {
	if userID == uuid.Nil || sessionID == uuid.Nil {
		return Principal{}, fmt.Errorf("%w: a user and session must have identifiers", ErrInvalidPrincipal)
	}
	if membership.Organization == uuid.Nil || !KnownRole(membership.Role) {
		return Principal{}, fmt.Errorf("%w: a user must have one Organization and Role",
			ErrInvalidPrincipal)
	}
	if len(displayName) > maxIdentifierLength {
		return Principal{}, fmt.Errorf("%w: the display name must be at most %d bytes",
			ErrInvalidPrincipal, maxIdentifierLength)
	}

	return Principal{
		userID:      userID,
		sessionID:   sessionID,
		displayName: strings.TrimSpace(displayName),
		membership:  membership,
	}, nil
}

// WithRequest records where the request came from and what it is called in the logs, so every
// event this principal produces can name both without each handler threading them.
func (p Principal) WithRequest(sourceAddress, requestID string) Principal {
	p.sourceAddress, p.requestID = sourceAddress, requestID
	return p
}

// IsZero reports the principal nobody resolved. It reaches nothing.
func (p Principal) IsZero() bool { return p.userID == uuid.Nil }

func (p Principal) UserID() uuid.UUID { return p.userID }

func (p Principal) SessionID() uuid.UUID { return p.sessionID }

func (p Principal) Email() string { return p.email }

func (p Principal) AuthenticationMethod() string { return p.authenticationMethod }

// ExpiresAt is when the browser session stops authenticating requests.
func (p Principal) ExpiresAt() time.Time { return p.expiresAt }

func (p Principal) DisplayName() string { return p.displayName }

func (p Principal) SourceAddress() string { return p.sourceAddress }

// RequestID ties this principal's events to the log lines for the same request.
func (p Principal) RequestID() string { return p.requestID }

// Organization is the sole tenant resolved during authentication.
func (p Principal) Organization() uuid.UUID { return p.membership.Organization }

func (p Principal) OrganizationName() string { return p.membership.DisplayName }

func (p Principal) Role() Role { return p.membership.Role }

func (p Principal) Can(permission Permission) bool { return p.membership.Role.Grants(permission) }

// Actor is how this principal appears in the record. The display name is copied here rather
// than joined at read time, so renaming or deleting a user never rewrites what the record says
// about what they did.
func (p Principal) Actor() audit.Actor {
	return audit.Actor{
		Kind:        audit.ActorUser,
		ID:          p.userID.String(),
		DisplayName: p.displayName,
	}
}
