package authz

import "slices"

import "strings"

type Role string

const (
	Admin  Role = "admin"
	Editor Role = "editor"
	Viewer Role = "viewer"
)

var roles = []Role{Admin, Editor, Viewer}

func Roles() []Role { return append([]Role(nil), roles...) }

func KnownRole(role Role) bool {
	return slices.Contains(roles, role)
}

func ParseRole(value string) (Role, bool) {
	role := Role(strings.TrimSpace(value))
	return role, KnownRole(role)
}

var reads = map[Permission]bool{
	IntegrationRead:   true,
	RelayRead:         true,
	IncidentRead:      true,
	PostmortemRead:    true,
	InvestigationRead: true,
	ConversationRead:  true,
	IdentityRead:      true,
	MemberRead:        true,
	AuditRead:         true,
}

// ReadOnly reports whether holding a permission can change anything. It is what the viewer
// test asserts against, so a mutating permission added to the read-only role fails the
// build.
func ReadOnly(permission Permission) bool { return reads[permission] }

// estateReads is what a role that may look at the tenant's operational record holds: the
// integrations, the fleet, the incidents, the investigations, and what happened. Identity
// and automation reads are deliberately not here — who may sign in is the Admin's to see.
var estateReads = []Permission{
	IntegrationRead,
	RelayRead,
	IncidentRead,
	PostmortemRead,
	InvestigationRead,
	ConversationRead,
	AuditRead,
}

// granted is the table. It is the specification of each role, and it is the thing to read
// when answering "what can an Editor do" — not the handlers.
var granted = map[Role]map[Permission]bool{
	Admin: setOf(allPermissions...),

	Editor: setOf(append(append([]Permission(nil), estateReads...),
		IntegrationVerify,
		IntegrationUpdate,
		IncidentMerge,
		InvestigationOpen,
		InvestigationCancel,
		ConversationWrite,
		PostmortemWrite,
	)...),

	Viewer: setOf(estateReads...),
}

// Grants reports whether this role holds a permission. An unrecognised role grants
// nothing, which is what makes a corrupted column or an unmapped identity-provider group
// safe.
func Grants(role Role, permission Permission) bool { return granted[role][permission] }

// Grants reports whether this role holds a permission.
func (r Role) Grants(permission Permission) bool { return Grants(r, permission) }

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
