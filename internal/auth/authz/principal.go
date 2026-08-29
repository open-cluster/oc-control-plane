// Package authz owns who a caller is and what they may do.
//
// WHO: a Principal, built with the same discipline tenancy.Organization is built with —
// unexported fields, one constructor, validation inside it — so a store function can trust the
// value it receives.
//
// WHAT THEY MAY DO: the permission vocabulary, the seven roles as a compiled table, the route
// table every route on the operator surface registers in as (method, pattern, permission), and
// the one middleware that reads it. Router builds the mux from that table, so the decision is
// made in a single place — a second copy of an authorization decision is a second place for it
// to be wrong.
package authz

import (
	"errors"
	"fmt"
	"slices"
	"strings"

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

// Principal is who is making a request, and which Organizations they hold a role in.
type Principal struct {
	kind          Kind
	id            string
	displayName   string
	memberships   map[string]Membership
	credentialID  string
	sourceAddress string
	requestID     string
}

// Membership is one organization and the role held in it.
type Membership struct {
	ID           string
	Organization tenancy.Organization
	DisplayName  string
	Role         Role
}

func NewPrincipal(
	kind Kind, id, displayName string, memberships []Membership,
) (Principal, error) {
	switch kind {
	case KindUser:
		if strings.TrimSpace(id) == "" {
			return Principal{}, fmt.Errorf("%w: a %s must have an identifier",
				ErrInvalidPrincipal, audit.ActorKind(kind))
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

	held := make(map[string]Membership, len(memberships))
	for _, membership := range memberships {
		if membership.Organization.IsEmpty() || !KnownRole(membership.Role) {
			continue
		}
		held[membership.Organization.String()] = membership
	}
	return Principal{
		kind:        kind,
		id:          strings.TrimSpace(id),
		displayName: strings.TrimSpace(displayName),
		memberships: held,
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

// RoleIn reports the role this principal holds in an organization.
func (p Principal) RoleIn(organization tenancy.Organization) (Role, bool) {
	membership, member := p.memberships[organization.String()]
	return membership.Role, member
}

// MemberOf reports whether this principal holds any role in an organization.
func (p Principal) MemberOf(organization tenancy.Organization) bool {
	_, member := p.memberships[organization.String()]
	return member
}

// CanDo reports whether this principal may perform something in an organization.
func (p Principal) CanDo(organization tenancy.Organization, permission Permission) bool {
	role, member := p.RoleIn(organization)
	return member && role.Grants(permission)
}

// Memberships reports every organization this principal holds a role in, sorted by identifier
// so that the session response is stable across requests.
func (p Principal) Memberships() []Membership {
	names := make([]string, 0, len(p.memberships))
	for name := range p.memberships {
		names = append(names, name)
	}
	slices.Sort(names)

	listed := make([]Membership, 0, len(names))
	for _, name := range names {
		listed = append(listed, p.memberships[name])
	}
	return listed
}

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
