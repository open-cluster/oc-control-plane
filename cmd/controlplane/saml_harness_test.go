package main

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

// A local SAML identity provider that signs real assertions with a real key, and can be made to
// sign the wrong thing.
//
// It has to be real. The whole point of the SAML work is that the signature is checked, and a
// harness that handed the control plane an unsigned document would test nothing except that the
// control plane refuses unsigned documents. So this mints a certificate, publishes it in
// metadata the control plane parses, and signs assertions with goxmldsig — the same library
// that verifies them, used from the other side.
//
// Every way it can be made to misbehave corresponds to an attack somebody has actually used
// against a SAML service provider.
type samlIdP struct {
	entityID    string
	ssoURL      string
	key         *rsa.PrivateKey
	certificate *x509.Certificate

	// Levers a test pulls to make exactly one thing wrong.
	//
	// signWithAnotherKey signs with a key the metadata does not publish, which is what an
	// attacker who can reach the assertion consumer service but not the identity provider has.
	signWithAnotherKey *rsa.PrivateKey
	// audience overrides the audience restriction, which is what a valid assertion for a
	// DIFFERENT tenant of this deployment looks like.
	audience string
	// recipient overrides the subject confirmation's recipient.
	recipient string
	// unsigned mints an assertion with no signature at all.
	unsigned bool
	// notBefore and notOnOrAfter move the condition window.
	notBefore    time.Time
	notOnOrAfter time.Time
	// attributes are what the assertion asserts, beyond the subject.
	attributes map[string][]string
}

func newSAMLIdP(t *testing.T, entityID, ssoURL string) *samlIdP {
	t.Helper()

	key, certificate := selfSigned(t, entityID)
	return &samlIdP{
		entityID:    entityID,
		ssoURL:      ssoURL,
		key:         key,
		certificate: certificate,
		attributes: map[string][]string{
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": {
				"grace@example.test",
			},
			"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name": {"Grace Hopper"},
		},
	}
}

func selfSigned(t *testing.T, name string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating an identity provider key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	raw, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("minting an identity provider certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatalf("parsing the certificate: %v", err)
	}
	return key, certificate
}

// metadata is what an administrator pastes when configuring this provider.
func (i *samlIdP) metadata() string {
	encoded := base64.StdEncoding.EncodeToString(i.certificate.Raw)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <KeyDescriptor use="signing">
      <KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#">
        <X509Data><X509Certificate>%s</X509Certificate></X509Data>
      </KeyInfo>
    </KeyDescriptor>
    <NameIDFormat>urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress</NameIDFormat>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s"/>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="%s"/>
  </IDPSSODescriptor>
</EntityDescriptor>`, i.entityID, encoded, i.ssoURL, i.ssoURL)
}

// metadataWithoutASigningKey is what a misconfigured provider publishes: everything except the
// certificate. An assertion from it could not be verified, so configuring it must be refused
// rather than accepted and discovered at somebody's first sign-in.
func (i *samlIdP) metadataWithoutASigningKey() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s"/>
  </IDPSSODescriptor>
</EntityDescriptor>`, i.entityID, i.ssoURL)
}

