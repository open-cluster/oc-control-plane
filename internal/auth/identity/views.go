package identity

import (
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// Response shapes are spelled out rather than serialised from storage types. A column
// added to a table must not silently become a field in a response — several of these tables
// hold a digest, and one holds a sealed client secret.
type sessionView struct {
	Principal            principalView  `json:"principal"`
	Organization         membershipView `json:"organization"`
	AuthenticationMethod string         `json:"authenticationMethod"`
	CSRF                 csrfView       `json:"csrf"`
	ExpiresAt            time.Time      `json:"expiresAt"`
}

type csrfView struct {
	Mode                     string `json:"mode"`
	RequiredForUnsafeMethods bool   `json:"requiredForUnsafeMethods"`
}

type principalView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	// Roles and Scopes retain the frontend's list shape even though a User has one Membership.
	Roles  []string `json:"roles"`
	Scopes []string `json:"scopes"`
}

type membershipView struct {
	Organization string `json:"organizationId"`
	DisplayName  string `json:"displayName"`
	Role         string `json:"role"`
}

func sessionViewOf(principal authz.Principal) sessionView {
	role := principal.Role()
	scopes := make([]string, 0, len(authz.Permissions()))
	for _, permission := range authz.Permissions() {
		if role.Grants(permission) {
			scopes = append(scopes, string(permission))
		}
	}

	return sessionView{
		Principal: principalView{
			ID:          principal.UserID().String(),
			DisplayName: principal.DisplayName(),
			Email:       principal.Email(),
			Roles:       []string{string(role)},
			Scopes:      scopes,
		},
		Organization: membershipView{
			Organization: principal.Organization().String(),
			DisplayName:  principal.OrganizationName(),
			Role:         string(role),
		},
		AuthenticationMethod: principal.AuthenticationMethod(),
		CSRF:                 csrfView{Mode: "origin", RequiredForUnsafeMethods: true},
		ExpiresAt:            principal.ExpiresAt(),
	}
}

type signOutView struct {
	SignedOut bool `json:"signedOut"`
}

type memberView struct {
	UserID      string    `json:"userId"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"createdAt"`
}

type memberListView struct {
	Members []memberView `json:"members"`
	Next    string       `json:"next"`
}

func memberViewOf(member storage.Member) memberView {
	return memberView{
		UserID:      member.UserID.String(),
		Email:       member.Email,
		DisplayName: member.DisplayName,
		Role:        string(member.Role),
		Disabled:    member.Disabled,
		CreatedAt:   member.CreatedAt,
	}
}

type liveSessionView struct {
	ID              string    `json:"id"`
	UserID          string    `json:"userId"`
	IssuedAt        time.Time `json:"issuedAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
	LastSeenAt      time.Time `json:"lastSeenAt"`
	ClientUserAgent string    `json:"clientUserAgent,omitempty"`
	RemoteAddr      string    `json:"remoteAddr,omitempty"`
}

type liveSessionListView struct {
	Sessions []liveSessionView `json:"sessions"`
	Next     *string           `json:"next"`
}

func liveSessionViewOf(live session.Session) liveSessionView {
	return liveSessionView{
		ID:              live.ID.String(),
		UserID:          live.UserID.String(),
		IssuedAt:        live.IssuedAt,
		ExpiresAt:       live.ExpiresAt,
		LastSeenAt:      live.LastSeenAt,
		ClientUserAgent: live.ClientUserAgent,
		RemoteAddr:      live.RemoteAddr,
	}
}

type policyView struct {
	SessionLifetimeSeconds int `json:"sessionLifetimeSeconds"`
	AuditRetentionDays     int `json:"auditRetentionDays"`
	// AuditRetentionEnforced states plainly that the schedule is declared and not yet applied.
	// A product reporting a retention period it does not enforce is worse than one reporting
	// none, so the surface says which this is.
	AuditRetentionEnforced bool `json:"auditRetentionEnforced"`
}

type auditEventView struct {
	ID            string         `json:"id"`
	OccurredAt    time.Time      `json:"occurredAt"`
	ActorKind     string         `json:"actorKind"`
	ActorID       string         `json:"actorId"`
	ActorName     string         `json:"actorName"`
	Action        string         `json:"action"`
	TargetKind    string         `json:"targetKind"`
	TargetID      string         `json:"targetId"`
	Outcome       string         `json:"outcome"`
	SourceAddress string         `json:"sourceAddress"`
	RequestID     string         `json:"requestId"`
	Detail        map[string]any `json:"detail"`
}

type auditListView struct {
	Events []auditEventView `json:"events"`
	Next   string           `json:"next"`
}

func auditEventViewOf(event audit.Recorded) auditEventView {
	return auditEventView{
		ID:            event.ID,
		OccurredAt:    event.OccurredAt,
		ActorKind:     event.Actor.Kind.String(),
		ActorID:       event.Actor.ID,
		ActorName:     event.Actor.DisplayName,
		Action:        string(event.Action),
		TargetKind:    string(event.Target.Kind),
		TargetID:      event.Target.ID,
		Outcome:       event.Outcome.String(),
		SourceAddress: event.SourceAddress,
		RequestID:     event.RequestID,
		Detail:        event.Detail,
	}
}
