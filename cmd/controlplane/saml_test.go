package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/authz"
)

// configureSAMLProvider registers a local identity provider as the tenant's way in.
func configureSAMLProvider(
	t *testing.T, plane *identityPlane, idp *samlIdP, settings map[string]any,
) providerBody {
	t.Helper()

	body := map[string]any{
		"name":         "Test SAML IdP",
		"protocol":     "saml",
		"samlMetadata": idp.metadata(),
	}
	for key, value := range settings {
		body[key] = value
	}

	answered := plane.call(t, http.MethodPost,
		plane.base(identityOrg)+"/identity-providers", body, asBootstrap)
	if answered.status != http.StatusCreated {
		t.Fatalf("configuring a SAML provider = %d: %s", answered.status, answered.body)
	}
	var provider providerBody
	decodeAnswer(t, answered, &provider)
	return provider
}

// Story 12: SAML 2.0 is available where an identity provider does not offer OIDC, so identity is
// not the reason a customer cannot deploy.
func TestOperatorSAML_APersonSignsInWithASignedAssertion(t *testing.T) {
	plane := startIdentityPlane(t)
	idp := newSAMLIdP(t, "https://idp.example.test/entity",
		"https://idp.example.test/sso")

	provider := configureSAMLProvider(t, plane, idp, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "platform_administrator",
		"verifiedDomains": []string{"example.test"},
	})

	completed := signInThroughSAML(t, plane, idp, provider, "grace@example.test")
	if completed.status != http.StatusFound {
		t.Fatalf("completing a SAML sign-in = %d: %s\nlogs:\n%s",
			completed.status, completed.body, plane.logs.String())
	}
	if !strings.HasPrefix(completed.location, identityConsole) {
		t.Errorf("the browser was sent to %q, want the console", completed.location)
	}

	who := readSession(t, plane, sessionCookie(t, completed))
	if who.Principal.Email != "grace@example.test" {
		t.Errorf("the session describes %+v, want the person the assertion named", who.Principal)
	}
	if who.Principal.DisplayName != "Grace Hopper" {
		t.Errorf("the display name is %q; the record has to be able to name a person",
			who.Principal.DisplayName)
	}
	if len(who.Organizations) != 1 ||
		who.Organizations[0].Role != string(authz.PlatformAdministrator) {
		t.Fatalf("the session reports %+v, want the provisioned role", who.Organizations)
	}

	// And the credential works on a real privileged read, which is the whole point.
	if roster := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays", nil,
		asSession(sessionCookie(t, completed))); roster.status != http.StatusOK {
		t.Errorf("a SAML-issued session could not read the roster: %d", roster.status)
	}
}

