package identity

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// ErrNotAdmitted reports a person the tenant's policy does not let in. Every reason collapses
// into one error deliberately: an unverified address, an unlisted domain and a group that maps
// to nothing are all "this provider does not admit you", and telling them apart is telling
// somebody outside the tenant what its policy is.
var ErrNotAdmitted = errors.New("this identity provider does not admit you")

// admission is what a provider's policy decided about one person.
type admission struct {
	// Role is what to grant a person the tenant has never seen. The zero role means grant
	// nothing: the person becomes a user with no membership, which signs them in to nothing
	// and lets an administrator find them rather than making them invisible.
	Role authz.Role
	// MappedFromGroup names the provider group the role came from, for the record. Empty when
	// the role is the provider's configured default.
	MappedFromGroup string
}

// admit applies a provider's policy to what the provider asserted.
//
// The order matters and is the order the stories are written in. Story 8 first: an address the
// provider did not say it verified is not evidence of a domain, so the check on the domain has
// to come after the check that the claim means anything. Then story 9: a group map is what
// keeps role assignment out of a second directory, so a mapped group beats the configured
// default. Then just-in-time provisioning itself, which is off unless the tenant turned it on.
func admit(provider storage.IdentityProvider, asserted claims) (admission, error) {
	if provider.Disabled() {
		return admission{}, ErrNotAdmitted
	}

	// A group mapping decides the role for everybody, not only for a first-time signer-in:
	// that is what makes the directory the source of truth rather than a one-time import.
	mapped, group := roleFromGroups(provider, asserted)

	if !provider.JITEnabled {
		// Provisioning is off. An existing member still signs in — their membership already
		// exists — and an unknown person gets a user row and no membership.
		return admission{Role: mapped, MappedFromGroup: group}, nil
	}

	if provider.RequireVerifiedEmail && !bool(asserted.EmailVerified) {
		return admission{}, fmt.Errorf(
			"%w: the provider did not say it verified this address", ErrNotAdmitted)
	}
	if !domainIsVerified(provider.VerifiedDomains, asserted.Email) {
		// An empty domain list admits nobody. Defaulting the other way would mean a tenant that
		// turned provisioning on and configured nothing else had admitted every account at
		// their identity provider, which is story 8's exact failure.
		return admission{}, fmt.Errorf(
			"%w: %q is not a domain this provider provisions from",
			ErrNotAdmitted, domainOf(asserted.Email))
	}

	role := mapped
	if !authz.KnownRole(role) {
		role, _ = authz.ParseRole(string(provider.JITRole))
	}
	return admission{Role: role, MappedFromGroup: group}, nil
}

// roleFromGroups resolves the strongest role the person's groups map to.
//
// Strongest rather than first: a person in both an administrators group and a viewers group is
// an administrator, and picking whichever the provider happened to list first would make their
// access depend on a directory's ordering. Nested groups are deliberately out of scope, so the
// claim is read as the flat list the provider sent.
func roleFromGroups(provider storage.IdentityProvider, asserted claims) (authz.Role, string) {
	if len(provider.GroupRoleMap) == 0 {
		return "", ""
	}
	groups := asserted.groupsFrom(provider.GroupClaim)
	if len(groups) == 0 {
		return "", ""
	}

	// authz.Roles() is ordered most privileged first, so the first declared role any group maps
	// to is the strongest one held.
	byRole := make(map[authz.Role]string, len(provider.GroupRoleMap))
	for group, name := range provider.GroupRoleMap {
		role, known := authz.ParseRole(name)
		if !known {
			// A group naming a role this build does not have maps to nothing. Refusing the
			// whole sign-in would turn a typo in a map into an outage for everyone.
			continue
		}
		for _, held := range groups {
			if strings.EqualFold(held, group) {
				byRole[role] = group
			}
		}
	}
	for _, role := range authz.Roles() {
		if group, mapped := byRole[role]; mapped {
			return role, group
		}
	}
	return "", ""
}

// domainIsVerified reports whether an address sits in one of the domains the tenant listed.
//
// The comparison is on the whole final label sequence, case-folded. A suffix match would admit
// "evil-example.com" for a tenant that listed "example.com", which is the kind of check that
// looks right and is a tenant boundary failure.
func domainIsVerified(verified []string, email string) bool {
	domain := domainOf(email)
	if domain == "" {
		return false
	}
	for _, allowed := range verified {
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(allowed, "@")), domain) {
			return true
		}
	}
	return false
}

func domainOf(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}
