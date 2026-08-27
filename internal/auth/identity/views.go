package identity

import (
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/session"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

// The shapes this surface answers with.
//
// They are spelled out rather than serialised from the storage types on purpose. A column
// added to a table must not silently become a field in a response — several of these tables
// hold a digest, and one holds a sealed client secret.

// sessionView answers "who is signed in and what may they see". It is the producer for the
// Principal contract the frontend already declares; the contract does not change here, the
// control plane implements it.
type sessionView struct {
	Principal     principalView    `json:"principal"`
	Organizations []membershipView `json:"organizations"`
	ExpiresAt     time.Time        `json:"expiresAt"`
}

type principalView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	// Roles and Scopes are the frontend's declared shape. Roles is every role this principal
	// holds anywhere, and Scopes is every permission those roles carry — flattened, because an
	// interface deciding whether to render a button asks about a permission and should not
	// have to hold the role table to answer.
	Roles  []string `json:"roles"`
	Scopes []string `json:"scopes"`
}

type membershipView struct {
	Organization string `json:"organizationId"`
	Role         string `json:"role"`
}

func sessionViewOf(
	principal authz.Principal, email string, expires time.Time,
) sessionView {
	memberships := principal.Memberships()

	organizations := make([]membershipView, 0, len(memberships))
	roles := make([]string, 0, len(memberships))
	seenRole := make(map[authz.Role]bool, len(memberships))
	seenScope := make(map[authz.Permission]bool)
	scopes := make([]string, 0, len(authz.Permissions()))

	for _, membership := range memberships {
		organizations = append(organizations, membershipView{
			Organization: membership.Organization.String(),
			Role:         string(membership.Role),
		})
		if !seenRole[membership.Role] {
			seenRole[membership.Role] = true
			roles = append(roles, string(membership.Role))
		}
	}
	// Declared order, so the list is the same on every request and a client diffing it sees a
	// change only when one happened.
	for _, permission := range authz.Permissions() {
		for _, membership := range memberships {
			if membership.Role.Grants(permission) && !seenScope[permission] {
				seenScope[permission] = true
				scopes = append(scopes, string(permission))
			}
		}
	}

	return sessionView{
		Principal: principalView{
			ID:          principal.ID(),
			Kind:        audit.ActorKind(principal.Kind()).String(),
			DisplayName: principal.DisplayName(),
			Email:       email,
			Roles:       roles,
			Scopes:      scopes,
		},
		Organizations: organizations,
		ExpiresAt:     expires,
	}
}

// signOutView is what sign-out answers. It is a true statement now: the row is gone before
// this is written.
type signOutView struct {
	SignedOut bool `json:"signedOut"`
}

type memberView struct {
	UserID      string    `json:"userId"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	Source      string    `json:"source"`
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
		Source:      member.Source.String(),
		Disabled:    member.Disabled,
		CreatedAt:   member.CreatedAt,
	}
}

type liveSessionView struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	IssuedAt   time.Time `json:"issuedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	UserAgent  string    `json:"userAgent"`
	Address    string    `json:"address"`
}

type liveSessionListView struct {
	Sessions []liveSessionView `json:"sessions"`
}

func liveSessionViewOf(live session.Session) liveSessionView {
	return liveSessionView{
		ID:         live.ID.String(),
		UserID:     live.UserID.String(),
		IssuedAt:   live.IssuedAt,
		ExpiresAt:  live.ExpiresAt,
		LastSeenAt: live.LastSeenAt,
		UserAgent:  live.UserAgent,
		Address:    live.Address,
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
