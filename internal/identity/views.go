package identity

import (
	"time"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/session"
	"github.com/open-cluster/oc-control-plane/internal/storage"
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

// providerChoiceView is everything an unauthenticated caller learns about a tenant's way in:
// which one to pick and what to call it. Never the issuer, never the client identifier.
type providerChoiceView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type providerChoicesView struct {
	Providers []providerChoiceView `json:"providers"`
}

// providerView is what an administrator sees. The sealed client secret has no field here and
// no path reads it back; an administrator who lost it re-enters one.
type providerView struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	Protocol             string            `json:"protocol"`
	Issuer               string            `json:"issuer"`
	ClientID             string            `json:"clientId"`
	VerifiedDomains      []string          `json:"verifiedDomains"`
	JITEnabled           bool              `json:"jitEnabled"`
	JITRole              string            `json:"jitRole"`
	RequireVerifiedEmail bool              `json:"requireVerifiedEmail"`
	GroupClaim           string            `json:"groupClaim"`
	GroupRoleMap         map[string]string `json:"groupRoleMap"`
	Disabled             bool              `json:"disabled"`
	CreatedAt            time.Time         `json:"createdAt"`
	UpdatedAt            time.Time         `json:"updatedAt"`
	// SignInURL is where a browser starts a sign-in against this provider, so a console does
	// not have to construct the path itself.
	SignInURL string `json:"signInUrl"`
}

func providerViewOf(provider storage.IdentityProvider) providerView {
	return providerView{
		ID:                   provider.ID.String(),
		Name:                 provider.Name,
		Protocol:             provider.Protocol.String(),
		Issuer:               provider.Issuer,
		ClientID:             provider.ClientID,
		VerifiedDomains:      orEmpty(provider.VerifiedDomains),
		JITEnabled:           provider.JITEnabled,
		JITRole:              string(provider.JITRole),
		RequireVerifiedEmail: provider.RequireVerifiedEmail,
		GroupClaim:           provider.GroupClaim,
		GroupRoleMap:         provider.GroupRoleMap,
		Disabled:             provider.Disabled(),
		CreatedAt:            provider.CreatedAt,
		UpdatedAt:            provider.UpdatedAt,
		SignInURL: "/operator/v1/organizations/" + provider.Organization +
			"/sign-in/" + provider.ID.String(),
	}
}

type providerListView struct {
	Providers []providerView `json:"providers"`
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

type serviceAccountView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"createdAt"`
	CreatedBy   string    `json:"createdBy"`
}

type serviceAccountListView struct {
	ServiceAccounts []serviceAccountView `json:"serviceAccounts"`
}

func serviceAccountViewOf(account storage.ServiceAccount) serviceAccountView {
	return serviceAccountView{
		ID:          account.ID.String(),
		Name:        account.Name,
		Description: account.Description,
		Disabled:    !account.DisabledAt.IsZero(),
		CreatedAt:   account.CreatedAt,
		CreatedBy:   account.CreatedBy,
	}
}

type tokenView struct {
	ID               string     `json:"id"`
	ServiceAccountID string     `json:"serviceAccountId"`
	Prefix           string     `json:"prefix"`
	Role             string     `json:"role"`
	ExpiresAt        time.Time  `json:"expiresAt"`
	LastUsedAt       *time.Time `json:"lastUsedAt"`
	Revoked          bool       `json:"revoked"`
	CreatedAt        time.Time  `json:"createdAt"`
	CreatedBy        string     `json:"createdBy"`
}

type tokenListView struct {
	Tokens []tokenView `json:"tokens"`
}

// issuedTokenView is the one response in this package that carries a credential. It is shown
// once, is not logged, is not returned again, and is not recoverable.
type issuedTokenView struct {
	Token        tokenView `json:"token"`
	Secret       string    `json:"secret"`
	SecretNotice string    `json:"secretNotice"`
}

func tokenViewOf(token storage.APIToken) tokenView {
	view := tokenView{
		ID:               token.ID.String(),
		ServiceAccountID: token.ServiceAccountID.String(),
		Prefix:           token.Prefix,
		Role:             string(token.Role),
		ExpiresAt:        token.ExpiresAt,
		Revoked:          token.Revoked(),
		CreatedAt:        token.CreatedAt,
		CreatedBy:        token.CreatedBy,
	}
	if !token.LastUsedAt.IsZero() {
		used := token.LastUsedAt
		view.LastUsedAt = &used
	}
	return view
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

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
