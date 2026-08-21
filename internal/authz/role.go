package authz

import "strings"

// Role is a named, fixed set of permissions.
//
// Three human roles and one machine role exist, and they are compiled rather than
// editable. Custom roles are deliberately out of scope: an editable role is a second
// authorization model to review, and the first question a design partner asks is what the
// shipped ones can do. The value is persisted as text in organization_membership.role and
// may arrive from an identity provider's group map, so an unrecognised one has to be inert
// rather than an error nobody handles.
type Role string

const (
	// Admin runs the tenant: identity configuration, members, sessions, tokens,
	// integrations, relays and the record.
	Admin Role = "admin"
	// Editor operates the estate during an incident — verifying integrations, turning one
	// off and back on, correcting an incident grouping — and cannot change who may sign in
	// or extend the estate.
	Editor Role = "editor"
	// Viewer is read-only across the tenant's operational record.
	Viewer Role = "viewer"
	// DirectorySynchroniser is what a customer's directory holds. It reaches the
	// provisioning endpoints and nothing else: the credential lives somewhere this product
	// does not control, and a token that could do more would be a token worth stealing for
	// something other than what it is for.
	DirectorySynchroniser Role = "directory_synchroniser"
)

// roles is the declared set, in the order the product presents them: most privileged
// first, so a list rendered from this reads as a ladder.
var roles = []Role{Admin, Editor, Viewer, DirectorySynchroniser}

// Roles returns the roles this build declares.
func Roles() []Role { return append([]Role(nil), roles...) }

// KnownRole reports whether a value names a role this build has.
func KnownRole(role Role) bool {
	for _, known := range roles {
		if known == role {
			return true
		}
	}
	return false
}

// ParseRole resolves what a database column or a group map said. An unrecognised value is
// refused rather than defaulted: defaulting up is a privilege escalation and defaulting
// down is a silent lockout, and both are worse than saying the value is not a role.
func ParseRole(value string) (Role, bool) {
	role := Role(strings.TrimSpace(value))
	return role, KnownRole(role)
}

// reads are the permissions that change nothing. The set is declared rather than derived
// from the name, because "integration.webhook-secret.rotate" reads like a noun and is a
// mutation.
var reads = map[Permission]bool{
	IntegrationRead:    true,
	RelayRead:          true,
	IncidentRead:       true,
	InvestigationRead:  true,
	ConversationRead:   true,
	IdentityRead:       true,
	MemberRead:         true,
	ServiceAccountRead: true,
	TokenRead:          true,
	AuditRead:          true,
}

// ReadOnly reports whether holding a permission can change anything. It is what the viewer
// test asserts against, so a mutating permission added to the read-only role fails the
// build.
func ReadOnly(permission Permission) bool { return reads[permission] }

// estateReads is what a role that may look at the tenant's operational record holds: the
// integrations, the fleet, the incidents, the investigations, and what happened. Identity
// and automation reads are deliberately not here — who may sign in is the Admin's to see.
var estateReads = []Permission{
	IntegrationRead, RelayRead, IncidentRead, InvestigationRead, ConversationRead,
	AuditRead,
}

// granted is the table. It is the specification of each role, and it is the thing to read
// when answering "what can an Editor do" — not the handlers.
var granted = map[Role]map[Permission]bool{
	Admin: setOf(allPermissions...),

	// Operating the estate during an incident. Verifying is here because "is this
	// integration actually working, or have we been reasoning from a source that stopped
	// answering an hour ago" is a question an Editor has at three in the morning, and
	// making them wake an Admin to ask it is the kind of gap that gets worked around with
	// a shared credential. Updating is here for the same incident: turning an integration
	// off is the remediation this surface actually offers. Opening an investigation is
	// here because incidents are when investigations happen. Creating, deleting and
	// secret rotation stay with the Admin — those change what the estate IS.
	Editor: setOf(append(append([]Permission(nil), estateReads...),
		IntegrationVerify, IntegrationUpdate, IncidentMerge, InvestigationOpen,
		ConversationWrite,
	)...),

	Viewer: setOf(estateReads...),

	// One permission. A directory reports who is in the company; it does not read this
	// tenant's estate or its record.
	DirectorySynchroniser: setOf(DirectorySync),
}

// Grants reports whether this role holds a permission. An unrecognised role grants
// nothing, which is what makes a corrupted column or an unmapped identity-provider group
// safe.
func Grants(role Role, permission Permission) bool { return granted[role][permission] }

// Grants reports whether this role holds a permission.
func (r Role) Grants(permission Permission) bool { return Grants(r, permission) }

// PermissionsOf returns what a role holds, in the declared order, so a surface that lists
// a role's permissions renders the same list every time.
func PermissionsOf(role Role) []Permission {
	held := granted[role]
	listed := make([]Permission, 0, len(held))
	for _, permission := range allPermissions {
		if held[permission] {
			listed = append(listed, permission)
		}
	}
	return listed
}

func setOf(permissions ...Permission) map[Permission]bool {
	set := make(map[Permission]bool, len(permissions))
	for _, permission := range permissions {
		set[permission] = true
	}
	return set
}
