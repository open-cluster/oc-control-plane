package identity

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewjam/saml"
	xrv "github.com/mattermost/xml-roundtrip-validator"

	"github.com/open-cluster/oc-control-plane/internal/storage"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// SAML 2.0 web browser SSO, service-provider initiated.
//
// The parts of this standard that have produced a decade of authentication bypasses —
// exclusive canonicalization, XML digital signature verification, and the signature-wrapping
// class of attack that comes from verifying one element and reading claims from another — are
// NOT implemented here. They come from github.com/crewjam/saml, which composes goxmldsig and
// mattermost/xml-roundtrip-validator, and it is used precisely because reimplementing that
// composition is where those bypasses live. What this file does is everything around it: which
// provider, what the request said, what the assertion means, and what happens next.
//
// IdP-initiated SAML is deliberately not served. An unsolicited assertion has no request of
// ours to be bound to, so the single-use flow row that makes a replay impossible under both
// protocols would have nothing to consume. It is recorded as out of scope in the specification
// rather than left as a gap somebody discovers.

// Bounds on what an identity provider may hand this process.
const (
	// maxMetadataLength bounds the metadata document an administrator pastes. Real ones are a
	// few kilobytes; a federation's aggregate can be megabytes, and this product consumes one
	// provider rather than a federation.
	maxMetadataLength = 512 * 1024
	// samlRequestLifetime is how long an AuthnRequest may take to come back. It is the same
	// bound the OIDC flow uses, and for the same reason.
	samlRequestLifetime = flowLifetime
)

// ErrMetadataRefused reports an identity provider metadata document this build will not accept.
var ErrMetadataRefused = errors.New("the identity provider metadata was refused")

// The attribute names identity providers actually send. There is no registry that every vendor
// agrees on, so this build reads the union rather than making an administrator map three
// fields by hand for a provider that already publishes them under a well-known name.
//
// The order is the order they are preferred in.
var (
	emailAttributes = []string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
		"urn:oid:0.9.2342.19200300.100.1.3",
		"urn:oid:1.2.840.113549.1.9.1",
		"email", "mail", "emailAddress", "emailaddress", "User.email",
	}
	nameAttributes = []string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
		"http://schemas.microsoft.com/identity/claims/displayname",
		"urn:oid:2.16.840.1.113730.3.1.241",
		"displayName", "displayname", "name", "cn",
	}
	givenNameAttributes = []string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/givenname",
		"urn:oid:2.5.4.42", "givenName", "firstName", "User.FirstName",
	}
	familyNameAttributes = []string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/surname",
		"urn:oid:2.5.4.4", "sn", "surname", "lastName", "User.LastName",
	}
	// groupAttributes is only the fallback. A provider's group attribute is configurable per
	// provider, because which one carries the groups is the one thing vendors genuinely differ
	// on in a way no list can cover.
	groupAttributes = []string{
		"http://schemas.xmlsoap.org/claims/Group",
		"http://schemas.microsoft.com/ws/2008/06/identity/claims/groups",
		"groups", "Group", "memberOf", "Role",
	}
)

