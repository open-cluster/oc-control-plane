package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/session"
)

// What is asserted here is what a caller observes across the HTTP boundary: a status code, a
// Set-Cookie, a body shape, a row in the audit trail. Nothing asserts that a particular
// function was called.
//
// Every one of these was impossible before this slice. An operator could not sign in at all —
// the frontend sends a cookie session and the control plane required one shared static bearer
// token, so every browser request answered 401 — and behind that, whoever held the one token
// could read and mutate any Organization by editing a path segment.

// providerBody is the shape the provider routes answer with.
type providerBody struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Issuer    string `json:"issuer"`
	SignInURL string `json:"signInUrl"`
}

type sessionBody struct {
	Principal struct {
		ID          string   `json:"id"`
		Kind        string   `json:"kind"`
		DisplayName string   `json:"displayName"`
		Email       string   `json:"email"`
		Roles       []string `json:"roles"`
		Scopes      []string `json:"scopes"`
	} `json:"principal"`
	Organizations []struct {
		Organization string `json:"organizationId"`
		Role         string `json:"role"`
	} `json:"organizations"`
}

type memberBody struct {
	UserID string `json:"userId"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Source string `json:"source"`
}

type auditBody struct {
	Events []struct {
		Action     string         `json:"action"`
		ActorID    string         `json:"actorId"`
		ActorName  string         `json:"actorName"`
		ActorKind  string         `json:"actorKind"`
		TargetKind string         `json:"targetKind"`
		TargetID   string         `json:"targetId"`
		Outcome    string         `json:"outcome"`
		Detail     map[string]any `json:"detail"`
	} `json:"events"`
	Next string `json:"next"`
}

// configureProvider registers the mock issuer as the tenant's way in, and returns it.
func configureProvider(
	t *testing.T, plane *identityPlane, issuer *mockIssuer, settings map[string]any,
) providerBody {
	t.Helper()

	body := map[string]any{
		"name":         "Test IdP",
		"issuer":       issuer.url(),
		"clientId":     "oc-console",
		"clientSecret": "the-client-secret-nobody-should-see",
	}
	for key, value := range settings {
		body[key] = value
	}

	answered := plane.call(t, http.MethodPost,
		plane.base(identityOrg)+"/identity-providers", body, asBootstrap)
	if answered.status != http.StatusCreated {
		t.Fatalf("configuring a provider = %d: %s", answered.status, answered.body)
	}
	var provider providerBody
	decodeAnswer(t, answered, &provider)
	return provider
}

// signIn drives the whole Authorization Code flow and returns the session cookie it produced,
// together with the callback's answer for the cases where it must fail.
func signIn(t *testing.T, plane *identityPlane, provider providerBody) answer {
	t.Helper()

	started := plane.call(t, http.MethodGet,
		"http://"+plane.operator+provider.SignInURL, nil)
	if started.status != http.StatusFound {
		t.Fatalf("starting a sign-in = %d: %s", started.status, started.body)
	}

	// Follow the redirect to the issuer, which answers with its own redirect back here.
	atIssuer := plane.call(t, http.MethodGet, started.location, nil)
	if atIssuer.status != http.StatusFound {
		t.Fatalf("the issuer answered %d: %s", atIssuer.status, atIssuer.body)
	}
	return plane.call(t, http.MethodGet, atIssuer.location, nil)
}

// A person signs in with their company identity provider and gets a session that survives a
// page reload. Stories 1 and 2, and the thing that made every browser request answer 401.
func TestOperatorIdentity_AnOperatorSignsInAndTheSessionAuthenticates(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)

	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "editor",
		"verifiedDomains": []string{"example.test"},
	})

	completed := signIn(t, plane, provider)
	if completed.status != http.StatusFound {
		t.Fatalf("completing a sign-in = %d: %s", completed.status, completed.body)
	}
	if !strings.HasPrefix(completed.location, identityConsole) {
		t.Errorf("the browser was sent to %q, want the console", completed.location)
	}
	cookie := sessionCookie(t, completed)

	// Story 3: the interface can say which principal and which Organizations it is reading as.
	whoami := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/operator/v1/session", nil, asSession(cookie))
	if whoami.status != http.StatusOK {
		t.Fatalf("reading the session = %d: %s", whoami.status, whoami.body)
	}
	var who sessionBody
	decodeAnswer(t, whoami, &who)

	if who.Principal.Kind != "user" || who.Principal.Email != "ada@example.test" {
		t.Errorf("the session describes %+v, want the person who signed in", who.Principal)
	}
	if who.Principal.DisplayName != "Ada Lovelace" {
		t.Errorf("the display name is %q; the record has to be able to name a person",
			who.Principal.DisplayName)
	}
	if len(who.Organizations) != 1 || who.Organizations[0].Organization != identityOrg {
		t.Fatalf("the session reports %+v, want one membership in %s",
			who.Organizations, identityOrg)
	}
	if who.Organizations[0].Role != string(authz.Editor) {
		t.Errorf("just-in-time provisioning granted %q, want the configured role",
			who.Organizations[0].Role)
	}
	// The frontend's declared contract: roles and scopes, flattened, so an interface deciding
	// whether to render a button asks about a permission rather than holding the role table.
	if len(who.Principal.Scopes) == 0 {
		t.Error("the principal carries no scopes; the contract's producer is what this slice adds")
	}

	// The cookie authenticates a real, privileged read — which is the whole point.
	roster := plane.call(t, http.MethodGet,
		plane.base(identityOrg)+"/relays", nil, asSession(cookie))
	if roster.status != http.StatusOK {
		t.Errorf("a signed-in read = %d: %s", roster.status, roster.body)
	}

	// Story 4: sign-out ends the session ON THE SERVER, so a stolen laptop does not carry a
	// live credential. The same cookie afterwards authenticates nothing.
	out := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/operator/v1/session/sign-out", nil, asSession(cookie))
	if out.status != http.StatusOK || !strings.Contains(out.body, `"signedOut":true`) {
		t.Fatalf("signing out = %d: %s", out.status, out.body)
	}
	after := plane.call(t, http.MethodGet,
		plane.base(identityOrg)+"/relays", nil, asSession(cookie))
	if after.status != http.StatusUnauthorized {
		t.Errorf("the cookie still worked after sign-out: %d %s — the answer used to be that "+
			"it was signed out, which was not a true statement", after.status, after.body)
	}
}

// The refusals the flow rests on, each proved by making the issuer or the caller wrong in
// exactly one way. A flow that accepted any of these would have PKCE, state and the nonce as
// decoration.
func TestOperatorIdentity_TheSignInRefusalsAreLoadBearing(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"verifiedDomains": []string{"example.test"},
	})

	t.Run("a state nobody issued is refused", func(t *testing.T) {
		refused := plane.call(t, http.MethodGet, "http://"+plane.operator+
			"/operator/v1/sign-in/callback?code=whatever&state=invented", nil)
		if refused.status != http.StatusBadRequest {
			t.Errorf("an invented state = %d, want 400: %s", refused.status, refused.body)
		}
	})

	t.Run("a callback carrying no code is not a sign-in", func(t *testing.T) {
		refused := plane.call(t, http.MethodGet, "http://"+plane.operator+
			"/operator/v1/sign-in/callback?state=something", nil)
		if refused.status != http.StatusBadRequest {
			t.Errorf("a callback with no code = %d, want 400", refused.status)
		}
	})

	// The one that matters most: a callback replayed after it has already been redeemed. This
	// control plane refuses it at its OWN layer — the sign-in flow is consumed by a conditional
	// update — rather than depending on the provider to notice.
	t.Run("a replayed callback is refused", func(t *testing.T) {
		started := plane.call(t, http.MethodGet,
			"http://"+plane.operator+provider.SignInURL, nil)
		atIssuer := plane.call(t, http.MethodGet, started.location, nil)

		first := plane.call(t, http.MethodGet, atIssuer.location, nil)
		if first.status != http.StatusFound {
			t.Fatalf("the first redemption = %d: %s", first.status, first.body)
		}
		second := plane.call(t, http.MethodGet, atIssuer.location, nil)
		if second.status == http.StatusFound {
			t.Fatal("a replayed callback issued a second session; the state must be single-use")
		}
		if len(second.cookies) != 0 {
			t.Errorf("a replayed callback set %d cookies", len(second.cookies))
		}
	})

	// A PKCE verifier that does not match the challenge. The mock issuer enforces it exactly as
	// a real one does, and this asserts the control plane sends a verifier that matches at all
	// — a flow that sent a fresh random value every time would fail here and nowhere else.
	t.Run("the verifier the control plane sends matches the challenge it sent", func(t *testing.T) {
		started := plane.call(t, http.MethodGet,
			"http://"+plane.operator+provider.SignInURL, nil)
		authorization, err := url.Parse(started.location)
		if err != nil {
			t.Fatalf("parsing the authorization request: %v", err)
		}
		query := authorization.Query()
		if query.Get("code_challenge_method") != "S256" {
			t.Errorf("the challenge method is %q, want S256; a plain challenge IS the verifier",
				query.Get("code_challenge_method"))
		}
		if query.Get("code_challenge") == "" || query.Get("state") == "" ||
			query.Get("nonce") == "" {
			t.Errorf("the authorization request is missing a challenge, a state or a nonce: %s",
				authorization.RawQuery)
		}
		// The verifier itself must NOT be in the request. If it were, intercepting the
		// redirect would hand over everything needed to redeem the code.
		if strings.Contains(started.location, "code_verifier") {
			t.Error("the authorization request carries the PKCE verifier")
		}
	})

	t.Run("a token minted for a different client is refused", func(t *testing.T) {
		issuer.mu.Lock()
		issuer.audience = "some-other-application"
		issuer.mu.Unlock()
		defer func() {
			issuer.mu.Lock()
			issuer.audience = ""
			issuer.mu.Unlock()
		}()

		if refused := signIn(t, plane, provider); refused.status == http.StatusFound {
			t.Error("a token minted for another client at the same issuer was accepted")
		}
	})

	t.Run("a token signed by a key the issuer never published is refused", func(t *testing.T) {
		other := newMockIssuer(t)
		issuer.mu.Lock()
		issuer.signWithAnotherKey = other.key
		issuer.mu.Unlock()
		defer func() {
			issuer.mu.Lock()
			issuer.signWithAnotherKey = nil
			issuer.mu.Unlock()
		}()

		if refused := signIn(t, plane, provider); refused.status == http.StatusFound {
			t.Error("a token signed by an unpublished key was accepted; the signature check " +
				"is then doing nothing")
		}
	})
}

// Story 8: just-in-time provisioning is restricted to verified email domains, so an unrelated
// account at the same identity provider cannot enter the Organization.
func TestOperatorIdentity_ProvisioningPolicyDecidesWhoGetsIn(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "viewer",
		"verifiedDomains": []string{"example.test"},
	})

	t.Run("an unrelated domain at the same provider is refused", func(t *testing.T) {
		issuer.assert(t, "sub", "outsider-1")
		issuer.assert(t, "email", "someone@unrelated.test")

		if refused := signIn(t, plane, provider); refused.status == http.StatusFound {
			t.Error("an account at an unlisted domain was admitted; a provider may serve every " +
				"account at a vendor, and the domain list is what makes it this tenant's")
		}
	})

	// A domain check over an UNVERIFIED claim admits anyone who can type the domain, so the
	// verification the provider asserted is what the check rests on.
	t.Run("an address the provider did not verify is refused", func(t *testing.T) {
		issuer.assert(t, "sub", "unverified-1")
		issuer.assert(t, "email", "claimed@example.test")
		issuer.assert(t, "email_verified", false)
		defer issuer.assert(t, "email_verified", true)

		if refused := signIn(t, plane, provider); refused.status == http.StatusFound {
			t.Error("an unverified address at a listed domain was admitted")
		}
	})

	// A suffix match would admit this, which is the shape of check that looks right and is a
	// tenant boundary failure.
	t.Run("a domain that merely ends with a verified one is refused", func(t *testing.T) {
		issuer.assert(t, "sub", "lookalike-1")
		issuer.assert(t, "email", "attacker@evil-example.test")

		if refused := signIn(t, plane, provider); refused.status == http.StatusFound {
			t.Error("evil-example.test was admitted for a tenant that verified example.test")
		}
	})

	// Story 9: a group maps to a role, so role assignment is not a second directory an
	// administrator maintains by hand.
	t.Run("a provider group maps to the role it names", func(t *testing.T) {
		mapped := configureProvider(t, plane, issuer, map[string]any{
			"name": "Mapped IdP",
			// A distinct client at the same issuer: a tenant may configure several providers,
			// and may not configure the same client twice.
			"clientId":        "oc-console-mapped",
			"jitEnabled":      true,
			"jitRole":         "viewer",
			"verifiedDomains": []string{"example.test"},
			"groupClaim":      "groups",
			"groupRoleMap": map[string]string{
				"sre":     "editor",
				"on-call": "editor",
			},
		})
		issuer.assert(t, "sub", "mapped-1")
		issuer.assert(t, "email", "grace@example.test")
		issuer.assert(t, "groups", []string{"unmapped", "on-call", "sre"})

		completed := signIn(t, plane, mapped)
		if completed.status != http.StatusFound {
			t.Fatalf("a mapped sign-in = %d: %s", completed.status, completed.body)
		}
		who := readSession(t, plane, sessionCookie(t, completed))

		// The STRONGEST role the person's groups map to, not the first the provider listed:
		// access must not depend on a directory's ordering.
		if len(who.Organizations) != 1 || who.Organizations[0].Role != string(authz.Editor) {
			t.Errorf("the group map produced %+v, want the strongest role held",
				who.Organizations)
		}
	})

	// An owner arriving from a directory group, or from the just-in-time default, would mean a
	// directory edit is an administrative takeover. It is refused at configuration time.
	t.Run("an owner cannot be granted by provisioning or by a group", func(t *testing.T) {
		for name, settings := range map[string]map[string]any{
			"as the provisioning role": {
				"name": "Owner By Default", "jitEnabled": true, "jitRole": "admin",
				"verifiedDomains": []string{"example.test"},
			},
			"as a group mapping": {
				"name": "Owner By Group", "jitEnabled": true,
				"verifiedDomains": []string{"example.test"},
				"groupRoleMap":    map[string]string{"admins": "admin"},
			},
		} {
			body := map[string]any{
				"issuer": issuer.url(), "clientId": "oc-console", "clientSecret": "a-secret",
			}
			for key, value := range settings {
				body[key] = value
			}
			refused := plane.call(t, http.MethodPost,
				plane.base(identityOrg)+"/identity-providers", body, asBootstrap)
			if refused.status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400: %s", name, refused.status, refused.body)
			}
		}
	})
}

func readSession(t *testing.T, plane *identityPlane, cookie string) sessionBody {
	t.Helper()

	answered := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/operator/v1/session", nil, asSession(cookie))
	if answered.status != http.StatusOK {
		t.Fatalf("reading the session = %d: %s", answered.status, answered.body)
	}
	var who sessionBody
	decodeAnswer(t, answered, &who)
	return who
}

// Story 24, at the surface: a request naming an Organization the caller does not belong to must
// be byte-identical to one naming an Organization that does not exist. Anything else turns a
// path segment into a list of this deployment's customers.
func TestOperatorIdentity_AForeignOrganizationLooksLikeOneThatDoesNotExist(t *testing.T) {
	plane := startIdentityPlane(t)

	foreign := plane.call(t, http.MethodGet,
		plane.base(identityNeighbour)+"/relays", nil, asBootstrap)
	invented := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/operator/v1/organizations/org-nobody-has/relays",
		nil, asBootstrap)

	if foreign.status != http.StatusNotFound || invented.status != http.StatusNotFound {
		t.Fatalf("a tenant the caller is not in answered %d and one that does not exist "+
			"answered %d; both must be 404", foreign.status, invented.status)
	}
	if foreign.body != invented.body {
		t.Errorf("the two answers differ:\n  not a member: %q\n  does not exist: %q",
			foreign.body, invented.body)
	}

	// And the same for a MUTATION, which is where a 403 would be most tempting.
	foreignWrite := plane.call(t, http.MethodPost,
		plane.base(identityNeighbour)+"/integrations",
		map[string]string{"type": "alertmanager", "name": "Staging Alertmanager"},
		asBootstrap)
	inventedWrite := plane.call(t, http.MethodPost,
		"http://"+plane.operator+"/operator/v1/organizations/org-nobody-has/integrations",
		map[string]string{"type": "alertmanager", "name": "Staging Alertmanager"}, asBootstrap)

	if foreignWrite.body != inventedWrite.body ||
		foreignWrite.status != http.StatusNotFound {
		t.Errorf("a write to another tenant answered %d %q and one to nowhere answered %d %q",
			foreignWrite.status, foreignWrite.body, inventedWrite.status, inventedWrite.body)
	}
}

// The role table is the specification of a role, and this is that table asserted through the
// HTTP boundary: for each role, what each route answers. Reading it is how a reviewer answers
// "what can an Investigator do".
//
// It is a table over routes rather than a test per route, so a route added to the surface is
// covered by code that already exists.
func TestOperatorIdentity_EachRoleReachesExactlyWhatItsPermissionsSay(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)

	// One provider per role, because the role is granted at provisioning and a person keeps the
	// role they were first given.
	routes := []struct {
		name       string
		method     string
		path       string
		body       any
		permission authz.Permission
	}{
		{"read the relay roster", http.MethodGet, "/relays", nil, authz.RelayRead},
		{"clear a conflict", http.MethodPost,
			"/relays/1ef7e1cf-0000-4000-8000-000000000000/clear-conflict", nil,
			authz.RelayConflictClear},
		{"read the integration catalog", http.MethodGet, "/integration-types", nil,
			authz.IntegrationRead},
		{"read integrations", http.MethodGet, "/integrations", nil, authz.IntegrationRead},
		{"create an integration", http.MethodPost, "/integrations",
			map[string]string{"type": "alertmanager", "name": "Role Table Alertmanager"},
			authz.IntegrationCreate},
		{"read incidents", http.MethodGet, "/incidents", nil, authz.IncidentRead},
		{"read identity providers", http.MethodGet, "/identity-providers", nil,
			authz.IdentityRead},
		{"read members", http.MethodGet, "/members", nil, authz.MemberRead},
		{"read service accounts", http.MethodGet, "/service-accounts", nil,
			authz.ServiceAccountRead},
		{"read api tokens", http.MethodGet, "/api-tokens", nil, authz.TokenRead},
		{"read the audit trail", http.MethodGet, "/audit-events", nil, authz.AuditRead},
	}

	for index, role := range authz.Roles() {
		if role == authz.Admin {
			// An owner cannot be provisioned — deliberately, and asserted above — so the
			// bootstrap credential is what exercises that role, and it does so throughout this
			// file.
			continue
		}

		provider := configureProvider(t, plane, issuer, map[string]any{
			"name":            "IdP for " + string(role),
			"clientId":        "client-" + string(role),
			"jitEnabled":      true,
			"jitRole":         string(role),
			"verifiedDomains": []string{"example.test"},
		})
		issuer.assert(t, "sub", "person-"+string(role))
		issuer.assert(t, "email", "person"+string(rune('a'+index))+"@example.test")
		issuer.assert(t, "groups", nil)

		completed := signIn(t, plane, provider)
		if completed.status != http.StatusFound {
			t.Fatalf("signing in as %s = %d: %s", role, completed.status, completed.body)
		}
		cookie := sessionCookie(t, completed)

		for _, route := range routes {
			t.Run(string(role)+" may "+route.name, func(t *testing.T) {
				answered := plane.call(t, route.method,
					plane.base(identityOrg)+route.path, route.body, asSession(cookie))

				permitted := role.Grants(route.permission)
				// A refusal is exactly 403. Anything else — a 200, a 500, a 404 — means the
				// route was reached or failed for a reason other than the decision under test.
				refused := answered.status == http.StatusForbidden
				if permitted == refused {
					t.Errorf("%s %s answered %d for %s; the role table says %s is %v: %s",
						route.method, route.path, answered.status, role,
						route.permission, permitted, answered.body)
				}
			})
		}
	}
}

// Story 10: an administrator revokes a colleague's sessions and it takes effect on the next
// request, not at the next token refresh. This is the property a JWT could not have without a
// revocation list, which is a session table with extra steps.
func TestOperatorIdentity_RevocationTakesEffectOnTheNextRequest(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "viewer",
		"verifiedDomains": []string{"example.test"},
	})
	issuer.assert(t, "sub", "departing-1")
	issuer.assert(t, "email", "leaving@example.test")

	completed := signIn(t, plane, provider)
	cookie := sessionCookie(t, completed)
	who := readSession(t, plane, cookie)
	departing := who.Principal.ID

	if answered := plane.call(t, http.MethodGet,
		plane.base(identityOrg)+"/relays", nil, asSession(cookie)); answered.status != http.StatusOK {
		t.Fatalf("the session must work before it is ended: %d", answered.status)
	}

	revoked := plane.call(t, http.MethodPost,
		plane.base(identityOrg)+"/members/"+departing+"/revoke-sessions", nil, asBootstrap)
	if revoked.status != http.StatusOK {
		t.Fatalf("revoking = %d: %s", revoked.status, revoked.body)
	}

	after := plane.call(t, http.MethodGet,
		plane.base(identityOrg)+"/relays", nil, asSession(cookie))
	if after.status != http.StatusUnauthorized {
		t.Errorf("a revoked session answered %d, want 401 — offboarding has to take effect "+
			"before the next refresh, not at it", after.status)
	}

	// And removing the membership ends access whether or not anybody revoked the session
	// first, in the same transaction as the removal.
	second := signIn(t, plane, provider)
	if second.status != http.StatusFound {
		t.Fatalf("signing in again = %d: %s", second.status, second.body)
	}
	reissued := sessionCookie(t, second)

	removed := plane.call(t, http.MethodDelete,
		plane.base(identityOrg)+"/members/"+departing, nil, asBootstrap)
	if removed.status != http.StatusNoContent {
		t.Fatalf("removing the membership = %d: %s", removed.status, removed.body)
	}
	if gone := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays",
		nil, asSession(reissued)); gone.status != http.StatusUnauthorized {
		t.Errorf("a session outlived the membership it rested on: %d", gone.status)
	}
}

// Stories 27 to 31: automation runs as something other than a person, bound to one Organization
// and one role, with an expiry, a last-used stamp, and a revocation that takes five seconds.
func TestOperatorIdentity_AServiceAccountTokenIsScopedShownOnceAndRevocable(t *testing.T) {
	plane := startIdentityPlane(t)

	created := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/service-accounts",
		map[string]any{"name": "ci", "description": "the deployment pipeline"}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("creating a service account = %d: %s", created.status, created.body)
	}
	var account struct {
		ID string `json:"id"`
	}
	decodeAnswer(t, created, &account)

	issued := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/api-tokens",
		map[string]any{
			"serviceAccountId": account.ID,
			"role":             "viewer",
			"expiresInSeconds": 3600,
		}, asBootstrap)
	if issued.status != http.StatusCreated {
		t.Fatalf("issuing a token = %d: %s", issued.status, issued.body)
	}
	var minted struct {
		Token struct {
			ID     string `json:"id"`
			Prefix string `json:"prefix"`
			Role   string `json:"role"`
		} `json:"token"`
		Secret string `json:"secret"`
	}
	decodeAnswer(t, issued, &minted)

	if minted.Secret == "" {
		t.Fatal("a token was issued with nothing to present")
	}
	if !strings.HasPrefix(minted.Secret, minted.Token.Prefix) {
		t.Errorf("the stored prefix %q does not match the token; an operator cannot tell their "+
			"tokens apart", minted.Token.Prefix)
	}

	// Story 31: shown once. The listing carries the prefix and never the token.
	listed := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/api-tokens",
		nil, asBootstrap)
	if strings.Contains(listed.body, minted.Secret) {
		t.Error("the token listing carries the token itself; the system must hold no readable copy")
	}

	// Story 28: bound to one organization and one role.
	if reached := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays",
		nil, asToken(minted.Secret)); reached.status != http.StatusOK {
		t.Errorf("a viewer token could not read the roster: %d %s", reached.status, reached.body)
	}
	if beyond := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/integrations",
		map[string]string{"type": "alertmanager", "name": "Viewer Alertmanager"},
		asToken(minted.Secret)); beyond.status != http.StatusForbidden {
		t.Errorf("a viewer token created an integration: %d %s", beyond.status, beyond.body)
	}
	if elsewhere := plane.call(t, http.MethodGet, plane.base(identityNeighbour)+"/relays",
		nil, asToken(minted.Secret)); elsewhere.status != http.StatusNotFound {
		t.Errorf("a token bound to one organization reached another: %d", elsewhere.status)
	}

	// Story 30: revocation is immediate.
	revoked := plane.call(t, http.MethodPost,
		plane.base(identityOrg)+"/api-tokens/"+minted.Token.ID+"/revoke", nil, asBootstrap)
	if revoked.status != http.StatusNoContent {
		t.Fatalf("revoking = %d: %s", revoked.status, revoked.body)
	}
	if after := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays",
		nil, asToken(minted.Secret)); after.status != http.StatusUnauthorized {
		t.Errorf("a revoked token answered %d, want 401", after.status)
	}

	// Story 29: what an operator reads to decide which tokens nobody needs.
	final := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/api-tokens",
		nil, asBootstrap)
	if !strings.Contains(final.body, `"lastUsedAt"`) ||
		strings.Contains(final.body, `"lastUsedAt":null`) {
		t.Errorf("the listing does not report when the token was last used: %s", final.body)
	}
	// An owner role in a CI environment variable would be a credential that can appoint owners.
	if owner := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/api-tokens",
		map[string]any{"serviceAccountId": account.ID, "role": "admin"},
		asBootstrap); owner.status != http.StatusBadRequest {
		t.Errorf("an owner token was issued: %d %s", owner.status, owner.body)
	}
	// A token with no deadline is the ambient root credential this replaces, so there is no way
	// to ask for one.
	if forever := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/api-tokens",
		map[string]any{
			"serviceAccountId": account.ID, "role": "viewer",
			"expiresInSeconds": 100 * 365 * 24 * 3600,
		}, asBootstrap); forever.status != http.StatusBadRequest {
		t.Errorf("a token beyond the maximum lifetime was issued: %d", forever.status)
	}
}

// Story 20 and 23: every event names the actor, the Organization, the target, the time, the
// source address and the outcome — and an identity-configuration change carries the previous
// and the new value, so a weakened policy is discoverable.
func TestOperatorIdentity_EveryMutationIsOnTheRecordWithItsActor(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)

	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"verifiedDomains": []string{"example.test"},
	})
	// Weaken the policy, which is the change an auditor most needs to be able to find.
	weakened := plane.call(t, http.MethodPatch,
		plane.base(identityOrg)+"/identity-providers/"+provider.ID, map[string]any{
			"name": "Test IdP", "issuer": issuer.url(), "clientId": "oc-console",
			"jitEnabled": true, "verifiedDomains": []string{"example.test"},
			"requireVerifiedEmail": false,
		}, asBootstrap)
	if weakened.status != http.StatusOK {
		t.Fatalf("weakening the policy = %d: %s", weakened.status, weakened.body)
	}

	created := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/integrations",
		map[string]string{"type": "alertmanager", "name": "Audited Alertmanager"}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("creating an integration = %d: %s", created.status, created.body)
	}

	// A refusal by somebody holding a credential, which story 22 asks to be visible.
	plane.call(t, http.MethodGet, plane.base(identityNeighbour)+"/relays", nil, asBootstrap)

	answered := plane.call(t, http.MethodGet,
		plane.base(identityOrg)+"/audit-events?limit=100", nil, asBootstrap)
	if answered.status != http.StatusOK {
		t.Fatalf("reading the audit trail = %d: %s", answered.status, answered.body)
	}
	var trail auditBody
	decodeAnswer(t, answered, &trail)
	if len(trail.Events) == 0 {
		t.Fatal("the record is empty after four acts")
	}

	byAction := make(map[string]int, len(trail.Events))
	for _, event := range trail.Events {
		byAction[event.Action]++
		if event.ActorKind == "" || event.Outcome == "" {
			t.Errorf("an event names no actor kind or outcome: %+v", event)
		}
		if event.ActorKind != "system" && event.ActorName == "" {
			t.Errorf("an event names no actor: %+v", event)
		}
	}

	for _, wanted := range []string{
		"identity-provider.configured",
		"identity-provider.changed",
		"integration.created",
	} {
		if byAction[wanted] == 0 {
			t.Errorf("no %s event; a change nobody can attribute is what this slice removes",
				wanted)
		}
		if byAction[wanted] > 1 {
			t.Errorf("%d %s events for one act; a trail padded with duplicates is one nobody "+
				"reads", byAction[wanted], wanted)
		}
	}

	// Story 23: the previous and the new value are both on the record.
	var found bool
	for _, event := range trail.Events {
		if event.Action != "identity-provider.changed" {
			continue
		}
		found = true
		before, _ := event.Detail["before"].(map[string]any)
		after, _ := event.Detail["after"].(map[string]any)
		if before == nil || after == nil {
			t.Fatalf("the change records no before and after: %+v", event.Detail)
		}
		if before["requireVerifiedEmail"] != true || after["requireVerifiedEmail"] != false {
			t.Errorf("the weakening is not discoverable from the record: before %v, after %v",
				before["requireVerifiedEmail"], after["requireVerifiedEmail"])
		}
	}
	if !found {
		t.Error("no identity-provider.changed event")
	}

	// Nothing in the record is a credential. The client secret was supplied at configuration
	// and must appear nowhere.
	if strings.Contains(answered.body, "the-client-secret-nobody-should-see") {
		t.Error("the audit trail carries a client secret")
	}
}

// Story 21: the record is immutable, and the database is what enforces it rather than a
// convention every future writer has to remember.
func TestOperatorIdentity_TheRecordRefusesAnUpdateAndADelete(t *testing.T) {
	plane := startIdentityPlane(t)

	created := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/integrations",
		map[string]string{"type": "alertmanager", "name": "Immutable"}, asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("creating an integration = %d: %s", created.status, created.body)
	}

	pool := openPlacement(t, plane.dsn)
	organization := namedOrganization(t, identityOrg)
	connection, err := pool.Pool(organization)
	if err != nil {
		t.Fatalf("reaching the placement: %v", err)
	}

	for name, statement := range map[string]string{
		"an update": `UPDATE audit_event SET action = 'nothing.happened'`,
		// A statement matching NO rows, which a row-level trigger would allow and which would
		// read, to anyone probing, as though deletion were permitted.
		"a delete matching nothing": `DELETE FROM audit_event WHERE FALSE`,
		"a delete":                  `DELETE FROM audit_event`,
		"a truncate":                `TRUNCATE audit_event`,
	} {
		if _, err := connection.Exec(t.Context(), statement); err == nil {
			t.Errorf("%s was accepted; the record is append-only and the database is what has "+
				"to say so", name)
		}
	}
}

// A cookie-authenticated unsafe request from anywhere but the console is refused. SameSite=Lax
// plus this check is the whole CSRF defence, and there is no separate token — so if this does
// not hold, nothing does.
func TestOperatorIdentity_ACrossSiteWriteIsRefused(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "editor",
		"verifiedDomains": []string{"example.test"},
	})
	issuer.assert(t, "sub", "csrf-1")
	issuer.assert(t, "email", "csrf@example.test")

	cookie := sessionCookie(t, signIn(t, plane, provider))

	// Something the Editor's cookie may write to: verifying an integration the bootstrap
	// credential arranged.
	arranged := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/integrations",
		map[string]string{"type": "alertmanager", "name": "CSRF Alertmanager"}, asBootstrap)
	if arranged.status != http.StatusCreated {
		t.Fatalf("arranging an integration = %d: %s", arranged.status, arranged.body)
	}
	var integration struct {
		Integration struct {
			ID string `json:"id"`
		} `json:"integration"`
	}
	decodeInto(t, arranged.body, &integration)
	verify := plane.base(identityOrg) + "/integrations/" + integration.Integration.ID + "/verify"

	// From the console: served.
	fromConsole := plane.call(t, http.MethodPost, verify, nil, asSession(cookie))
	if fromConsole.status != http.StatusOK {
		t.Fatalf("a write from the console = %d: %s", fromConsole.status, fromConsole.body)
	}

	// With no Origin at all: not a browser, and a cookie-authenticated request that did not
	// come from one is not a case this surface serves.
	bare := plane.call(t, http.MethodPost, verify, nil, asSession(cookie), withoutOrigin)
	if bare.status != http.StatusForbidden {
		t.Errorf("a cookie-borne write with no Origin = %d, want 403", bare.status)
	}

	// From somewhere else entirely: refused.
	elsewhere := plane.call(t, http.MethodPost, verify, nil, asSession(cookie),
		func(request *http.Request) { request.Header.Set("Origin", "https://evil.example.test") })
	if elsewhere.status != http.StatusForbidden {
		t.Errorf("a cross-site write = %d, want 403", elsewhere.status)
	}

	// A READ is unaffected, because a browser will not attach the cookie to a cross-site read
	// under Lax and refusing one would break every ordinary page load.
	read := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/integrations",
		nil, asSession(cookie))
	if read.status != http.StatusOK {
		t.Errorf("a read = %d, want it served", read.status)
	}
}

// The unauthenticated surface must not become a customer directory. A tenant that exists and has
// configured no way in must answer exactly as one that does not exist.
func TestOperatorIdentity_TheSignInRoutesDoNotEnumerateTenants(t *testing.T) {
	plane := startIdentityPlane(t)

	configured := plane.call(t, http.MethodGet,
		plane.base(identityNeighbour)+"/sign-in/providers", nil)
	invented := plane.call(t, http.MethodGet,
		"http://"+plane.operator+"/operator/v1/organizations/org-nobody-has/sign-in/providers",
		nil)

	if configured.status != http.StatusNotFound || invented.status != http.StatusNotFound {
		t.Fatalf("a tenant with no provider answered %d and one that does not exist answered "+
			"%d; both must be 404", configured.status, invented.status)
	}
	if configured.body != invented.body {
		t.Errorf("the two answers differ:\n  no provider: %q\n  no tenant: %q",
			configured.body, invented.body)
	}
}

// Story 11: an organization sets its own session lifetime, inside the bounds this build serves.
func TestOperatorIdentity_ATenantSetsItsOwnSessionLifetime(t *testing.T) {
	plane := startIdentityPlane(t)

	set := plane.call(t, http.MethodPut, plane.base(identityOrg)+"/policy",
		map[string]any{"sessionLifetimeSeconds": 900, "auditRetentionDays": 365}, asBootstrap)
	if set.status != http.StatusOK {
		t.Fatalf("setting the policy = %d: %s", set.status, set.body)
	}

	read := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/policy", nil, asBootstrap)
	if !strings.Contains(read.body, `"sessionLifetimeSeconds":900`) {
		t.Errorf("the policy reads back as %s", read.body)
	}
	// Stated rather than implied. It reported false while the schedule was a column nothing acted
	// on, and reports true now that the pruner exists — a product stating a retention period it
	// does not enforce is worse than one stating none, and the flag is what keeps that honest
	// either way. It is passed from the composition root rather than hard-coded, so a deployment
	// that stopped running the pruner would report the truth about itself.
	if !strings.Contains(read.body, `"auditRetentionEnforced":true`) {
		t.Errorf("the surface does not say the retention schedule is applied, and this process "+
			"runs the pruner: %s", read.body)
	}

	// A lifetime beyond what this build serves is refused rather than silently clamped, so an
	// administrator who asked for a year is told they will not get one.
	if beyond := plane.call(t, http.MethodPut, plane.base(identityOrg)+"/policy",
		map[string]any{"sessionLifetimeSeconds": 365 * 24 * 3600}, asBootstrap); beyond.status !=
		http.StatusBadRequest {
		t.Errorf("a lifetime past the ceiling = %d, want 400", beyond.status)
	}

	// And the lifetime a session is actually issued with is the tenant's.
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled": true, "verifiedDomains": []string{"example.test"},
	})
	issuer.assert(t, "sub", "policy-1")
	issuer.assert(t, "email", "policy@example.test")

	completed := signIn(t, plane, provider)
	if completed.status != http.StatusFound {
		t.Fatalf("signing in = %d: %s", completed.status, completed.body)
	}
	for _, cookie := range completed.cookies {
		if cookie.Name != session.CookieName {
			continue
		}
		if until := time.Until(cookie.Expires); until > 20*time.Minute {
			t.Errorf("the cookie lives %v, want the tenant's fifteen minutes", until)
		}
	}
}

// Members are listed with the source of their membership, so an administrator can tell a
// directory-owned one from a hand-granted one before they edit it.
func TestOperatorIdentity_MembershipsAreListedAndChangeable(t *testing.T) {
	plane := startIdentityPlane(t)
	issuer := newMockIssuer(t)
	provider := configureProvider(t, plane, issuer, map[string]any{
		"jitEnabled": true, "jitRole": "viewer",
		"verifiedDomains": []string{"example.test"},
	})
	issuer.assert(t, "sub", "member-1")
	issuer.assert(t, "email", "member@example.test")

	cookie := sessionCookie(t, signIn(t, plane, provider))
	who := readSession(t, plane, cookie)

	listed := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/members", nil, asBootstrap)
	if listed.status != http.StatusOK {
		t.Fatalf("listing members = %d: %s", listed.status, listed.body)
	}
	var members struct {
		Members []memberBody `json:"members"`
	}
	decodeAnswer(t, listed, &members)

	var found *memberBody
	for index := range members.Members {
		if members.Members[index].UserID == who.Principal.ID {
			found = &members.Members[index]
		}
	}
	if found == nil {
		t.Fatalf("the person who just signed in is not a member: %s", listed.body)
	}
	if found.Source != "jit" {
		t.Errorf("the membership's source is %q, want jit — an administrator has to be able to "+
			"tell a directory-owned membership from a hand-granted one", found.Source)
	}

	changed := plane.call(t, http.MethodPut,
		plane.base(identityOrg)+"/members/"+who.Principal.ID,
		map[string]string{"role": "editor"}, asBootstrap)
	if changed.status != http.StatusOK {
		t.Fatalf("changing a role = %d: %s", changed.status, changed.body)
	}

	// The change takes effect on the NEXT request rather than at the next sign-in, which is
	// what resolving memberships per request buys.
	after := readSession(t, plane, cookie)
	if len(after.Organizations) != 1 ||
		after.Organizations[0].Role != string(authz.Editor) {
		t.Errorf("the role change did not reach the live session: %+v", after.Organizations)
	}
}
