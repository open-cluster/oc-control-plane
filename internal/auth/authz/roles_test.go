package authz

import "testing"

func TestKnownRoles(t *testing.T) {
	for _, role := range []Role{Admin, Editor, Viewer} {
		if !KnownRole(role) {
			t.Errorf("%s is not known", role)
		}
	}
	if KnownRole("superuser") {
		t.Fatal("unknown role is known")
	}
	for _, permission := range allPermissions {
		if Role("superuser").Grants(permission) {
			t.Errorf("unknown role grants %s", permission)
		}
	}
}

func TestAdminGrantsEveryDeclaredPermission(t *testing.T) {
	for _, permission := range allPermissions {
		if !Admin.Grants(permission) {
			t.Errorf("admin does not grant %s", permission)
		}
	}
}

func TestEditorCannotAdministerIdentityOrInfrastructure(t *testing.T) {
	for _, permission := range []Permission{
		IntegrationRead, IntegrationVerify, IntegrationUpdate,
		IncidentRead, IncidentMerge, InvestigationOpen, ConversationWrite,
	} {
		if !Editor.Grants(permission) {
			t.Errorf("editor does not grant %s", permission)
		}
	}
	for _, permission := range []Permission{
		IntegrationCreate, IntegrationDelete, IntegrationSecretRotate,
		RelayBootstrapIssue, RelayConflictClear, IdentityConfigure, MemberManage,
	} {
		if Editor.Grants(permission) {
			t.Errorf("editor grants %s", permission)
		}
	}
}

func TestViewerHasExplicitReadOnlyBoundary(t *testing.T) {
	for _, permission := range []Permission{
		IntegrationRead, RelayRead, IncidentRead, PostmortemRead,
		InvestigationRead, ConversationRead, AuditRead,
	} {
		if !Viewer.Grants(permission) {
			t.Errorf("viewer does not grant %s", permission)
		}
	}
	for _, permission := range []Permission{
		IntegrationCreate, IntegrationUpdate, IntegrationDelete,
		IncidentMerge, InvestigationOpen, ConversationWrite, MemberManage,
	} {
		if Viewer.Grants(permission) {
			t.Errorf("viewer grants %s", permission)
		}
	}
}
