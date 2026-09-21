package authz

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// maxIdentifierLength bounds a principal's identifier and display name.
const maxIdentifierLength = 256

var ErrInvalidPrincipal = errors.New("invalid principal")
var ErrNotAMember = errors.New("principal holds no membership in this organization")

type Kind int16

const (
	KindUser   Kind = Kind(audit.ActorUser)
	KindSystem Kind = Kind(audit.ActorSystem)
)

// Principal is the complete identity and tenant boundary for one authenticated request.
type Principal struct {
	kind          Kind
	id            string
	displayName   string
	membership    Membership
	credentialID  string
	sourceAddress string
	requestID     string
	sessionInfo   SessionInfo
}

// SessionInfo contains identity metadata verified during request authentication.
type SessionInfo struct {
	Email                string
	AuthenticationMethod string
	ExpiresAt            time.Time
}

func (p Principal) WithSessionInfo(info SessionInfo) Principal {
	p.sessionInfo = info
	return p
}

func (p Principal) SessionInfo() SessionInfo { return p.sessionInfo }

// Membership is one organization and the role held in it.
type Membership struct {
	Organization tenancy.Organization
	DisplayName  string
	Role         Role
}

func NewPrincipal(
	kind Kind, id, displayName string, membership Membership,
) (Principal, error) {
	switch kind {
	case KindUser:
		if strings.TrimSpace(id) == "" {
			return Principal{}, fmt.Errorf("%w: a %s must have an identifier",
				ErrInvalidPrincipal, audit.ActorKind(kind))
		}
		if membership.Organization.IsEmpty() || !KnownRole(membership.Role) {
			return Principal{}, fmt.Errorf("%w: a user must have one Organization and Role",
				ErrInvalidPrincipal)
		}
	case KindSystem:
	default:
		return Principal{}, fmt.Errorf("%w: %d is not a kind of principal",
			ErrInvalidPrincipal, kind)
	}
	if len(id) > maxIdentifierLength || len(displayName) > maxIdentifierLength {
		return Principal{}, fmt.Errorf("%w: the identifier and display name must be at most "+
			"%d bytes", ErrInvalidPrincipal, maxIdentifierLength)
	}

	return Principal{
		kind:        kind,
		id:          strings.TrimSpace(id),
		displayName: strings.TrimSpace(displayName),
		membership:  membership,
	}, nil
}

// WithCredential records which session or token this request presented.
func (p Principal) WithCredential(id string) Principal {
	p.credentialID = id
	return p
}

// WithRequest records where the request came from and what it is called in the logs, so every
// event this principal produces can name both without each handler threading them.
func (p Principal) WithRequest(sourceAddress, requestID string) Principal {
	p.sourceAddress, p.requestID = sourceAddress, requestID
	return p
}

// IsZero reports the principal nobody resolved. It reaches nothing.
func (p Principal) IsZero() bool { return p.kind == 0 }

// Kind reports what sort of party this is.
func (p Principal) Kind() Kind { return p.kind }

// ID is the user or service account identifier.
func (p Principal) ID() string { return p.id }

// DisplayName is what the record will call this actor.
func (p Principal) DisplayName() string { return p.displayName }

// CredentialID names the session or API token this request presented.
func (p Principal) CredentialID() string { return p.credentialID }

// SourceAddress is where the request came from.
func (p Principal) SourceAddress() string { return p.sourceAddress }

// RequestID ties this principal's events to the log lines for the same request.
func (p Principal) RequestID() string { return p.requestID }

// Organization is the sole tenant resolved during authentication.
func (p Principal) Organization() tenancy.Organization { return p.membership.Organization }

// OrganizationDisplayName is the current Organization's human-facing name.
func (p Principal) OrganizationDisplayName() string { return p.membership.DisplayName }

// Role is the current Role resolved during authentication.
func (p Principal) Role() Role { return p.membership.Role }

// Can reports whether the current Role grants a Permission.
func (p Principal) Can(permission Permission) bool { return p.membership.Role.Grants(permission) }

// Actor is how this principal appears in the record. The display name is copied here rather
// than joined at read time, so renaming or deleting a user never rewrites what the record says
// about what they did.
func (p Principal) Actor() audit.Actor {
	return audit.Actor{
		Kind:        audit.ActorKind(p.kind),
		ID:          p.id,
		DisplayName: p.displayName,
	}
}
