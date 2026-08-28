package identity

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type organizationView struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}

type organizationListView struct {
	Organizations []organizationView `json:"organizations"`
}

type createOrganizationRequest struct {
	Organization string `json:"organization"`
}

func (h Handlers) organizations(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	memberships := principal.Memberships()
	views := make([]organizationView, 0, len(memberships))
	for _, membership := range memberships {
		views = append(views, organizationView{
			ID: membership.Organization.String(), Role: string(membership.Role),
		})
	}
	writeJSON(writer, http.StatusOK, organizationListView{Organizations: views})
}

func (h Handlers) createOrganization(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	if h.CanCreateOrganization == nil || !h.CanCreateOrganization(principal) {
		writeJSON(writer, http.StatusForbidden,
			errorView{Error: "organization creation is not permitted"})
		return
	}
	var body createOrganizationRequest
	if !decode(writer, request, &body) {
		return
	}
	organization, err := tenancy.NewOrganization(strings.TrimSpace(body.Organization))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "organization is not a name"})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	membership, err := h.Database.CreateOrganization(ctx, principal, organization)
	if errors.Is(err, storage.ErrOrganizationExists) {
		writeJSON(writer, http.StatusConflict, errorView{Error: "organization already exists"})
		return
	}
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusCreated, organizationView{
		ID: membership.Organization.String(), Role: string(membership.Role),
	})
}

func (h Handlers) permissions(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	role, member := principal.RoleIn(organization)
	if !member {
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
		return
	}
	permissions := make([]string, 0, len(authz.Permissions()))
	for _, permission := range authz.Permissions() {
		if role.Grants(permission) {
			permissions = append(permissions, string(permission))
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"organizationId": organization.String(),
		"role":           string(role),
		"permissions":    permissions,
	})
}