// The refusals the flow rests on, each by making exactly one thing wrong. Every case here is an
// attack somebody has used against a real service provider, and a flow that accepted any of them
// would have its signature check as decoration.
func TestOperatorSAML_TheRefusalsAreLoadBearing(t *testing.T) {
	plane := startIdentityPlane(t)

	admit := map[string]any{
		"jitEnabled":      true,
		"jitRole":         "viewer",
		"verifiedDomains": []string{"example.test"},
	}

	t.Run("an assertion signed by a key the provider never published", func(t *testing.T) {
		idp := newSAMLIdP(t, "https://idp1.example.test/entity", "https://idp1.example.test/sso")
		provider := configureSAMLProvider(t, plane, idp, mergedWith(admit, map[string]any{
			"name": "unpublished key",
		}))

		// The attacker's position exactly: they can reach the assertion consumer service and
		// mint whatever XML they like, and they do not have the provider's key.
		other := newSAMLIdP(t, "https://idp1.example.test/entity", "https://idp1.example.test/sso")
		idp.signWithAnotherKey = other.key

		refused := signInThroughSAML(t, plane, idp, provider, "grace@example.test")
		if refused.status == http.StatusFound {
			t.Fatal("an assertion signed by an unpublished key was accepted; the signature " +
				"check is then doing nothing at all")
		}
		if len(refused.cookies) != 0 {
			t.Errorf("a refused assertion set %d cookies", len(refused.cookies))
		}
	})

	t.Run("an assertion with no signature at all", func(t *testing.T) {
		idp := newSAMLIdP(t, "https://idp2.example.test/entity", "https://idp2.example.test/sso")
		provider := configureSAMLProvider(t, plane, idp, mergedWith(admit, map[string]any{
			"name": "unsigned",
		}))
		idp.unsigned = true

		if refused := signInThroughSAML(
			t, plane, idp, provider, "grace@example.test"); refused.status == http.StatusFound {
			t.Error("an unsigned assertion was accepted")
		}
	})

	// The tenancy property. An assertion that is entirely valid for ANOTHER customer's service
	// provider must not sign anybody in here, and the per-provider entity identifier is what
	// makes that true rather than a shared one.
	t.Run("a valid assertion for a different audience", func(t *testing.T) {
		idp := newSAMLIdP(t, "https://idp3.example.test/entity", "https://idp3.example.test/sso")
		provider := configureSAMLProvider(t, plane, idp, mergedWith(admit, map[string]any{
			"name": "wrong audience",
		}))
		idp.audience = "https://another-customers-service-provider.example.test"

		if refused := signInThroughSAML(
			t, plane, idp, provider, "grace@example.test"); refused.status == http.StatusFound {
			t.Error("an assertion for another service provider was accepted; every tenant's " +
				"assertions would then be interchangeable")
		}
	})

	t.Run("an assertion whose window has passed", func(t *testing.T) {
		idp := newSAMLIdP(t, "https://idp4.example.test/entity", "https://idp4.example.test/sso")
		provider := configureSAMLProvider(t, plane, idp, mergedWith(admit, map[string]any{
			"name": "expired",
		}))
		idp.notBefore = time.Now().Add(-2 * time.Hour)
		idp.notOnOrAfter = time.Now().Add(-time.Hour)

		if refused := signInThroughSAML(
			t, plane, idp, provider, "grace@example.test"); refused.status == http.StatusFound {
			t.Error("an expired assertion was accepted")
		}
	})

	t.Run("an assertion delivered to the wrong recipient", func(t *testing.T) {
		idp := newSAMLIdP(t, "https://idp5.example.test/entity", "https://idp5.example.test/sso")
		provider := configureSAMLProvider(t, plane, idp, mergedWith(admit, map[string]any{
			"name": "wrong recipient",
		}))
		idp.recipient = "https://somewhere-else.example.test/acs"

		if refused := signInThroughSAML(
			t, plane, idp, provider, "grace@example.test"); refused.status == http.StatusFound {
			t.Error("an assertion addressed elsewhere was accepted")
		}
	})

	// The one this product's own machinery is responsible for rather than the library's: the
	// relay state is single-use, so the same POST twice signs somebody in once.
	t.Run("a replayed response", func(t *testing.T) {
		idp := newSAMLIdP(t, "https://idp6.example.test/entity", "https://idp6.example.test/sso")
		provider := configureSAMLProvider(t, plane, idp, mergedWith(admit, map[string]any{
			"name": "replay",
		}))

		started := plane.call(t, http.MethodGet,
			"http://"+plane.operator+provider.SignInURL, nil)
		requestID, relay := readAuthnRequest(t, started.location)
		consumer := "http://" + plane.operator + "/operator/v1/organizations/" + identityOrg +
			"/sign-in/saml/" + provider.ID + "/callback"
		audience := "http://" + plane.operator + "/operator/v1/organizations/" + identityOrg +
			"/saml/" + provider.ID
		document := idp.respond(t, requestID, "grace@example.test", audience, consumer)

		form := url.Values{"SAMLResponse": {document}, "RelayState": {relay}}
		first := plane.postForm(t, consumer, form)
		if first.status != http.StatusFound {
			t.Fatalf("the first delivery = %d: %s", first.status, first.body)
		}
		second := plane.postForm(t, consumer, form)
		if second.status == http.StatusFound {
			t.Fatal("the same response twice issued two sessions; the relay state must be " +
				"single-use, and that is this product's defence rather than the library's")
		}
		if len(second.cookies) != 0 {
			t.Errorf("a replayed response set %d cookies", len(second.cookies))
		}
	})

	t.Run("a relay state nobody issued", func(t *testing.T) {
		idp := newSAMLIdP(t, "https://idp7.example.test/entity", "https://idp7.example.test/sso")
		provider := configureSAMLProvider(t, plane, idp, mergedWith(admit, map[string]any{
			"name": "invented relay",
		}))
		consumer := "http://" + plane.operator + "/operator/v1/organizations/" + identityOrg +
			"/sign-in/saml/" + provider.ID + "/callback"

		refused := plane.postForm(t, consumer, url.Values{
			"SAMLResponse": {idp.respond(t, "id-invented", "grace@example.test", "x", consumer)},
			"RelayState":   {"a-relay-state-nobody-was-ever-issued"},
		})
		if refused.status != http.StatusBadRequest {
			t.Errorf("an invented relay state = %d, want 400", refused.status)
		}
	})
}

