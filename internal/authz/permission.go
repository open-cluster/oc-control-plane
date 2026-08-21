package authz

// Permission is one verb-noun capability a route requires. The vocabulary is closed and lives
// here: a route may only require a permission this file declares, and the gate in
// internal/gates refuses one that does not.
//
// It is a string rather than an integer because it is never persisted — a role's permissions
// are compiled, not stored — and because a 403 that names what was missing is worth more to an
// operator than one that names a number.
type Permission string

// The permissions this build knows. Grouped by the noun rather than by the role, because the
// question a reviewer asks is "what does integration.delete let someone do", not "what is in
// this list".
const (
	// Integrations: the catalog of types and the configured installations.
	IntegrationRead   Permission = "integration.read"
	IntegrationCreate Permission = "integration.create"
	// IntegrationUpdate covers renaming, revising configuration and labels, and setting
	// the enabled state.
	IntegrationUpdate Permission = "integration.update"
	// IntegrationDelete is NARROW and stays narrow. An Integration that anything has
	// delivered through or been read through is refused — the record of what a source
	// produced must survive, which is why disabling exists. What this covers is the
	// Integration created by mistake five minutes ago.
	IntegrationDelete Permission = "integration.delete"
	// IntegrationVerify exercises an Integration against the system at the far end. It is
	// a mutation rather than a read: it writes a result and moves the Integration's
	// status, and is the difference between configured and known to work.
	IntegrationVerify       Permission = "integration.verify"
	IntegrationSecretRotate Permission = "integration.webhook-secret.rotate"

	// Relays. Clearing a conflict destroys a credential-theft finding, which is why it is a
	// permission of its own rather than part of reading the roster.
	RelayRead          Permission = "relay.read"
	RelayConflictClear Permission = "relay.conflict.clear"
	// RelayBootstrapIssue mints a credential that enrols a new Relay into the tenant. It is
	// separate from reading the fleet because it is the one relay operation that CREATES the
	// ability to join it.
	RelayBootstrapIssue Permission = "relay.bootstrap-token.issue"

	// Incidents: the operational episode Signals group into. MERGING decides what an
	// incident is about, which is why it is a permission of its own rather than part of
	// reading.
	IncidentRead  Permission = "incident.read"
	IncidentMerge Permission = "incident.merge"

	// Investigations. Opening one spends model budget and reads connected systems, which
	// is why it is not part of reading the record it produces.
	InvestigationRead Permission = "investigation.read"
	InvestigationOpen Permission = "investigation.open"

	// Conversations: the multi-turn context a person talks to. Writing covers opening one
	// and sending a message to one, because both do the same thing — a message opens a
	// turn, and a turn is an investigation. It is granted exactly where investigation.open
	// is, since anyone who may spend the budget once may spend it twice in a row.
	ConversationRead  Permission = "conversation.read"
	ConversationWrite Permission = "conversation.write"

	// Identity: who may sign in, as what, and under what policy.
	IdentityRead      Permission = "identity.read"
	IdentityConfigure Permission = "identity.configure"
	MemberRead        Permission = "member.read"
	MemberManage      Permission = "member.manage"
	SessionRevoke     Permission = "session.revoke"

	// Automation.
	ServiceAccountRead   Permission = "service-account.read"
	ServiceAccountManage Permission = "service-account.manage"
	TokenRead            Permission = "api-token.read"
	TokenManage          Permission = "api-token.manage"

	// A directory synchronising itself into a tenant. It is one permission rather than
	// member.manage because the two are different jobs: an administrator decides who may be
	// in this tenant, and a directory reports who is in the company.
	DirectorySync Permission = "directory.sync"

	// The record.
	AuditRead Permission = "audit.read"
)

// allPermissions is every permission this build declares, in a stable order. It is the set the
// route gate validates against and the set an Admin holds.
var allPermissions = []Permission{
	IntegrationRead, IntegrationCreate, IntegrationUpdate, IntegrationDelete,
	IntegrationVerify, IntegrationSecretRotate,
	RelayRead, RelayConflictClear, RelayBootstrapIssue,
	IncidentRead, IncidentMerge,
	InvestigationRead, InvestigationOpen,
	ConversationRead, ConversationWrite,
	IdentityRead, IdentityConfigure, MemberRead, MemberManage, SessionRevoke,
	ServiceAccountRead, ServiceAccountManage, TokenRead, TokenManage,
	DirectorySync,
	AuditRead,
}

// Permissions returns every permission this build declares.
func Permissions() []Permission {
	return append([]Permission(nil), allPermissions...)
}

// Declared reports whether a permission is one this build knows. A route requiring anything
// else is a build failure rather than a route nobody can reach.
func Declared(permission Permission) bool {
	for _, known := range allPermissions {
		if known == permission {
			return true
		}
	}
	return false
}
