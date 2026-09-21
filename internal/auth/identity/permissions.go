package identity

import (
	"net/http"
	"slices"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

func (h Handlers) permissions(writer http.ResponseWriter, request *http.Request) {
	query, ok := listQuery(writer, request, listing.Spec{
		DefaultSort: listing.Sort{Field: "name"},
	})
	if !ok {
		return
	}
	principal := h.caller(request)
	organization := principal.Organization()
	role := principal.Role()
	permissions := make([]string, 0, len(authz.Permissions()))
	for _, permission := range authz.Permissions() {
		if role.Grants(permission) {
			permissions = append(permissions, string(permission))
		}
	}
	slices.Sort(permissions)
	permissions, next, err := listing.SlicePage(permissions, query)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"organizationId": organization.String(),
		"role":           string(role),
		"permissions":    permissions,
		"next":           listing.CursorPtr(next),
	})
}