// respond mints the SAMLResponse a browser would POST back, answering one AuthnRequest.
func (i *samlIdP) respond(
	t *testing.T, requestID, subject, audience, recipient string,
) string {
	t.Helper()

	if i.audience != "" {
		audience = i.audience
	}
	if i.recipient != "" {
		recipient = i.recipient
	}
	notBefore, notOnOrAfter := i.notBefore, i.notOnOrAfter
	if notBefore.IsZero() {
		notBefore = time.Now().Add(-time.Minute)
	}
	if notOnOrAfter.IsZero() {
		notOnOrAfter = time.Now().Add(5 * time.Minute)
	}

	document := etree.NewDocument()
	response := document.CreateElement("samlp:Response")
	response.CreateAttr("xmlns:samlp", "urn:oasis:names:tc:SAML:2.0:protocol")
	response.CreateAttr("xmlns:saml", "urn:oasis:names:tc:SAML:2.0:assertion")
	response.CreateAttr("ID", "_response-"+strings.TrimPrefix(requestID, "id-"))
	response.CreateAttr("Version", "2.0")
	response.CreateAttr("IssueInstant", now())
	response.CreateAttr("Destination", recipient)
	response.CreateAttr("InResponseTo", requestID)
	response.CreateElement("saml:Issuer").SetText(i.entityID)
	response.CreateElement("samlp:Status").
		CreateElement("samlp:StatusCode").
		CreateAttr("Value", "urn:oasis:names:tc:SAML:2.0:status:Success")

	assertion := etree.NewElement("saml:Assertion")
	assertion.CreateAttr("xmlns:saml", "urn:oasis:names:tc:SAML:2.0:assertion")
	assertion.CreateAttr("xmlns:xsi", "http://www.w3.org/2001/XMLSchema-instance")
	assertion.CreateAttr("ID", "_assertion-"+strings.TrimPrefix(requestID, "id-"))
	assertion.CreateAttr("Version", "2.0")
	assertion.CreateAttr("IssueInstant", now())
	assertion.CreateElement("saml:Issuer").SetText(i.entityID)

	subjectElement := assertion.CreateElement("saml:Subject")
	nameID := subjectElement.CreateElement("saml:NameID")
	nameID.CreateAttr("Format", "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress")
	nameID.SetText(subject)
	confirmation := subjectElement.CreateElement("saml:SubjectConfirmation")
	confirmation.CreateAttr("Method", "urn:oasis:names:tc:SAML:2.0:cm:bearer")
	data := confirmation.CreateElement("saml:SubjectConfirmationData")
	data.CreateAttr("InResponseTo", requestID)
	data.CreateAttr("Recipient", recipient)
	data.CreateAttr("NotOnOrAfter", notOnOrAfter.UTC().Format(time.RFC3339))

	conditions := assertion.CreateElement("saml:Conditions")
	conditions.CreateAttr("NotBefore", notBefore.UTC().Format(time.RFC3339))
	conditions.CreateAttr("NotOnOrAfter", notOnOrAfter.UTC().Format(time.RFC3339))
	conditions.CreateElement("saml:AudienceRestriction").
		CreateElement("saml:Audience").SetText(audience)

	authn := assertion.CreateElement("saml:AuthnStatement")
	authn.CreateAttr("AuthnInstant", now())
	authn.CreateAttr("SessionIndex", "_session-"+requestID)
	authn.CreateElement("saml:AuthnContext").
		CreateElement("saml:AuthnContextClassRef").
		SetText("urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport")

	if len(i.attributes) > 0 {
		statement := assertion.CreateElement("saml:AttributeStatement")
		for name, values := range i.attributes {
			attribute := statement.CreateElement("saml:Attribute")
			attribute.CreateAttr("Name", name)
			attribute.CreateAttr("NameFormat",
				"urn:oasis:names:tc:SAML:2.0:attrname-format:unspecified")
			for _, value := range values {
				attribute.CreateElement("saml:AttributeValue").SetText(value)
			}
		}
	}

	if i.unsigned {
		response.AddChild(assertion)
	} else {
		response.AddChild(i.sign(t, assertion))
	}

	// NOT indented. Pretty-printing after signing inserts whitespace text nodes inside the
	// signed assertion, which changes its canonical form and breaks the digest — the first
	// thing this harness got wrong, and a fair demonstration that the verification is real.
	raw, err := document.WriteToString()
	if err != nil {
		t.Fatalf("serialising the response: %v", err)
	}
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// sign puts a real XML digital signature on the assertion, with the same library that verifies
// it. Signing with goxmldsig rather than by hand is what makes the verification meaningful: a
// hand-rolled signature that the verifier happened to accept would prove nothing about a real
// provider's.
func (i *samlIdP) sign(t *testing.T, assertion *etree.Element) *etree.Element {
	t.Helper()

	signing := i.key
	if i.signWithAnotherKey != nil {
		signing = i.signWithAnotherKey
	}
	context := dsig.NewDefaultSigningContext(&fixedKeyStore{
		key:         signing,
		certificate: i.certificate.Raw,
	})
	if err := context.SetSignatureMethod(dsig.RSASHA256SignatureMethod); err != nil {
		t.Fatalf("choosing a signature method: %v", err)
	}
	// EXCLUSIVE canonicalization, which is what every real identity provider uses and what the
	// default here is not.
	//
	// The default is inclusive C14N 1.1, and inclusive canonicalization takes in every
	// namespace in scope. This assertion is signed standing alone and verified as a child of a
	// Response that declares samlp — so under inclusive rules the canonical form the verifier
	// computes has a namespace the signer's did not, and the digest cannot match. Exclusive
	// canonicalization includes only what the element visibly uses, which is why the standard
	// settled on it and why getting this wrong looks exactly like a forged signature.
	context.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")

	signed, err := context.SignEnveloped(assertion)
	if err != nil {
		t.Fatalf("signing the assertion: %v", err)
	}
	return signed
}

// fixedKeyStore hands goxmldsig the one key this identity provider signs with.
type fixedKeyStore struct {
	key         *rsa.PrivateKey
	certificate []byte
}

func (s *fixedKeyStore) GetKeyPair() (*rsa.PrivateKey, []byte, error) {
	return s.key, s.certificate, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// signInThroughSAML drives the whole flow: start, read the AuthnRequest the control plane sent,
// mint a response answering it, and POST it back to the assertion consumer service.
func signInThroughSAML(
	t *testing.T, plane *identityPlane, idp *samlIdP, provider providerBody, subject string,
) answer {
	t.Helper()

	started := plane.call(t, http.MethodGet, "http://"+plane.operator+provider.SignInURL, nil)
	if started.status != http.StatusFound {
		t.Fatalf("starting a SAML sign-in = %d: %s", started.status, started.body)
	}

	requestID, relay := readAuthnRequest(t, started.location)
	consumer := "http://" + plane.operator + "/operator/v1/organizations/" + identityOrg +
		"/sign-in/saml/" + provider.ID + "/callback"
	audience := "http://" + plane.operator + "/operator/v1/organizations/" + identityOrg +
		"/saml/" + provider.ID

	return plane.postForm(t, consumer, url.Values{
		"SAMLResponse": {idp.respond(t, requestID, subject, audience, consumer)},
		"RelayState":   {relay},
	})
}

// readAuthnRequest pulls the request identifier and the relay state out of the redirect the
// control plane sent the browser to.
//
// It also asserts what a redirect binding must carry, because a request missing any of it would
// produce a response nothing could be bound to — and the flow would then appear to work while
// resting on nothing.
func readAuthnRequest(t *testing.T, location string) (string, string) {
	t.Helper()

	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parsing the authorization redirect: %v", err)
	}
	query := parsed.Query()
	relay := query.Get("RelayState")
	if relay == "" {
		t.Fatal("the authentication request carries no relay state; nothing would bind the " +
			"response to a sign-in this product started")
	}

	deflated, err := base64.StdEncoding.DecodeString(query.Get("SAMLRequest"))
	if err != nil {
		t.Fatalf("decoding the authentication request: %v", err)
	}
	inflated := inflate(t, deflated)

	var request struct {
		XMLName xml.Name `xml:"AuthnRequest"`
		ID      string   `xml:"ID,attr"`
	}
	if err := xml.Unmarshal(inflated, &request); err != nil {
		t.Fatalf("reading the authentication request: %v (%s)", err, inflated)
	}
	if request.ID == "" {
		t.Fatal("the authentication request has no identifier; the response could not be bound")
	}
	return request.ID, relay
}

// inflate reverses the DEFLATE the redirect binding requires. It is spelled out because the
// binding says raw deflate with no zlib header, and reading it wrong looks like a malformed
// request rather than a decoding mistake.
func inflate(t *testing.T, compressed []byte) []byte {
	t.Helper()

	reader := flate.NewReader(bytes.NewReader(compressed))
	defer func() { _ = reader.Close() }()

	inflated, err := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err != nil {
		t.Fatalf("inflating the authentication request: %v", err)
	}
	return inflated
}

// postForm submits a form the way a browser submits the identity provider's auto-posting page.
//
// No Origin header: a SAML assertion arrives cross-site by construction, from a form the
// identity provider served, so a callback that required one would refuse every real sign-in.
// That the route is public is what makes it work, and the test omitting the header is what
// proves the route really is.
func (p *identityPlane) postForm(t *testing.T, target string, values url.Values) answer {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("building the callback: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("posting to %s: %v", target, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the callback response: %v", err)
	}
	return answer{
		status:   response.StatusCode,
		body:     string(body),
		cookies:  response.Cookies(),
		location: response.Header.Get("Location"),
	}
}
