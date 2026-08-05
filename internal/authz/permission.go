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
// question a reviewer asks is "what does connection.delete let someone do", not "what is in
// this list".
const (
	// Environments.
	EnvironmentRead   Permission = "environment.read"
	EnvironmentCreate Permission = "environment.create"
	EnvironmentUpdate Permission = "environment.update"
	EnvironmentDelete Permission = "environment.delete"

	// Connections and the Integration catalog they instantiate.
	ConnectionRead   Permission = "connection.read"
	ConnectionCreate Permission = "connection.create"
	// ConnectionUpdate covers setting a Connection's enabled state, which is the only change
	// this build offers. There is deliberately no connection.delete: a Connection is disabled
	// rather than removed, so the record of what a source produced survives, and a permission
	// for an operation the product does not have would read as though it did.
	ConnectionUpdate       Permission = "connection.update"
	ConnectionSecretRotate Permission = "connection.trigger.secret.rotate"
	IntegrationRead        Permission = "integration.read"

	// Relays. Clearing a conflict destroys a credential-theft finding, which is why it is a
	// permission of its own rather than part of reading the roster.
	RelayRead          Permission = "relay.read"
	RelayConflictClear Permission = "relay.conflict.clear"

	// Investigations.
	InvestigationRead   Permission = "investigation.read"
	InvestigationOpen   Permission = "investigation.open"
	InvestigationCancel Permission = "investigation.cancel"
	InvestigationReopen Permission = "investigation.reinvestigate"

	// Identity: who may sign in, as what, and under what policy.
	IdentityRead      Permission = "identity.read"
	IdentityConfigure Permission = "identity.configure"
	MemberRead        Permission = "member.read"
	MemberManage      Permission = "member.manage"
	// MemberOwnerManage is the one permission a Platform administrator does not hold. It is
	// what separates the two administrative roles: an administrator runs the tenant, and an
	// owner decides who else may.
	MemberOwnerManage Permission = "member.owner.manage"
	SessionRevoke     Permission = "session.revoke"

	// Automation.
	ServiceAccountRead   Permission = "service-account.read"
	ServiceAccountManage Permission = "service-account.manage"
	TokenRead            Permission = "api-token.read"
	TokenManage          Permission = "api-token.manage"

	// The record.
	AuditRead Permission = "audit.read"
)

// allPermissions is every permission this build declares, in a stable order. It is the set the
// route gate validates against and the set an owner holds.
var allPermissions = []Permission{
	EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete,
	ConnectionRead, ConnectionCreate, ConnectionUpdate, ConnectionSecretRotate, IntegrationRead,
	RelayRead, RelayConflictClear,
	InvestigationRead, InvestigationOpen, InvestigationCancel, InvestigationReopen,
	IdentityRead, IdentityConfigure, MemberRead, MemberManage, MemberOwnerManage, SessionRevoke,
	ServiceAccountRead, ServiceAccountManage, TokenRead, TokenManage,
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
