package authz_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/authz"
)

// The role table IS the specification of a role: reading it is how a reviewer answers "what
// can an Investigator do". These tests assert the properties the specification states in
// prose, so a permission quietly added to a role fails here rather than shipping.

// Seven roles, fixed sets, no custom roles in this release. A role parsed from a string that
// names none of them must not resolve to something with permissions.
func TestTheRolesAreTheSevenTheProductDeclares(t *testing.T) {
	t.Parallel()

	want := []authz.Role{
		authz.OrganizationOwner, authz.PlatformAdministrator, authz.IntegrationManager,
		authz.Investigator, authz.Responder, authz.Viewer, authz.Auditor,
	}

	got := authz.Roles()
	if len(got) != len(want) {
		t.Fatalf("this build has %d roles, want the %d the product declares: %v",
			len(got), len(want), got)
	}
	for _, role := range want {
		if !authz.KnownRole(role) {
			t.Errorf("%s is not a known role", role)
		}
	}
}

func TestARoleNobodyDeclaredGrantsNothing(t *testing.T) {
	t.Parallel()

	invented := authz.Role("superuser")

	if authz.KnownRole(invented) {
		t.Fatal("an invented role is known")
	}
	for _, permission := range authz.Permissions() {
		if invented.Grants(permission) {
			t.Errorf("an invented role grants %s; an unparsed role must be inert, because the "+
				"value arrives from a database column and an identity provider's group map",
				permission)
		}
	}
}

// Story 19: an auditor has read access to the audit log and nothing else, so that their access
// does not itself become a risk.
func TestAnAuditorReadsTheRecordAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, permission := range authz.Permissions() {
		granted := authz.Auditor.Grants(permission)
		if permission == authz.AuditRead && !granted {
			t.Error("an auditor cannot read the audit log")
		}
		if permission != authz.AuditRead && granted {
			t.Errorf("an auditor holds %s; the role exists precisely so that reading the record "+
				"does not require holding anything else", permission)
		}
	}
}

// Story 15: an integration manager configures Connections without being able to change who may
// sign in.
func TestAnIntegrationManagerCannotTouchIdentity(t *testing.T) {
	t.Parallel()

	for _, forbidden := range []authz.Permission{
		authz.IdentityConfigure, authz.MemberManage, authz.MemberOwnerManage,
		authz.SessionRevoke, authz.ServiceAccountManage, authz.TokenManage, authz.AuditRead,
	} {
		if authz.IntegrationManager.Grants(forbidden) {
			t.Errorf("an integration manager holds %s; the role's whole point is that "+
				"configuring Connections is not the same job as deciding who may sign in",
				forbidden)
		}
	}
	for _, required := range []authz.Permission{
		authz.ConnectionCreate, authz.ConnectionUpdate, authz.ConnectionSecretRotate,
		authz.IntegrationRead,
	} {
		if !authz.IntegrationManager.Grants(required) {
			t.Errorf("an integration manager cannot %s, which is the job", required)
		}
	}
}

// Story 16: an investigator opens and cancels Investigations and cannot delete Connections, so
// that an incident-time mistake cannot damage the estate.
func TestAnInvestigatorCannotDamageTheEstate(t *testing.T) {
	t.Parallel()

	for _, required := range []authz.Permission{
		authz.InvestigationOpen, authz.InvestigationCancel, authz.InvestigationRead,
	} {
		if !authz.Investigator.Grants(required) {
			t.Errorf("an investigator cannot %s, which is the job", required)
		}
	}
	for _, forbidden := range []authz.Permission{
		authz.ConnectionCreate, authz.ConnectionUpdate, authz.EnvironmentDelete,
		authz.ConnectionSecretRotate,
	} {
		if authz.Investigator.Grants(forbidden) {
			t.Errorf("an investigator holds %s; an incident-time mistake must not be able to "+
				"damage the estate", forbidden)
		}
	}
}

// Story 18: a viewer is read-only. Nothing in the set may change anything.
func TestAViewerChangesNothing(t *testing.T) {
	t.Parallel()

	for _, permission := range authz.Permissions() {
		if !authz.Viewer.Grants(permission) {
			continue
		}
		if !authz.ReadOnly(permission) {
			t.Errorf("a viewer holds %s, which is not a read; being looped in during an "+
				"incident must not come with the ability to change anything", permission)
		}
	}
	if !authz.Viewer.Grants(authz.InvestigationRead) {
		t.Error("a viewer cannot read investigations, which is the whole role")
	}
}

// An owner holds everything, or a permission added later is silently unreachable by anybody.
func TestAnOwnerHoldsEveryPermissionThisBuildDeclares(t *testing.T) {
	t.Parallel()

	for _, permission := range authz.Permissions() {
		if !authz.OrganizationOwner.Grants(permission) {
			t.Errorf("the owner does not hold %s; a permission no role holds is a route "+
				"nobody in the product can reach", permission)
		}
	}
}

// The two administrative roles must actually differ, or one of them is decoration. The
// difference is deciding who else may administer the tenant.
func TestAPlatformAdministratorRunsTheTenantAndDoesNotDecideItsOwners(t *testing.T) {
	t.Parallel()

	if authz.PlatformAdministrator.Grants(authz.MemberOwnerManage) {
		t.Error("a platform administrator can appoint an owner; the two roles are then the " +
			"same role with two names")
	}
	for _, required := range []authz.Permission{
		authz.IdentityConfigure, authz.MemberManage, authz.SessionRevoke,
		authz.RelayConflictClear, authz.AuditRead, authz.TokenManage,
	} {
		if !authz.PlatformAdministrator.Grants(required) {
			t.Errorf("a platform administrator cannot %s, which is the job", required)
		}
	}
}

// Clearing a session conflict destroys a credential-theft finding. The spec names it as one of
// the highest-privilege operations in the product; no role below administrator may hold it.
func TestOnlyAnAdministratorMayDestroyACredentialTheftFinding(t *testing.T) {
	t.Parallel()

	for _, role := range authz.Roles() {
		if role == authz.OrganizationOwner || role == authz.PlatformAdministrator {
			continue
		}
		if role.Grants(authz.RelayConflictClear) {
			t.Errorf("%s can clear a session conflict; withdrawing the mark destroys the "+
				"finding, and nothing else in the product records that it existed", role)
		}
	}
}