// A provider that publishes no signing certificate could never be verified, so an assertion
// from it would be accepted on the strength of having arrived. It is refused at CONFIGURATION
// time, where an administrator can fix it.
func TestOperatorSAML_MetadataIsCheckedWhenItIsPasted(t *testing.T) {
	plane := startIdentityPlane(t)
	idp := newSAMLIdP(t, "https://idp.example.test/entity", "https://idp.example.test/sso")

	for name, document := range map[string]string{
		"no signing certificate": idp.metadataWithoutASigningKey(),
		"not metadata at all":    "<html><body>your session has expired</body></html>",
		"not XML":                "{}",
		"empty":                  "",
	} {
		t.Run(name, func(t *testing.T) {
			refused := plane.call(t, http.MethodPost,
				plane.base(identityOrg)+"/identity-providers", map[string]any{
					"name": "refused " + name, "protocol": "saml", "samlMetadata": document,
				}, asBootstrap)
			if refused.status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400: %s", name, refused.status, refused.body)
			}
		})
	}

	// And a good one is accepted, so the cases above fail for their own reason rather than
	// because nothing is ever accepted.
	provider := configureSAMLProvider(t, plane, idp, nil)
	if provider.SignInURL == "" {
		t.Error("a configured provider reports no way to sign in through it")
	}

	// The administrator's next step: our metadata, to hand to their provider. Getting the
	// audience or the recipient wrong by hand is the most common SAML misconfiguration there
	// is, and this is what stops it being done by hand.
	metadata := plane.call(t, http.MethodGet,
		plane.base(identityOrg)+"/identity-providers/"+provider.ID+"/saml-metadata",
		nil, asBootstrap)
	if metadata.status != http.StatusOK {
		t.Fatalf("reading our own metadata = %d: %s", metadata.status, metadata.body)
	}
	for _, wanted := range []string{
		"EntityDescriptor",
		"/saml/" + provider.ID,
		"/sign-in/saml/" + provider.ID + "/callback",
	} {
		if !strings.Contains(metadata.body, wanted) {
			t.Errorf("our metadata does not carry %q:\n%s", wanted, metadata.body)
		}
	}
}

// Story 8 applies to SAML too: a tenant's provisioning policy decides who a signed assertion
// admits, not the fact that it verified.
func TestOperatorSAML_ThePolicyStillDecidesWhoGetsIn(t *testing.T) {
	plane := startIdentityPlane(t)
	idp := newSAMLIdP(t, "https://idp.example.test/entity", "https://idp.example.test/sso")
	provider := configureSAMLProvider(t, plane, idp, map[string]any{
		"jitEnabled":      true,
		"jitRole":         "viewer",
		"verifiedDomains": []string{"example.test"},
		"groupClaim":      "http://schemas.xmlsoap.org/claims/Group",
		"groupRoleMap":    map[string]string{"sre": "investigator"},
	})

	t.Run("an unrelated domain is refused even with a valid signature", func(t *testing.T) {
		idp.attributes = map[string][]string{
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": {
				"outsider@unrelated.test",
			},
		}
		if refused := signInThroughSAML(t, plane, idp, provider,
			"outsider@unrelated.test"); refused.status == http.StatusFound {
			t.Error("a valid assertion for an unlisted domain was admitted; a signature says " +
				"who sent it, not that this tenant wants them")
		}
	})

	// Story 9 for SAML: a group in the assertion maps to a role, so role assignment is not a
	// second directory an administrator maintains by hand.
	t.Run("a group attribute maps to the role it names", func(t *testing.T) {
		idp.attributes = map[string][]string{
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": {
				"mapped@example.test",
			},
			"http://schemas.xmlsoap.org/claims/Group": {"finance", "sre"},
		}

		completed := signInThroughSAML(t, plane, idp, provider, "mapped@example.test")
		if completed.status != http.StatusFound {
			t.Fatalf("a mapped sign-in = %d: %s", completed.status, completed.body)
		}
		who := readSession(t, plane, sessionCookie(t, completed))
		if len(who.Organizations) != 1 ||
			who.Organizations[0].Role != string(authz.Investigator) {
			t.Errorf("the group attribute produced %+v, want the mapped role",
				who.Organizations)
		}
	})
}

func mergedWith(base, extra map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}
