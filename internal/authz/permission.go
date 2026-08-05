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
	// ConnectionUpdate covers setting a Connection's enabled state and revising its
	// configuration.
	ConnectionUpdate Permission = "connection.update"
	// ConnectionDelete is NARROW and stays narrow. A Connection that anything has delivered
	// through, read through, or that is its Environment's last evidence source, is refused —
	// the record of what a source produced must survive, which is why disabling exists. What
	// this covers is the Connection created by mistake five minutes ago, which until now had to
	// be carried forever because the only alternative was destroying history.
	ConnectionDelete Permission = "connection.delete"
	// ConnectionValidate exercises a Connection against the system at the far end. It is a
	// mutation rather than a read: it writes a result, moves the Connection's state, and is the
	// difference between a Connection that is configured and one that is known to work.
	ConnectionValidate     Permission = "connection.validate"
	ConnectionSecretRotate Permission = "connection.trigger.secret.rotate"
	IntegrationRead        Permission = "integration.read"

	// Relays. Clearing a conflict destroys a credential-theft finding, which is why it is a
	// permission of its own rather than part of reading the roster.
	RelayRead          Permission = "relay.read"
	RelayConflictClear Permission = "relay.conflict.clear"
	// RelayBootstrapIssue mints a credential that enrols a new Relay into the tenant. It is
	// separate from reading the fleet because it is the one relay operation that CREATES the
	// ability to join it, and a role that may look at the estate should not by that fact be able
	// to extend it.
	RelayBootstrapIssue Permission = "relay.bootstrap-token.issue"

	// Incidents: the operational episode Signals group into, and the correction of a grouping.
	// Reading one is what everybody looking at the tenant does. MERGING decides what an incident
	// is about — and so what an Investigation opened for it would be scoped to — which is why it
	// is a permission of its own rather than part of reading.
	IncidentRead  Permission = "incident.read"
	IncidentMerge Permission = "incident.merge"

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

	// A directory synchronising itself into a tenant. It is one permission rather than
	// member.manage because the two are different jobs: an administrator decides who may be in
	// this tenant, and a directory reports who is in the company. A credential sitting in a
	// customer's identity vendor should be able to do the second and not the first.
	DirectorySync Permission = "directory.sync"

	// The record.
	AuditRead Permission = "audit.read"
)

// allPermissions is every permission this build declares, in a stable order. It is the set the
// route gate validates against and the set an owner holds.
var allPermissions = []Permission{
	EnvironmentRead, EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete,
	ConnectionRead, ConnectionCreate, ConnectionUpdate, ConnectionDelete, ConnectionValidate,
	ConnectionSecretRotate, IntegrationRead,
	RelayRead, RelayConflictClear, RelayBootstrapIssue,
	IncidentRead, IncidentMerge,
	InvestigationRead, InvestigationOpen, InvestigationCancel, InvestigationReopen,
	IdentityRead, IdentityConfigure, MemberRead, MemberManage, MemberOwnerManage, SessionRevoke,
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
