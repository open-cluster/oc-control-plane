package identity

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// These cover the seams an end-to-end sign-in cannot reach cheaply: what happens to a client
// secret at rest, and the two string checks a mistake in would be a security defect that
// nothing else would catch — the open-redirect guard and the verified-domain comparison.
//
// The flow itself — PKCE, state, nonce, code replay, signature verification, provisioning
// policy — is asserted through the HTTP boundary against a mock issuer in
// cmd/controlplane/identity_test.go, because what matters there is what a caller observes.

// The client secret is the one credential in this schema that is encrypted rather than
// digested, because it has to be presented to a token endpoint rather than compared against.
// That makes the round trip load-bearing, and it makes a wrong key having to FAIL load-bearing.
func TestASealedClientSecretComesBackAndOnlyUnderItsOwnKey(t *testing.T) {
	t.Parallel()

	const secret = "the-client-secret-a-provider-issued"
	sealer := sealerWith(t, 1)

	sealed, err := sealer.Seal(secret)
	if err != nil {
		t.Fatalf("sealing: %v", err)
	}
	if strings.Contains(string(sealed), secret) {
		t.Fatal("the sealed value contains the secret")
	}

	opened, err := sealer.Open(sealed)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if opened != secret {
		t.Errorf("opened %q, want the secret back", opened)
	}

	// Sealing twice must not produce the same bytes, or the column leaks by comparison: two
	// tenants configuring the same provider would be visible as such to anyone with a dump.
	again, err := sealer.Seal(secret)
	if err != nil {
		t.Fatalf("sealing again: %v", err)
	}
	if string(again) == string(sealed) {
		t.Error("the same secret sealed twice produced the same bytes")
	}

	// A different key must FAIL rather than return something. GCM's tag is what tells a rotated
	// key apart from a tampered column, and neither is a secret to present to a provider.
	if _, err := sealerWith(t, 2).Open(sealed); err == nil {
		t.Error("another key opened the secret")
	}
	// And a tampered value likewise.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := sealer.Open(tampered); err == nil {
		t.Error("a tampered value opened")
	}
}

// A deployment with no key cannot hold a client secret, and says so rather than storing one in
// the clear.
func TestWithNoKeyNothingIsSealed(t *testing.T) {
	t.Parallel()

	var unconfigured Sealer
	if unconfigured.Configured() {
		t.Fatal("the zero sealer reports itself configured")
	}
	if _, err := unconfigured.Seal("a secret"); err == nil {
		t.Error("an unconfigured sealer sealed something")
	}
	if _, err := NewSealer(make([]byte, 16)); err == nil {
		t.Error("a 128-bit key was accepted where 256 is required")
	}
}

func sealerWith(t *testing.T, seed byte) Sealer {
	t.Helper()

	key := make([]byte, SealKeyLength)
	for index := range key {
		key[index] = seed + byte(index)
	}
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatalf("building a sealer: %v", err)
	}
	return sealer
}

// returnTo reaches a Location header. A value that got there unvalidated is an open redirect
// carrying this product's own domain, which is the shape a convincing phishing link takes.
func TestOnlyASameSitePathSurvivesAsAReturnTarget(t *testing.T) {
	t.Parallel()

	handlers := Handlers{}
	for _, testCase := range []struct {
		name     string
		asked    string
		accepted bool
	}{
		{"nothing at all becomes the root", "", true},
		{"an ordinary path", "/investigations/1", true},
		{"a path with a query", "/investigations?status=open", true},
		{"an absolute url elsewhere", "https://evil.example.com/", false},
		// The one that looks like a path and is not: a protocol-relative URL.
		{"a protocol-relative url", "//evil.example.com/", false},
		{"a backslash-smuggled host", "/\\evil.example.com", false},
		{"a scheme-relative url with credentials", "//user@evil.example.com", false},
		{"a bare host", "evil.example.com", false},
		{"a javascript url", "javascript:alert(1)", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			_, ok := handlers.returnTarget(recorder, testCase.asked)
			if ok != testCase.accepted {
				t.Errorf("returnTo=%q accepted=%v, want %v (answered %d)",
					testCase.asked, ok, testCase.accepted, recorder.Code)
			}
		})
	}
}

// The domain comparison decides who a tenant admits. A suffix match would admit
// "evil-example.com" for a tenant that listed "example.com", which is the kind of check that
// looks right and is a tenant boundary failure.
func TestAVerifiedDomainIsMatchedWholeAndCaseFolded(t *testing.T) {
	t.Parallel()

	verified := []string{"Example.test", "@second.test"}

	for _, testCase := range []struct {
		email    string
		admitted bool
	}{
		{"ada@example.test", true},
		{"ada@EXAMPLE.TEST", true},
		{"ada@second.test", true},
		{"ada@evil-example.test", false},
		{"ada@example.test.evil.test", false},
		{"ada@sub.example.test", false},
		{"ada@", false},
		{"ada", false},
		{"", false},
	} {
		if got := domainIsVerified(verified, testCase.email); got != testCase.admitted {
			t.Errorf("%q admitted=%v, want %v", testCase.email, got, testCase.admitted)
		}
	}
	// An empty list admits nobody. Defaulting the other way would mean a tenant that turned
	// provisioning on and configured nothing else had admitted every account at their provider.
	if domainIsVerified(nil, "ada@example.test") {
		t.Error("a provider with no verified domain admitted somebody")
	}
}

