package identity

import (
	"net/http"
	"slices"

	"github.com/open-cluster/oc-control-plane/internal/api/listing"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type memberRequest struct {
	Role string `json:"role"`
}

func (h Handlers) listMembers(w http.ResponseWriter, r *http.Request) {
	query, ok := listQuery(w, r, listing.Spec{
		DefaultSort: listing.Sort{Field: "createdAt"},
	})
	if !ok {
		return
	}
	principal := h.caller(r)
	organization := h.organization(r)
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	list, err := h.Database.ListMembers(ctx, principal, organization, storage.Page{
		Limit: query.Limit,
		After: query.Cursor,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make([]memberView, 0, len(list.Members))
	for _, member := range list.Members {
		views = append(views, memberViewOf(member))
	}
	writeJSON(w, http.StatusOK, memberListView{
		Members: views,
		Next:    list.Next,
	})
}

func (h Handlers) setMember(w http.ResponseWriter, r *http.Request) {
	principal := h.caller(r)
	organization := h.organization(r)
	user, ok := identifier(w, r, "user")
	if !ok {
		return
	}
	var body memberRequest
	if !decode(w, r, &body) {
		return
	}
	role, known := authz.ParseRole(body.Role)
	if !known {
		writeJSON(w, http.StatusBadRequest, errorView{Error: "role is not one this build has"})
		return
	}
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	member, err := h.Database.UpdateMembership(
		ctx, principal, organization, user, role)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, memberViewOf(member))
}

func (h Handlers) removeMember(w http.ResponseWriter, r *http.Request) {
	principal := h.caller(r)
	organization := h.organization(r)
	user, ok := identifier(w, r, "user")
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	if err := h.Database.RemoveMembership(ctx, principal, organization, user); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type policyRequest struct {
	AuditRetentionDays int `json:"auditRetentionDays"`
}

func (h Handlers) readPolicy(w http.ResponseWriter, r *http.Request) {
	_ = h.caller(r)
	organization := h.organization(r)
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	retention, err := h.Database.OrganizationAuditRetention(ctx, organization)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policyView{
		SessionLifetimeSeconds: int(h.SessionLifetime.Seconds()),
		AuditRetentionDays:     retention,
		AuditRetentionEnforced: true})
}

func (h Handlers) writePolicy(w http.ResponseWriter, r *http.Request) {
	principal := h.caller(r)
	organization := h.organization(r)
	var body policyRequest
	if !decode(w, r, &body) {
		return
	}
	if body.AuditRetentionDays < 0 {
		writeJSON(w, http.StatusBadRequest, errorView{Error: "auditRetentionDays must not be negative"})
		return
	}
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	if err := h.Database.SetOrganizationAuditRetention(ctx, principal, organization, body.AuditRetentionDays); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policyView{SessionLifetimeSeconds: int(h.SessionLifetime.Seconds()), AuditRetentionDays: body.AuditRetentionDays, AuditRetentionEnforced: true})
}
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

// Audit access is read-only and events are returned newest first so investigations start
// from the most recent change.
func (h Handlers) auditEvents(writer http.ResponseWriter, request *http.Request) {
	query, ok := listQuery(writer, request, listing.Spec{
		DefaultSort: listing.Sort{Field: "occurredAt", Descending: true},
	})
	if !ok {
		return
	}
	principal := h.caller(request)
	organization := h.organization(request)
	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	list, err := h.Database.AuditEvents(ctx, principal, organization, audit.Page{
		Limit: query.Limit,
		After: query.Cursor,
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	views := make([]auditEventView, 0, len(list.Events))
	for _, event := range list.Events {
		views = append(views, auditEventViewOf(event))
	}
	writeJSON(writer, http.StatusOK, auditListView{Events: views, Next: list.Next})
}
