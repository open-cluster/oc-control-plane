package authz

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

const maxIdentifierLength = 256

var ErrInvalidPrincipal = errors.New("invalid principal")
var ErrNotAMember = errors.New("principal holds no membership in this organization")

type Principal struct {
	userID        uuid.UUID
	sessionID     uuid.UUID
	displayName   string
	membership    Membership
	sourceAddress string
	requestID     string
	email         string
}

type Membership struct {
	Organization uuid.UUID
	DisplayName  string
	Role         Role
}

func NewPrincipal(
	userID, sessionID uuid.UUID, displayName, email string, membership Membership,
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
		email:       strings.TrimSpace(email),
		membership:  membership,
	}, nil
}

func (p Principal) WithRequest(sourceAddress, requestID string) Principal {
	p.sourceAddress, p.requestID = sourceAddress, requestID
	return p
}

func (p Principal) IsEmpty() bool { return p.userID == uuid.Nil }

func (p Principal) UserID() uuid.UUID { return p.userID }

func (p Principal) SessionID() uuid.UUID { return p.sessionID }

func (p Principal) Email() string { return p.email }

func (p Principal) DisplayName() string { return p.displayName }

func (p Principal) SourceAddress() string { return p.sourceAddress }

func (p Principal) RequestID() string { return p.requestID }

func (p Principal) Organization() uuid.UUID { return p.membership.Organization }

func (p Principal) OrganizationName() string { return p.membership.DisplayName }

func (p Principal) Role() Role { return p.membership.Role }

func (p Principal) HavePermission(permission Permission) bool {
	return p.membership.Role.Grants(permission)
}

func (p Principal) Actor() audit.Actor {
	return audit.Actor{
		Kind:        audit.ActorUser,
		ID:          p.userID.String(),
		DisplayName: p.displayName,
	}
}
