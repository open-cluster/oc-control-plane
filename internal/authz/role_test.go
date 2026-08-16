package authz_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/authz"
)

// The role table IS the specification of a role: reading it is how a reviewer answers
// "what can an Editor do". These tests assert the properties the table states, so a
// permission quietly added to a role fails here rather than shipping.

// Three human roles and one machine role, fixed sets, no custom roles. The count is
// asserted rather than the list merely being read, so a fifth is a decision somebody
// writes down.
func TestTheRolesAreTheFourTheProductDeclares(t *testing.T) {
	t.Parallel()

	want := []authz.Role{
		authz.Admin, authz.Editor, authz.Viewer, authz.DirectorySynchroniser,
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

// An Admin holds everything, or a permission added later is silently unreachable by
// anybody.
func TestAnAdminHoldsEveryPermissionThisBuildDeclares(t *testing.T) {
	t.Parallel()

	for _, permission := range authz.Permissions() {
		if !authz.Admin.Grants(permission) {
			t.Errorf("the admin does not hold %s; a permission no role holds is a route "+
				"nobody in the product can reach", permission)
		}
	}
}

// An Editor operates the estate during an incident and cannot change what the estate IS or
// who may sign in. The forbidden set is what separates operating from administering.
func TestAnEditorOperatesWithoutAdministering(t *testing.T) {
	t.Parallel()

	for _, required := range []authz.Permission{
		authz.IntegrationRead, authz.IntegrationVerify, authz.IntegrationUpdate,
		authz.IncidentRead, authz.IncidentMerge, authz.RelayRead, authz.AuditRead,
	} {
		if !authz.Editor.Grants(required) {
			t.Errorf("an editor cannot %s, which is the job at three in the morning", required)
		}
	}
	for _, forbidden := range []authz.Permission{
		authz.IntegrationCreate, authz.IntegrationDelete, authz.IntegrationSecretRotate,
		authz.RelayBootstrapIssue, authz.RelayConflictClear,
		authz.IdentityConfigure, authz.MemberManage, authz.SessionRevoke,
		authz.ServiceAccountManage, authz.TokenManage, authz.DirectorySync,
	} {
		if authz.Editor.Grants(forbidden) {
			t.Errorf("an editor holds %s; changing what the estate is, and who may sign in, "+
				"is the Admin's", forbidden)
		}
	}
}

// A Viewer is read-only. Nothing in the set may change anything.
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
	for _, required := range []authz.Permission{
		authz.IntegrationRead, authz.IncidentRead, authz.RelayRead, authz.AuditRead,
	} {
		if !authz.Viewer.Grants(required) {
			t.Errorf("a viewer cannot %s, which is the whole role", required)
		}
	}
}

// Clearing a session conflict destroys a credential-theft finding; issuing a bootstrap
// token extends the estate. Both stay with the Admin: no role below it may hold either.
func TestOnlyAnAdminMayDestroyAFindingOrExtendTheEstate(t *testing.T) {
	t.Parallel()

	for _, role := range authz.Roles() {
		if role == authz.Admin {
			continue
		}
		if role.Grants(authz.RelayConflictClear) {
			t.Errorf("%s can clear a session conflict; withdrawing the mark destroys the "+
				"finding, and nothing else in the product records that it existed", role)
		}
		if role.Grants(authz.RelayBootstrapIssue) {
			t.Errorf("%s can issue a bootstrap token, which enrols infrastructure into the "+
				"tenant", role)
		}
	}
}

// A directory's credential reaches the provisioning endpoints and nothing else: the
// credential lives somewhere this product does not control, so what it can reach when it
// leaks is the whole question.
func TestADirectorySynchroniserProvisionsAndReadsNothingElse(t *testing.T) {
	t.Parallel()

	for _, permission := range authz.Permissions() {
		granted := authz.DirectorySynchroniser.Grants(permission)
		if permission == authz.DirectorySync && !granted {
			t.Error("a directory synchroniser cannot provision, which is the whole role")
		}
		if permission != authz.DirectorySync && granted {
			t.Errorf("a directory synchroniser holds %s; this credential sits in a customer's "+
				"identity vendor and what it reaches when it leaks is the whole question",
				permission)
		}
	}
}

// And nobody below the Admin may provision by hand. A directory is a source of truth about
// who is in the company, so a credential that could write to it could grant itself a
// tenant.
func TestOnlyAnAdminOrADirectoryMayProvision(t *testing.T) {
	t.Parallel()

	for _, role := range authz.Roles() {
		switch role {
		case authz.Admin, authz.DirectorySynchroniser:
			if !role.Grants(authz.DirectorySync) {
				t.Errorf("%s cannot provision", role)
			}
		default:
			if role.Grants(authz.DirectorySync) {
				t.Errorf("%s can provision; a directory decides who is in a tenant, so writing "+
					"to it is granting a tenant", role)
			}
		}
	}
}
