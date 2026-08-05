package authz

import "strings"

// Role is a named, fixed set of permissions.
//
// Eight exist and they are compiled rather than editable. The specification proposed seven; the
// eighth — DirectorySynchroniser — arrived with SCIM and is recorded there with its reason. In
// short: a directory's credential lives in a customer's identity vendor, and the alternative
// was issuing it a Platform administrator token, which is a far worse thing to leave in an
// integration's configuration than one narrow role is a departure from a list. Custom roles are deliberately out of
// scope for this release: an editable role is a second authorization model to review, and the
// first question a design partner asks is what the shipped ones can do. The value is persisted
// as text in organization_membership.role and arrives from an identity provider's group map,
// so an unrecognised one has to be inert rather than an error nobody handles.
type Role string

const (
	// OrganizationOwner holds everything, including who else may administer the tenant.
	OrganizationOwner Role = "organization_owner"
	// PlatformAdministrator runs the tenant: identity configuration, members, sessions,
	// tokens, the estate and the record. It cannot appoint an owner.
	PlatformAdministrator Role = "platform_administrator"
	// IntegrationManager configures Connections and Environments and is deliberately unable to
	// change who may sign in.
	//
	// The name is the specification's and is kept because it is product-facing, but it sits
	// awkwardly against CONTEXT.md: an Integration is a compiled closed vocabulary and nobody
	// manages one. What this role manages is CONNECTIONS — the configured instances. Recorded
	// here rather than renamed, because a role an operator has been told they hold is a worse
	// thing to rename quietly than a comment is to write.
	IntegrationManager Role = "integration_manager"
	// Investigator opens, cancels and reads Investigations, and cannot damage the estate.
	Investigator Role = "investigator"
	// Responder is an Investigator who may also act on the estate during the incident.
	Responder Role = "responder"
	// Viewer is read-only across the tenant.
	Viewer Role = "viewer"
	// Auditor reads the record and nothing else.
	Auditor Role = "auditor"
	// DirectorySynchroniser is what a customer's directory holds. It reaches the provisioning
	// endpoints and nothing else, for the same reason an Auditor reaches the record and nothing
	// else: the credential lives somewhere this product does not control.
	DirectorySynchroniser Role = "directory_synchroniser"
)

// roles is the declared set, in the order the product presents them: most privileged first, so
// a list rendered from this reads as a ladder.
var roles = []Role{
	OrganizationOwner, PlatformAdministrator, IntegrationManager,
	Investigator, Responder, Viewer, Auditor, DirectorySynchroniser,
}

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
// refused rather than defaulted: defaulting up is a privilege escalation and defaulting down
// is a silent lockout, and both are worse than saying the value is not a role.
func ParseRole(value string) (Role, bool) {
	role := Role(strings.TrimSpace(value))
	return role, KnownRole(role)
}

// reads are the permissions that change nothing. The set is declared rather than derived from
// the name, because "connection.trigger.secret.rotate" reads like a noun and is a mutation.
var reads = map[Permission]bool{
	EnvironmentRead:    true,
	ConnectionRead:     true,
	IntegrationRead:    true,
	RelayRead:          true,
	InvestigationRead:  true,
	IdentityRead:       true,
	MemberRead:         true,
	ServiceAccountRead: true,
	TokenRead:          true,
	AuditRead:          true,
}

// ReadOnly reports whether holding a permission can change anything. It is what the viewer
// test asserts against, so a mutating permission added to the read-only role fails the build.
func ReadOnly(permission Permission) bool { return reads[permission] }

// everyRead is what a role that may look at the tenant but change nothing holds.
var everyRead = []Permission{
	EnvironmentRead, ConnectionRead, IntegrationRead, RelayRead, InvestigationRead,
}

// granted is the table. It is the specification of each role, and it is the thing to read when
// answering "what can an Investigator do" — not the handlers.
var granted = map[Role]map[Permission]bool{
	OrganizationOwner:     setOf(allPermissions...),
	PlatformAdministrator: without(setOf(allPermissions...), MemberOwnerManage),

	// The role that actually runs the estate. It gains the Connection lifecycle in full —
	// validating, revising and the narrow delete — because those are the operations that turn
	// "configured" into "known to work", and a role that may create a Connection and never check
	// it is a role that can only leave the estate in a state somebody else has to verify.
	//
	// It does NOT gain relay.bootstrap-token.issue. Enrolling a Relay is installing
	// infrastructure into the tenant, and it stays with the two administrative roles.
	IntegrationManager: setOf(append(append([]Permission(nil), everyRead...),
		EnvironmentCreate, EnvironmentUpdate, EnvironmentDelete,
		ConnectionCreate, ConnectionUpdate, ConnectionDelete, ConnectionValidate,
		ConnectionSecretRotate,
	)...),

	Investigator: setOf(append(append([]Permission(nil), everyRead...),
		InvestigationOpen, InvestigationCancel, InvestigationReopen,
	)...),

	// A responder is an investigator who may also act on the estate during the incident —
	// turning a Connection off is the remediation this surface actually offers. Recording a
	// performed remediation (story 17) has no route yet and therefore no permission; adding
	// the route is the investigation outcome slice's work, not this one's.
	//
	// Validating is here for the incident itself. "Is this Connection actually working, or have
	// we been reasoning from a source that stopped answering an hour ago" is a question a
	// responder has at three in the morning, and making them wake an administrator to ask it is
	// the kind of gap that gets worked around with a shared credential.
	Responder: setOf(append(append([]Permission(nil), everyRead...),
		InvestigationOpen, InvestigationCancel, InvestigationReopen, ConnectionUpdate,
		ConnectionValidate,
	)...),

	Viewer:  setOf(everyRead...),
	Auditor: setOf(AuditRead),

	// One permission, like the Auditor's. A directory reports who is in the company; it does
	// not read this tenant's estate, its investigations or its record, and a token that could
	// would be a token worth stealing for something other than what it is for.
	DirectorySynchroniser: setOf(DirectorySync),
}

// Grants reports whether this role holds a permission. An unrecognised role grants nothing,
// which is what makes a corrupted column or an unmapped identity-provider group safe.
func Grants(role Role, permission Permission) bool { return granted[role][permission] }

// Grants reports whether this role holds a permission.
func (r Role) Grants(permission Permission) bool { return Grants(r, permission) }

// PermissionsOf returns what a role holds, in the declared order, so a surface that lists a
// role's permissions renders the same list every time.
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

func without(set map[Permission]bool, removed ...Permission) map[Permission]bool {
	for _, permission := range removed {
		delete(set, permission)
	}
	return set
}