// parseIdPMetadata reads what an administrator pasted.
//
// The round-trip validation runs FIRST and is not decoration: it refuses XML whose parsed form
// does not re-serialise to the same document, which is the general shape of the
// signature-wrapping attacks this standard is known for. Refusing it at configuration time
// means a document that could be read two ways never reaches the signature check at all.
func parseIdPMetadata(document string) (*saml.EntityDescriptor, error) {
	trimmed := strings.TrimSpace(document)
	if trimmed == "" || len(trimmed) > maxMetadataLength {
		return nil, fmt.Errorf("%w: it is empty or larger than %d bytes",
			ErrMetadataRefused, maxMetadataLength)
	}
	if err := xrv.Validate(bytes.NewBufferString(trimmed)); err != nil {
		return nil, fmt.Errorf("%w: it does not survive a parse and re-serialise, which is the "+
			"shape a signature-wrapping document takes", ErrMetadataRefused)
	}

	entity := &saml.EntityDescriptor{}
	err := xml.Unmarshal([]byte(trimmed), entity)
	if err != nil && strings.Contains(err.Error(), "EntitiesDescriptor") {
		// A provider may publish its descriptor wrapped in an aggregate. The first entity that
		// actually describes an identity provider is the one meant.
		entities := &saml.EntitiesDescriptor{}
		if unwrapErr := xml.Unmarshal([]byte(trimmed), entities); unwrapErr != nil {
			return nil, fmt.Errorf("%w: it is not metadata", ErrMetadataRefused)
		}
		for index := range entities.EntityDescriptors {
			if len(entities.EntityDescriptors[index].IDPSSODescriptors) > 0 {
				entity = &entities.EntityDescriptors[index]
				err = nil
				break
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: it is not metadata", ErrMetadataRefused)
	}

	if len(entity.IDPSSODescriptors) == 0 {
		return nil, fmt.Errorf("%w: it describes no identity provider", ErrMetadataRefused)
	}
	if strings.TrimSpace(entity.EntityID) == "" {
		return nil, fmt.Errorf("%w: it names no entity", ErrMetadataRefused)
	}
	// A provider with no published signing certificate could not be verified, so an assertion
	// from it would be accepted on the strength of having arrived. Refused at configuration
	// time, where an administrator can fix it, rather than at somebody's first sign-in.
	if !publishesASigningKey(entity) {
		return nil, fmt.Errorf("%w: it publishes no signing certificate, so nothing it sent "+
			"could be verified", ErrMetadataRefused)
	}
	if entity.IDPSSODescriptors[0].SingleSignOnServices == nil {
		return nil, fmt.Errorf("%w: it offers no single sign-on service", ErrMetadataRefused)
	}
	return entity, nil
}

func publishesASigningKey(entity *saml.EntityDescriptor) bool {
	for _, descriptor := range entity.IDPSSODescriptors {
		for _, key := range descriptor.KeyDescriptors {
			if key.Use != "" && key.Use != "signing" {
				continue
			}
			for _, certificate := range key.KeyInfo.X509Data.X509Certificates {
				if strings.TrimSpace(certificate.Data) != "" {
					return true
				}
			}
		}
	}
	return false
}

// serviceProvider is what this control plane looks like to one tenant's identity provider.
//
// The entity identifier and the assertion consumer service are PER PROVIDER rather than one
// for the deployment, and that is a tenancy decision rather than a cosmetic one: the audience
// restriction the library checks is this value, so an assertion minted for one customer's
// service provider cannot be replayed at another's. One shared entity identifier would make
// every tenant's assertions interchangeable.
func (h Handlers) serviceProvider(
	provider storage.IdentityProvider,
) (*saml.ServiceProvider, error) {
	metadata, err := parseIdPMetadata(provider.SAMLMetadata)
	if err != nil {
		return nil, err
	}
	entity, err := url.Parse(h.samlEntityID(provider))
	if err != nil {
		return nil, fmt.Errorf("%w: this deployment's public URL is unusable",
			ErrProviderUnreachable)
	}
	consumer, err := url.Parse(h.samlConsumerURL(provider))
	if err != nil {
		return nil, fmt.Errorf("%w: this deployment's public URL is unusable",
			ErrProviderUnreachable)
	}

	return &saml.ServiceProvider{
		EntityID:    entity.String(),
		MetadataURL: *entity,
		AcsURL:      *consumer,
		IDPMetadata: metadata,
		// No key. This build does not sign its AuthnRequests: the request carries no secret,
		// the response is bound to it by InResponseTo, and the flow row that records it is
		// single-use. Signing would mean holding a private key per deployment for a property
		// already held by something simpler. Providers that REQUIRE a signed request are not
		// served, and the specification records that rather than leaving it to be discovered.
		AuthnNameIDFormat: saml.EmailAddressNameIDFormat,
	}, nil
}

func (h Handlers) samlEntityID(provider storage.IdentityProvider) string {
	return strings.TrimSuffix(h.PublicURL, "/") + Base + "/organizations/" +
		provider.Organization + "/saml/" + provider.ID.String()
}

func (h Handlers) samlConsumerURL(provider storage.IdentityProvider) string {
	return strings.TrimSuffix(h.PublicURL, "/") + Base + "/organizations/" +
		provider.Organization + "/sign-in/saml/" + provider.ID.String() + "/callback"
}

// samlAssertion is what a verified assertion said, in the same shape the OIDC path produces so
// that the admission policy, the provisioning and the session issue are one code path for both
// protocols rather than two that can drift.
type samlAssertion struct {
	subject     string
	email       string
	displayName string
	groups      []string
}

// asClaims renders a verified assertion into the shape the tenant's policy is applied to.
//
// EmailVerified is TRUE, and that is a statement worth making explicitly. SAML has no
// equivalent of the email_verified claim, and the reason is that it does not need one: the
// assertion is signed by the identity provider the tenant configured, asserting an attribute
// that provider controls. That is a stronger claim than a self-asserted OIDC address, not a
// weaker one, and treating it as unverified would mean no SAML tenant could ever use a verified
// domain policy.
func (a samlAssertion) asClaims(groupClaim string) claims {
	raw := map[string]any{}
	if len(a.groups) > 0 {
		values := make([]any, 0, len(a.groups))
		for _, group := range a.groups {
			values = append(values, group)
		}
		if groupClaim == "" {
			groupClaim = "groups"
		}
		raw[groupClaim] = values
	}
	return claims{
		Subject:       a.subject,
		Email:         a.email,
		EmailVerified: true,
		Name:          a.displayName,
		raw:           raw,
	}
}

// readAssertion pulls what this build needs out of a verified assertion.
//
// It reads from the assertion the library RETURNED, which is the object whose signature was
// verified. That is the whole defence against signature wrapping at this layer: there is no
// second parse of the response document here, and therefore no way for claims to come from an
// element other than the one that was checked.
func readAssertion(assertion *saml.Assertion, groupAttribute string) (samlAssertion, error) {
	read := samlAssertion{}
	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		read.subject = strings.TrimSpace(assertion.Subject.NameID.Value)
	}
	if read.subject == "" {
		return samlAssertion{}, fmt.Errorf("%w: it names no subject", ErrTokenRefused)
	}

	attributes := map[string][]string{}
	for _, statement := range assertion.AttributeStatements {
		for _, attribute := range statement.Attributes {
			for _, key := range []string{attribute.Name, attribute.FriendlyName} {
				if strings.TrimSpace(key) == "" {
					continue
				}
				for _, value := range attribute.Values {
					if trimmed := strings.TrimSpace(value.Value); trimmed != "" {
						attributes[key] = append(attributes[key], trimmed)
					}
				}
			}
		}
	}

	read.email = firstAttribute(attributes, emailAttributes)
	if read.email == "" && strings.Contains(read.subject, "@") {
		// A NameID in the email format is the address, and many providers send nothing else.
		read.email = read.subject
	}
	read.displayName = firstAttribute(attributes, nameAttributes)
	if read.displayName == "" {
		given := firstAttribute(attributes, givenNameAttributes)
		family := firstAttribute(attributes, familyNameAttributes)
		read.displayName = strings.TrimSpace(given + " " + family)
	}

	if groupAttribute != "" {
		read.groups = attributes[groupAttribute]
	}
	if len(read.groups) == 0 {
		for _, candidate := range groupAttributes {
			if values, present := attributes[candidate]; present {
				read.groups = values
				break
			}
		}
	}
	return read, nil
}

func firstAttribute(attributes map[string][]string, preferred []string) string {
	for _, name := range preferred {
		if values, present := attributes[name]; present && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// startSAMLSignIn sends the browser to the tenant's identity provider over the redirect binding.
func (h Handlers) startSAMLSignIn(
	writer http.ResponseWriter, request *http.Request,
	organization tenancy.Organization, provider storage.IdentityProvider, returnTo string,
) {
	serviceProvider, err := h.serviceProvider(provider)
	if err != nil {
		h.refuseSignIn(writer, request, organization, provider.ID, err.Error())
		return
	}

	authentication, err := serviceProvider.MakeAuthenticationRequest(
		serviceProvider.GetSSOBindingLocation(saml.HTTPRedirectBinding),
		saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		h.refuseSignIn(writer, request, organization, provider.ID,
			"the authentication request could not be built")
		return
	}

	// The relay state is this product's own opaque value, exactly as the OIDC state is, and only
	// its digest is stored. It is what the callback is looked up by; the request identifier
	// stored beside it is what the assertion must name in InResponseTo. Two different checks
	// over two different values, and the response has to satisfy both.
	relay, err := randomToken()
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()

	if err := h.Placements.StartSignIn(ctx, organization, storage.SignInFlow{
		ID:           newFlowID(),
		Organization: organization.String(),
		ProviderID:   provider.ID,
		RequestID:    authentication.ID,
		ReturnTo:     returnTo,
		ExpiresAt:    nowPlus(samlRequestLifetime),
	}, relay); err != nil {
		h.fail(writer, request, err)
		return
	}
	h.recordSignInStarted(request, organization, provider)

	redirect, err := authentication.Redirect(relay, serviceProvider)
	if err != nil {
		h.refuseSignIn(writer, request, organization, provider.ID,
			"the authentication request could not be encoded")
		return
	}
	http.Redirect(writer, request, redirect.String(), http.StatusFound)
}

// completeSAMLSignIn takes the browser back from the identity provider.
//
// The order is the order the refusals matter in, and it is the same order the OIDC callback
// uses. The relay state is redeemed FIRST, and redeeming it is a conditional update, so a
// replayed POST is refused before this process spends anything verifying a signature it has
// already seen. Only then is the response parsed, and it is parsed by the library — signature,
// conditions, audience, recipient, InResponseTo and the not-on-or-after window together.
func (h Handlers) completeSAMLSignIn(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "this is not a sign-in"})
		return
	}
	relay := request.PostFormValue("RelayState")
	if relay == "" || request.PostFormValue("SAMLResponse") == "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "this is not a sign-in"})
		return
	}

	ctx, cancel := contextWithTimeout(request, signInTimeout)
	defer cancel()

	flow, err := h.Placements.RedeemSignIn(ctx, relay)
	if err != nil {
		h.Logger.WarnContext(ctx, "a saml callback presented an unusable relay state",
			slog.String("caller", request.RemoteAddr))
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "this sign-in cannot be completed"})
		return
	}
	organization, err := tenancy.NewOrganization(flow.Organization)
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	provider, err := h.Placements.IdentityProviderForSignIn(ctx, organization, flow.ProviderID)
	if err != nil || provider.Protocol != storage.ProtocolSAML {
		h.refuseSignIn(writer, request, organization, flow.ProviderID, "the provider is gone")
		return
	}
	serviceProvider, err := h.serviceProvider(provider)
	if err != nil {
		h.refuseSignIn(writer, request, organization, provider.ID, err.Error())
		return
	}

	// Exactly one request identifier is accepted: the one this flow sent. Handing the library
	// a wider set would mean an assertion answering some other sign-in of ours was admitted.
	assertion, err := serviceProvider.ParseResponse(request, []string{flow.RequestID})
	if err != nil {
		// An unsigned response, a signature that does not verify, an assertion for another
		// audience, one outside its window, one answering a different request, and a document
		// that does not survive a re-serialise all arrive here. The browser is told one thing
		// and the record is told which.
		h.refuseSignIn(writer, request, organization, provider.ID, samlReason(err))
		return
	}

	read, err := readAssertion(assertion, provider.GroupClaim)
	if err != nil {
		h.refuseSignIn(writer, request, organization, provider.ID, err.Error())
		return
	}
	// The issuer recorded against the person is the PROVIDER'S entity identifier rather than
	// anything the assertion said about itself. The library has already checked that the
	// assertion came from the configured provider; taking the issuer from configuration means a
	// person's identity is keyed on what a tenant set up, not on a string in a document.
	h.admitAndIssue(writer, request, organization, provider,
		read.asClaims(provider.GroupClaim), provider.Issuer, read.subject, flow.ReturnTo)
}