// A person in several mapped groups gets the STRONGEST role, not the one the provider happened
// to list first: access must not depend on a directory's ordering.
func TestAGroupMapYieldsTheStrongestRoleHeld(t *testing.T) {
	t.Parallel()

	provider := storage.IdentityProvider{
		GroupClaim: "groups",
		GroupRoleMap: map[string]string{
			"viewers":     string(authz.Viewer),
			"sre":         string(authz.Editor),
			"platform":    string(authz.Editor),
			"nonexistent": "a-role-this-build-does-not-have",
		},
	}

	for _, testCase := range []struct {
		name   string
		groups []any
		want   authz.Role
	}{
		{"one mapped group", []any{"viewers"}, authz.Viewer},
		{"the strongest of several", []any{"viewers", "sre", "platform"},
			authz.Editor},
		{"listed in the other order", []any{"platform", "viewers"},
			authz.Editor},
		{"case-folded", []any{"SRE"}, authz.Editor},
		{"a group nobody mapped", []any{"finance"}, ""},
		// A group naming a role this build no longer has maps to nothing rather than failing
		// the sign-in: a typo in a directory must not be an outage for everyone.
		{"a group mapped to nothing real", []any{"nonexistent"}, ""},
		{"no groups at all", nil, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			asserted := claims{raw: map[string]any{}}
			if testCase.groups != nil {
				asserted.raw["groups"] = testCase.groups
			}
			role, _ := roleFromGroups(provider, asserted)
			if role != testCase.want {
				t.Errorf("mapped to %q, want %q", role, testCase.want)
			}
		})
	}
}

// A provider's group claim arrives in more than one shape. A reader that handled one would fail
// against half the market for no reason a customer could act on.
func TestTheGroupClaimIsReadInTheShapesProvidersSendIt(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		value any
		want  []string
	}{
		{"a list of strings", []any{"a", "b"}, []string{"a", "b"}},
		{"a single string", "a", []string{"a"}},
		{"a list of objects with names",
			[]any{map[string]any{"name": "a"}}, []string{"a"}},
		{"something else entirely", 42, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := claims{raw: map[string]any{"groups": testCase.value}}.groupsFrom("groups")
			if len(got) != len(testCase.want) {
				t.Fatalf("read %v, want %v", got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					t.Errorf("read %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

// An issuer this process will not call. HTTPS is required because everything about the flow
// rests on the back channel reaching the host it claims to be; loopback is the one exception,
// and it cannot be reached from outside the host.
func TestAnIssuerMustBeHTTPSOrLoopback(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		issuer   string
		accepted bool
	}{
		{"https://login.example.com", true},
		{"https://login.example.com/tenant/1", true},
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://login.example.com", false},
		{"ftp://login.example.com", false},
		{"login.example.com", false},
		{"", false},
	} {
		accepted := usableIssuer(testCase.issuer) == nil
		if accepted != testCase.accepted {
			t.Errorf("%q accepted=%v, want %v", testCase.issuer, accepted, testCase.accepted)
		}
	}
}

// The provisioning policy, as one decision. The end-to-end tests prove it through the flow;
// this proves the ordering the stories depend on — verification before domain, and a group
// mapping beating the configured default.
func TestAdmissionAppliesThePolicyInTheOrderTheStoriesAskFor(t *testing.T) {
	t.Parallel()

	base := storage.IdentityProvider{
		JITEnabled:           true,
		JITRole:              authz.Viewer,
		RequireVerifiedEmail: true,
		VerifiedDomains:      []string{"example.test"},
		GroupClaim:           "groups",
		GroupRoleMap:         map[string]string{"sre": string(authz.Editor)},
	}
	verified := claims{
		Email: "ada@example.test", EmailVerified: true, raw: map[string]any{},
	}

	t.Run("a verified address at a listed domain takes the configured role", func(t *testing.T) {
		t.Parallel()
		admitted, err := admit(base, verified)
		if err != nil {
			t.Fatalf("admitting: %v", err)
		}
		if admitted.Role != authz.Viewer {
			t.Errorf("granted %q, want the configured default", admitted.Role)
		}
	})

	t.Run("a mapped group beats the configured default", func(t *testing.T) {
		t.Parallel()
		mapped := verified
		mapped.raw = map[string]any{"groups": []any{"sre"}}

		admitted, err := admit(base, mapped)
		if err != nil {
			t.Fatalf("admitting: %v", err)
		}
		if admitted.Role != authz.Editor || admitted.MappedFromGroup != "sre" {
			t.Errorf("granted %q from %q, want the mapped role and its group",
				admitted.Role, admitted.MappedFromGroup)
		}
	})

	t.Run("an unverified address is refused before the domain is looked at", func(t *testing.T) {
		t.Parallel()
		unverified := verified
		unverified.EmailVerified = false

		if _, err := admit(base, unverified); err == nil {
			t.Error("an unverified address was admitted; a domain check over an unverified " +
				"claim admits anyone who can type the domain")
		}
	})

	t.Run("a disabled provider admits nobody", func(t *testing.T) {
		t.Parallel()
		disabled := base
		disabled.DisabledAt = disabled.CreatedAt.AddDate(0, 0, 1)

		if _, err := admit(disabled, verified); err == nil {
			t.Error("a disabled provider admitted somebody")
		}
	})

	// With provisioning off, an existing member still signs in — their membership already
	// exists — and an unknown person becomes a user with no membership, which reaches nothing
	// and lets an administrator find them rather than making them invisible.
	t.Run("with provisioning off nothing is granted and nothing is refused", func(t *testing.T) {
		t.Parallel()
		off := base
		off.JITEnabled = false
		unrelated := claims{Email: "someone@unrelated.test", raw: map[string]any{}}

		admitted, err := admit(off, unrelated)
		if err != nil {
			t.Fatalf("provisioning being off must not refuse a sign-in: %v", err)
		}
		if admitted.Role != "" {
			t.Errorf("granted %q with provisioning off", admitted.Role)
		}
	})
}
