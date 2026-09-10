package identity

import (
	"net/http"
	"slices"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

type organizationView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Membership  struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"membership"`
}

type organizationListView struct {
	Organizations []organizationView `json:"organizations"`
	Next          *string            `json:"next"`
}

var organizationsListSpec = listing.Spec{
	DefaultSort: listing.Sort{Field: "id"},
}

type createOrganizationRequest struct {
	DisplayName string `json:"displayName"`
}

func (h Handlers) organizations(writer http.ResponseWriter, request *http.Request) {
	query, ok := listQuery(writer, request, organizationsListSpec)
	if !ok {
		return
	}
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	memberships := principal.Memberships()
	page, next, err := listing.Cut(memberships, query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	views := make([]organizationView, 0, len(page))
	for _, membership := range page {
		view := organizationView{
			ID: membership.Organization.String(), DisplayName: membership.DisplayName,
		}
		view.Membership.ID = membership.ID
		view.Membership.Role = string(membership.Role)
		views = append(views, view)
	}
	writeJSON(writer, http.StatusOK, organizationListView{
		Organizations: views, Next: listing.Continuation(next),
	})
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
	displayName := strings.TrimSpace(body.DisplayName)
	if displayName == "" || len(displayName) > 256 {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "displayName must be between 1 and 256 characters"})
		return
	}
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()
	membership, err := h.Database.CreateOrganization(ctx, principal, displayName)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	view := organizationView{ID: membership.Organization.String(), DisplayName: displayName}
	view.Membership.ID = membership.ID
	view.Membership.Role = string(membership.Role)
	writeJSON(writer, http.StatusCreated, view)
}

func (h Handlers) permissions(writer http.ResponseWriter, request *http.Request) {
	query, ok := listQuery(writer, request, listing.Spec{
		DefaultSort: listing.Sort{Field: "name"},
	})
	if !ok {
		return
	}
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
	slices.Sort(permissions)
	permissions, next, err := listing.Cut(permissions, query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"organizationId": organization.String(),
		"role":           string(role),
		"permissions":    permissions,
		"next":           listing.Continuation(next),
	})
}