// samlReason renders a library refusal for the RECORD, not for the browser.
//
// crewjam's invalid-response error carries the private reason it refused for; unwrapping it is
// what makes an audit entry say "the signature does not verify" rather than "refused", which
// is the difference between an administrator fixing a certificate and opening a support ticket.
func samlReason(err error) string {
	var invalid *saml.InvalidResponseError
	if errors.As(err, &invalid) && invalid.PrivateErr != nil {
		return invalid.PrivateErr.Error()
	}
	return err.Error()
}

// samlMetadata is this deployment's own service-provider metadata for one provider.
//
// An administrator hands it to their identity provider rather than typing an entity identifier
// and a callback URL, which is where the two most common misconfigurations come from: an
// audience that does not match and a recipient that does not. Both are checked at sign-in, so
// getting them wrong by hand is a refusal with no obvious cause.
func (h Handlers) samlMetadata(writer http.ResponseWriter, request *http.Request) {
	if _, ok := h.caller(writer, request); !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	id, ok := identifier(writer, request, "provider")
	if !ok {
		return
	}

	ctx, cancel := contextWithTimeout(request, readTimeout)
	defer cancel()

	provider, err := h.Placements.IdentityProviderForSignIn(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if provider.Protocol != storage.ProtocolSAML {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "this identity provider does not speak SAML"})
		return
	}

	serviceProvider, err := h.serviceProvider(provider)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	document, err := xml.MarshalIndent(serviceProvider.Metadata(), "", "  ")
	if err != nil {
		h.fail(writer, request, err)
		return
	}

	writer.Header().Set("Content-Type", "application/samlmetadata+xml")
	writer.Header().Set("Content-Disposition", `attachment; filename="opencluster-sp.xml"`)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(xml.Header))
	_, _ = writer.Write(document)
}
