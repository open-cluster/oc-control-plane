package controlplane

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// WHAT THIS DEPLOYMENT SERVES, READ BACK OVER THE WIRE.
//
// The seam is the composition root, because the document's whole value is that a client can
// fetch it: a description assembled correctly in a unit test and served by nothing would be
// exactly the state this issue found the product in.
//
// The assertions are the ones a console actually depends on. Which optional surfaces exist,
// so a 404 is not the discovery mechanism. What a listing accepts, checked by SENDING what
// the document says is accepted rather than by comparing two lists. And what a write's body
// may carry, checked by sending a field the document omits and confirming the refusal the
// document implies.

type descriptionBody struct {
	Surfaces []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Note    string `json:"note"`
	} `json:"surfaces"`
	Routes []struct {
		Method     string `json:"method"`
		Path       string `json:"path"`
		Access     string `json:"access"`
		Permission string `json:"permission"`
	} `json:"routes"`
	Listings []struct {
		Method      string   `json:"method"`
		Path        string   `json:"path"`
		Searchable  bool     `json:"searchable"`
		Sortable    []string `json:"sortable"`
		DefaultSort string   `json:"defaultSort"`
		Filters     []string `json:"filters"`
	} `json:"listings"`
	ListingRules struct {
		Reserved       []string `json:"reservedParameters"`
		SortPrefixes   []string `json:"sortPrefixes"`
		DefaultLimit   int      `json:"defaultLimit"`
		MaxLimit       int      `json:"maxLimit"`
		LimitIsClamped bool     `json:"limitIsClamped"`
	} `json:"listingRules"`
	RequestBodies []struct {
		Method string         `json:"method"`
		Path   string         `json:"path"`
		Schema map[string]any `json:"schema"`
	} `json:"requestBodies"`
}

func (p *integrationPlane) description(t *testing.T) descriptionBody {
	t.Helper()

	status, body := p.call(t, http.MethodGet, "http://"+p.operator+"/operator/v1", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the deployment description = %d: %s", status, body)
	}
	var document descriptionBody
	decodeInto(t, body, &document)
	return document
}

func (d descriptionBody) listing(method, path string) (int, bool) {
	for index, listing := range d.Listings {
		if listing.Method == method && listing.Path == path {
			return index, true
		}
	}
	return 0, false
}

// The base of the API answers its own index. It was a 404, which is why "the control plane
// does not serve it" kept getting written down as fact about routes that exist.
func TestDescription_TheOperatorAPIBaseAnswersWhatThisDeploymentServes(t *testing.T) {
	plane := startIntegrationPlane(t)
	document := plane.description(t)

	if len(document.Routes) == 0 {
		t.Fatal("the description names no routes")
	}
	// The routes a console needs and could not find by grepping this repository, because
	// identity builds its routes through constructors rather than as literal strings. That
	// is the property that made a shipped sign-in surface get recorded as unimplemented.
	for _, wanted := range []string{
		"/operator/v1/organizations/{organization}/sign-in/local",
		"/operator/v1/organizations/{organization}/sign-in/oidc",
		"/operator/v1/sign-in/callback",
	} {
		found := false
		for _, route := range document.Routes {
			found = found || route.Path == wanted
		}
		if !found {
			t.Errorf("the description does not name %s, which this deployment serves", wanted)
		}
	}
}

// A caller with no credential learns nothing. The document describes the deployment rather
// than a tenant, which is why it needs no permission — and that is not the same as public.
func TestDescription_IsNotReadableWithoutACredential(t *testing.T) {
	plane := startIntegrationPlane(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+plane.operator+"/operator/v1", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling the description: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)

	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("an anonymous read of the description = %d, want 401", response.StatusCode)
	}
}

// The point of the surfaces list: a 404 stops being how a console discovers what exists.
func TestDescription_NamesTheOptionalSurfacesAndWhetherTheyAreServed(t *testing.T) {
	plane := startIntegrationPlane(t)
	document := plane.description(t)

	found := false
	for _, surface := range document.Surfaces {
		if surface.Name != "conversations" {
			continue
		}
		found = true
		// This plane starts with the default configuration, which is off. What matters is
		// that the answer is STATED either way; the gate asserts both positions.
		if surface.Note == "" {
			t.Error("the conversations surface is named with no note")
		}
		if surface.Enabled {
			t.Log("this deployment serves conversations")
		}
	}
	if !found {
		t.Error("the description names no conversations surface, so a 404 is still the " +
			"only way to discover whether this deployment has one")
	}

	// The ROUTES are declared whether or not the surface is served: the table is the API's
	// index and is the same in every build. A console reads the surface entry, not the
	// presence of a route, to decide what to offer.
	routes := 0
	for _, route := range document.Routes {
		if strings.Contains(route.Path, "/conversations") {
			routes++
		}
	}
	if routes == 0 {
		t.Error("the description names no conversation routes; the table does not change " +
			"shape with configuration")
	}
}

