package identity

import (
	"net/http"
	"strconv"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

type memberRequest struct {
	Role   *string `json:"role"`
	Active *bool   `json:"active"`
}

func (h Handlers) listMembers(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	organization, ok := h.organization(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	list, err := h.Database.ListMembers(ctx, principal, organization, storage.Page{Limit: pageSize(r), After: r.URL.Query().Get("after")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make([]memberView, 0, len(list.Members))
	for _, member := range list.Members {
		views = append(views, memberViewOf(member))
	}
	writeJSON(w, http.StatusOK, memberListView{Members: views, Next: list.Next})
}

func (h Handlers) setMember(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	organization, ok := h.organization(w, r)
	if !ok {
		return
	}
	user, ok := identifier(w, r, "user")
	if !ok {
		return
	}
	var body memberRequest
	if !decode(w, r, &body) {
		return
	}
	if body.Role == nil && body.Active == nil {
		writeJSON(w, http.StatusBadRequest, errorView{Error: "role or active is required"})
		return
	}
	var role *authz.Role
	if body.Role != nil {
		parsed, known := authz.ParseRole(*body.Role)
		if !known {
			writeJSON(w, http.StatusBadRequest, errorView{Error: "role is not one this build has"})
			return
		}
		role = &parsed
	}
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	member, err := h.Database.UpdateMembership(
		ctx, principal, organization, user, role, body.Active)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, memberViewOf(member))
}

func (h Handlers) removeMember(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	organization, ok := h.organization(w, r)
	if !ok {
		return
	}
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
	SessionLifetimeSeconds int `json:"sessionLifetimeSeconds"`
	AuditRetentionDays     int `json:"auditRetentionDays"`
}

func (h Handlers) readPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.caller(w, r); !ok {
		return
	}
	organization, ok := h.organization(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	lifetime, retention, err := h.Database.SessionPolicy(ctx, organization)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policyView{SessionLifetimeSeconds: int(session.ClampLifetime(lifetime).Seconds()), AuditRetentionDays: retention, AuditRetentionEnforced: h.RetentionEnforced})
}

func (h Handlers) writePolicy(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	organization, ok := h.organization(w, r)
	if !ok {
		return
	}
	var body policyRequest
	if !decode(w, r, &body) {
		return
	}
	if body.SessionLifetimeSeconds < 0 || body.AuditRetentionDays < 0 {
		writeJSON(w, http.StatusBadRequest, errorView{Error: "a policy value must not be negative"})
		return
	}
	lifetime := time.Duration(body.SessionLifetimeSeconds) * time.Second
	if lifetime != 0 && (lifetime < session.MinLifetime || lifetime > session.MaxLifetime) {
		writeJSON(w, http.StatusBadRequest, errorView{Error: "sessionLifetimeSeconds must be between " + secondsIn(session.MinLifetime) + " and " + secondsIn(session.MaxLifetime) + ", or 0 for the product default"})
		return
	}
	ctx, cancel := contextWithTimeout(r, readTimeout)
	defer cancel()
	if err := h.Database.SetSessionPolicy(ctx, principal, organization, lifetime, body.AuditRetentionDays); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, policyView{SessionLifetimeSeconds: int(session.ClampLifetime(lifetime).Seconds()), AuditRetentionDays: body.AuditRetentionDays, AuditRetentionEnforced: h.RetentionEnforced})
}

func secondsIn(value time.Duration) string { return strconv.Itoa(int(value.Seconds())) }
