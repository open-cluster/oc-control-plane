// Package authz owns who a caller is and what they may do.
//
// It exists because the operator surface had one shared static token and was cross-tenant by
// design, so whoever held it could read and mutate any Organization by editing a URL path
// segment, and the record could say where a claim came from and never who made it.
//
// Two subjects live here and nothing else, and the files divide along them.
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
//
// The package performs no I/O and knows nothing about how a credential is resolved. Whoever
// assembles the surface supplies that as a function, which is what keeps the session store,
// the API token store and this decision independent of each other.
package authz

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

// maxIdentifierLength bounds a principal's identifier and display name. Both arrive from an
// external identity provider, so both are somebody else's strings.
const maxIdentifierLength = 256

// ErrInvalidPrincipal reports a principal that cannot be constructed.
var ErrInvalidPrincipal = errors.New("invalid principal")

// ErrNotAMember reports a principal with no membership in the organization a call named.
//
// It lives here rather than in internal/store/postgres because the membership is this package's
// concept and the capabilities that have to recognise it must not import persistence to do so
// (ADR-017). Storage returns it; a handler maps it to 404 and never to 403.
var ErrNotAMember = errors.New("principal holds no membership in this organization")

// Kind is what sort of party is acting. It mirrors audit.ActorKind because they are the same
// question asked at two moments — at the door and in the record — and letting them drift would
// mean an event whose actor kind disagrees with the credential that produced it.
type Kind int16

const (
	KindUser   Kind = Kind(audit.ActorUser)
	KindSystem Kind = Kind(audit.ActorSystem)
)

// Principal is who is making a request, and which Organizations they hold a role in.
//
// The fields are unexported for the reason tenancy.Organization's are: a Principal cannot be
// constructed by literal, so a store function handed one may trust that its memberships came
// from the database on this request rather than from whatever a handler assembled.
//
// Memberships are resolved per request rather than carried in the credential. That is what
// makes story 10 true — an administrator revoking a colleague's membership takes effect on the
// colleague's next request, not at their next sign-in.
type Principal struct {
	kind Kind
	// id is the user identifier. Empty only for the system principal.
	id string
	// displayName is what the record will call this actor. For a person it is the name their
	// identity provider gave.
	displayName string
	// memberships maps an organization identifier to the role held in it. A principal with no
	// entry for an organization is not a member of it, and every route naming that
	// organization answers 404 — not 403, which would confirm the tenant exists.
	memberships map[string]Role
	// credentialID names the session this request presented, so a revocation can be traced.
	credentialID string
	// sourceAddress and requestID travel with the principal because every audit event needs
	// them and threading them separately through every handler is how one gets forgotten.
	sourceAddress string
	requestID     string
}

// Membership is one organization and the role held in it.
type Membership struct {
	Organization tenancy.Organization
	Role         Role
}

// NewPrincipal builds a principal from what a credential resolved to.
//
// An unrecognised role in a membership is DROPPED rather than refused. The value arrives from
// a database column and from an identity provider's group map, and refusing the whole sign-in
// because one group mapped to a role this build no longer has would turn a rename into an
// outage. A dropped membership is a principal who is not a member of that organization, which
// answers 404 — a safe direction to fail.
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

	held := make(map[string]Role, len(memberships))
	for _, membership := range memberships {
		if membership.Organization.IsEmpty() || !KnownRole(membership.Role) {
			continue
		}
		held[membership.Organization.String()] = membership.Role
	}
	return Principal{
		kind:        kind,
		id:          strings.TrimSpace(id),
		displayName: strings.TrimSpace(displayName),
		memberships: held,
	}, nil
}

// SystemPrincipal is the control plane acting on its own behalf: a worker, a sweeper, a
// migration. It holds no membership and therefore reaches no route — it exists so that work
// the product does for itself has an actor in the record rather than an empty column.
func SystemPrincipal(what string) Principal {
	return Principal{kind: KindSystem, displayName: what}
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

// RoleIn reports the role this principal holds in an organization, and whether they hold one
// at all. A principal who holds none is not a member, and the difference between that and an
// organization that does not exist is not one this API will tell anybody.
func (p Principal) RoleIn(organization tenancy.Organization) (Role, bool) {
	role, member := p.memberships[organization.String()]
	return role, member
}

// MemberOf reports whether this principal holds any role in an organization.
func (p Principal) MemberOf(organization tenancy.Organization) bool {
	_, member := p.memberships[organization.String()]
	return member
}

// Can reports whether this principal may perform something in an organization. It is the whole
// decision in one call: not a member and not permitted are both false here, and the caller
// decides which status says so.
func (p Principal) Can(organization tenancy.Organization, permission Permission) bool {
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
		organization, err := tenancy.NewOrganization(name)
		if err != nil {
			continue
		}
		listed = append(listed, Membership{Organization: organization, Role: p.memberships[name]})
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
