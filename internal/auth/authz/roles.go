package authz

import (
	"slices"
	"strings"
)

type Permission string

const (
	IntegrationRead         Permission = "integration.read"
	IntegrationCreate       Permission = "integration.create"
	IntegrationUpdate       Permission = "integration.update"
	IntegrationDelete       Permission = "integration.delete"
	IntegrationVerify       Permission = "integration.verify"
	IntegrationSecretRotate Permission = "integration.webhook-secret.rotate"

	RelayRead           Permission = "relay.read"
	RelayConflictClear  Permission = "relay.conflict.clear"
	RelayBootstrapIssue Permission = "relay.bootstrap-token.issue"

	IncidentRead    Permission = "incident.read"
	IncidentMerge   Permission = "incident.merge"
	PostmortemRead  Permission = "postmortem.read"
	PostmortemWrite Permission = "postmortem.write"

	InvestigationRead     Permission = "investigation.read"
	InvestigationOpen     Permission = "investigation.open"
	InvestigationCancel   Permission = "investigation.cancel"
	WebhookDeliveryReplay Permission = "webhook-delivery.replay"

	ConversationRead  Permission = "conversation.read"
	ConversationWrite Permission = "conversation.write"

	IdentityRead      Permission = "identity.read"
	IdentityConfigure Permission = "identity.configure"
	MemberRead        Permission = "member.read"
	MemberManage      Permission = "member.manage"

	AuditRead Permission = "audit.read"
)

// allPermissions is every permission this build declares, in a stable order.
var allPermissions = []Permission{
	IntegrationRead,
	IntegrationCreate,
	IntegrationUpdate,
	IntegrationDelete,
	IntegrationVerify,
	IntegrationSecretRotate,
	RelayRead,
	RelayConflictClear,
	RelayBootstrapIssue,
	IncidentRead,
	IncidentMerge,
	PostmortemRead,
	PostmortemWrite,
	InvestigationRead,
	InvestigationOpen,
	InvestigationCancel,
	WebhookDeliveryReplay,
	ConversationRead,
	ConversationWrite,
	IdentityRead,
	IdentityConfigure,
	MemberRead,
	MemberManage,
	AuditRead,
}

func Permissions() []Permission {
	return append([]Permission(nil), allPermissions...)
}

// Declared reports whether a permission is one this build knows. A route requiring anything
// else is a build failure rather than a route nobody can reach.
func Declared(permission Permission) bool {
	return slices.Contains(allPermissions, permission)
}

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

func ReadOnly(permission Permission) bool { return reads[permission] }

// Identity reads are deliberately excluded: who may sign in is the Admin's to see.
var estateReads = []Permission{
	IntegrationRead,
	RelayRead,
	IncidentRead,
	PostmortemRead,
	InvestigationRead,
	ConversationRead,
	AuditRead,
}

// granted is the compact specification of what each Role can do.
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

// Grants reports whether a Role holds a Permission. Unknown Roles grant nothing.
func Grants(role Role, permission Permission) bool { return granted[role][permission] }

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