// The listing contract is asserted by USING it. Comparing the document against a second
// list written in a test would pass while both were wrong together.
func TestDescription_AListingAcceptsExactlyWhatTheDocumentSaysItDoes(t *testing.T) {
	plane := startIntegrationPlane(t)
	document := plane.description(t)

	const path = "/operator/v1/organizations/{organization}/integrations"
	index, found := document.listing(http.MethodGet, path)
	if !found {
		t.Fatalf("the description says nothing about the integrations listing: %+v",
			document.Listings)
	}
	listing := document.Listings[index]
	if len(listing.Sortable) == 0 || listing.DefaultSort == "" {
		t.Fatalf("the integrations listing is described as ordered by nothing: %+v", listing)
	}

	base := plane.base(surfaceOrg) + "/integrations"
	for _, sortable := range listing.Sortable {
		for _, sign := range document.ListingRules.SortPrefixes {
			status, body := plane.call(t, http.MethodGet, base+"?sort="+sign+sortable, nil)
			if status != http.StatusOK {
				t.Errorf("the document offers sort=%s%s and it answered %d: %s",
					sign, sortable, status, body)
			}
		}
	}
	for _, filter := range listing.Filters {
		// The VALUE does not have to match anything. What is asserted is that the
		// parameter is understood: an undeclared one is refused rather than ignored, so a
		// 400 here would mean the document named a narrowing the endpoint does not serve.
		status, body := plane.call(t, http.MethodGet, base+"?"+filter+"=none", nil)
		if status != http.StatusOK && status != http.StatusBadRequest {
			t.Errorf("the document offers the %q filter and it answered %d: %s",
				filter, status, body)
		}
		if status == http.StatusBadRequest && strings.Contains(body, "does not accept") {
			t.Errorf("the document offers the %q filter and the listing refuses it: %s",
				filter, body)
		}
	}
	// And a parameter the document does NOT name is refused, so the published list is a
	// real contract rather than a description of "anything goes".
	if status, _ := plane.call(t, http.MethodGet, base+"?invented=1", nil); status !=
		http.StatusBadRequest {
		t.Errorf("an undeclared filter answered %d, want 400", status)
	}
}

// The shared rules are the ones the listings really apply.
func TestDescription_TheStatedPagingRulesAreTheOnesApplied(t *testing.T) {
	plane := startIntegrationPlane(t)
	document := plane.description(t)
	rules := document.ListingRules

	if rules.DefaultLimit <= 0 || rules.MaxLimit < rules.DefaultLimit {
		t.Fatalf("the stated paging rules are not a range: %+v", rules)
	}
	base := plane.base(surfaceOrg) + "/integrations"

	// Above the ceiling is CLAMPED rather than refused, which is what the document says
	// and what a client would otherwise have to discover by being rejected.
	if !rules.LimitIsClamped {
		t.Fatal("the document says a limit above the ceiling is refused; this deployment " +
			"clamps it, and the two must not disagree")
	}
	status, body := plane.call(t, http.MethodGet,
		base+"?limit="+strconv.Itoa(rules.MaxLimit*10), nil)
	if status != http.StatusOK {
		t.Errorf("a limit above the stated ceiling answered %d: %s", status, body)
	}

	// Every reserved name is UNDERSTOOD by a listing that declares none of them as
	// filters. The value is the listing's own, because reserved says the name is part of
	// the contract and not that anything may be said with it: an opaque cursor nobody was
	// issued is still refused, and rightly — a caller who invented one and was answered
	// page one would have no way to tell that from the end of the listing.
	index, found := document.listing(http.MethodGet,
		"/operator/v1/organizations/{organization}/integrations")
	if !found {
		t.Fatalf("the integrations listing is not described: %+v", document.Listings)
	}
	values := map[string]string{
		"search": "anything",
		"sort":   strings.TrimPrefix(document.Listings[index].DefaultSort, "-"),
		"cursor": "",
		"limit":  "10",
	}
	for _, name := range rules.Reserved {
		value, known := values[name]
		if !known {
			t.Fatalf("the document reserves %q and this test has no value to try it with", name)
		}
		status, body := plane.call(t, http.MethodGet, base+"?"+name+"="+value, nil)
		if status != http.StatusOK {
			t.Errorf("the document reserves %q and %s=%s answered %d: %s",
				name, name, value, status, body)
		}
	}
}

// A published body schema has to be the one the decoder enforces, in both directions.
func TestDescription_ARequestBodySchemaIsTheOneTheDecoderEnforces(t *testing.T) {
	plane := startIntegrationPlane(t)
	document := plane.description(t)

	const path = "/operator/v1/organizations/{organization}/integrations"
	var schema map[string]any
	for _, body := range document.RequestBodies {
		if body.Method == http.MethodPost && body.Path == path {
			schema = body.Schema
		}
	}
	if schema == nil {
		t.Fatalf("the description says nothing about what creating an integration accepts: %+v",
			document.RequestBodies)
	}
	if schema["additionalProperties"] != false {
		t.Errorf("the schema is open and the decoder is closed: %+v", schema)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, expected := range []string{"type", "name", "configuration", "labels", "relayId"} {
		if _, present := properties[expected]; !present {
			t.Errorf("the schema omits %q, which the create operation accepts", expected)
		}
	}

	// A field the schema does not publish is refused, which is what makes publishing the
	// closed shape worth anything.
	status, _ := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/integrations",
		map[string]any{"type": "alertmanager", "name": "described", "invented": true})
	if status != http.StatusBadRequest {
		t.Errorf("a field the schema omits answered %d, want 400", status)
	}
}
