package authz_test

import (
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

// The role table IS the specification of a role: reading it is how a reviewer answers
// "what can an Editor do". These tests assert the properties the table states, so a
// permission quietly added to a role fails here rather than shipping.

// Three human roles, fixed sets, no custom roles. The count is
// asserted rather than the list merely being read, so a fourth is a decision somebody
// writes down.
func TestTheRolesAreTheThreeTheProductDeclares(t *testing.T) {
	t.Parallel()

	want := []authz.Role{
		authz.Admin, authz.Editor, authz.Viewer,
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

func TestRetiredDirectorySynchroniserIsInert(t *testing.T) {
	t.Parallel()

	retired := authz.Role("directory_synchroniser")
	if authz.KnownRole(retired) {
		t.Fatal("the retired directory synchronizer remains a role this build accepts")
	}
	for _, permission := range authz.Permissions() {
		if retired.Grants(permission) {
			t.Fatalf("the retired directory synchronizer still grants %s", permission)
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
		authz.WebhookWorkReplay,
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
