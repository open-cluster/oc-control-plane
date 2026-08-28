package authz

import "slices"

type Permission string

const (
	// ========= Integrations =========
	IntegrationRead         Permission = "integration.read"
	IntegrationCreate       Permission = "integration.create"
	IntegrationUpdate       Permission = "integration.update"
	IntegrationDelete       Permission = "integration.delete"
	IntegrationVerify       Permission = "integration.verify"
	IntegrationSecretRotate Permission = "integration.webhook-secret.rotate"

	// ========= Relays =========
	RelayRead           Permission = "relay.read"
	RelayConflictClear  Permission = "relay.conflict.clear"
	RelayBootstrapIssue Permission = "relay.bootstrap-token.issue"

	// ========= Incidents =========
	IncidentRead    Permission = "incident.read"
	IncidentMerge   Permission = "incident.merge"
	PostmortemRead  Permission = "postmortem.read"
	PostmortemWrite Permission = "postmortem.write"

	// ========= Investigations =========
	InvestigationRead     Permission = "investigation.read"
	InvestigationOpen     Permission = "investigation.open"
	InvestigationCancel   Permission = "investigation.cancel"
	WebhookDeliveryReplay Permission = "webhook-delivery.replay"

	// ========= Conversations =========
	ConversationRead  Permission = "conversation.read"
	ConversationWrite Permission = "conversation.write"

	// ========= Identity =========
	IdentityRead      Permission = "identity.read"
	IdentityConfigure Permission = "identity.configure"
	MemberRead        Permission = "member.read"
	MemberManage      Permission = "member.manage"
	SessionRevoke     Permission = "session.revoke"

	// ========= Audit =========
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
	SessionRevoke,
	AuditRead,
}

// Permissions returns every permission this build declares.
func Permissions() []Permission {
	return append([]Permission(nil), allPermissions...)
}

// Declared reports whether a permission is one this build knows. A route requiring anything
// else is a build failure rather than a route nobody can reach.
func Declared(permission Permission) bool {
	return slices.Contains(allPermissions, permission)
}
