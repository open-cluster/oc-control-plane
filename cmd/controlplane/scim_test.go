package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/authz"
)

// SCIM, driven the way a directory drives it.
//
// Every request here is one Okta or Microsoft Entra actually sends, in the order they send it:
// read the service provider configuration, probe for a person by userName, create them, create
// a group, add them to it, and later set them inactive. What is asserted is what a directory
// observes plus what the CONTROL PLANE does about it — because the second is the whole point
// and no directory can see it.

// scimClient is a directory: a base URL and a bearer token, which is all Okta and Entra are
// given.
type scimClient struct {
	plane  *identityPlane
	base   string
	secret string
}

// asDirectory issues a token holding the directory synchroniser role and nothing else, which is
// exactly what an administrator would paste into their identity vendor.
func asDirectory(t *testing.T, plane *identityPlane) *scimClient {
	t.Helper()

	created := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/service-accounts",
		map[string]any{"name": "directory " + uniqueSuffix(), "description": "the customer's IdP"},
		asBootstrap)
	if created.status != http.StatusCreated {
		t.Fatalf("creating the directory's account = %d: %s", created.status, created.body)
	}
	var account struct {
		ID string `json:"id"`
	}
	decodeAnswer(t, created, &account)

	issued := plane.call(t, http.MethodPost, plane.base(identityOrg)+"/api-tokens",
		map[string]any{
			"serviceAccountId": account.ID,
			"role":             string(authz.DirectorySynchroniser),
			"expiresInSeconds": 3600,
		}, asBootstrap)
	if issued.status != http.StatusCreated {
		t.Fatalf("issuing the directory's token = %d: %s", issued.status, issued.body)
	}
	var minted struct {
		Secret string `json:"secret"`
	}
	decodeAnswer(t, issued, &minted)

	return &scimClient{
		plane:  plane,
		base:   "http://" + plane.operator + "/scim/v2/organizations/" + identityOrg,
		secret: minted.Secret,
	}
}

func uniqueSuffix() string { return time.Now().Format("150405.000000000") }

// send makes one request the way a directory makes it: a bearer token, a SCIM content type, and
// no Origin — because a directory is not a browser and requiring one would break every real
// integration.
func (c *scimClient) send(t *testing.T, method, path string, body any) answer {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the body: %v", err)
		}
		payload = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, payload)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.secret)
	if body != nil {
		request.Header.Set("Content-Type", "application/scim+json")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling %s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	return answer{status: response.StatusCode, body: string(raw)}
}

type scimUserBody struct {
	ID         string   `json:"id"`
	UserName   string   `json:"userName"`
	ExternalID string   `json:"externalId"`
	Active     bool     `json:"active"`
	Roles      []string `json:"roles"`
}

type scimListBody struct {
	TotalResults int               `json:"totalResults"`
	Resources    []json.RawMessage `json:"Resources"`
}

type scimGroupBody struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	Members     []struct {
		Value string `json:"value"`
	} `json:"members"`
}

// Story 13: a directory provisions users and groups, so the customer's directory stays the
// source of truth.
func TestOperatorSCIM_ADirectoryProvisionsPeopleAndGroups(t *testing.T) {
	plane := startIdentityPlane(t)
	directory := asDirectory(t, plane)

	// What a directory reads first. It branches on this, so an answer that overclaimed would
	// produce documents this surface then refuses.
	config := directory.send(t, http.MethodGet, "/ServiceProviderConfig", nil)
	if config.status != http.StatusOK {
		t.Fatalf("reading the service provider configuration = %d: %s",
			config.status, config.body)
	}
	// The keys come out sorted, because the document is a Go map. What matters is what it
	// says, not the order it says it in.
	for _, wanted := range []string{`"patch":{"supported":true}`, `"supported":false`} {
		if !strings.Contains(strings.ReplaceAll(config.body, " ", ""), wanted) {
			t.Errorf("the configuration does not state %s:\n%s", wanted, config.body)
		}
	}
	if resources := directory.send(t, http.MethodGet, "/ResourceTypes", nil); resources.status !=
		http.StatusOK {
		t.Errorf("reading the resource types = %d", resources.status)
	}
	if schemas := directory.send(t, http.MethodGet, "/Schemas", nil); schemas.status !=
		http.StatusOK {
		t.Errorf("reading the schemas = %d", schemas.status)
	}

	// The existence probe every directory makes before it creates anybody. An unfiltered answer
	// here is how a directory concludes somebody does not exist and creates them twice.
	probe := directory.send(t, http.MethodGet,
		`/Users?filter=userName%20eq%20%22ada@example.test%22`, nil)
	if probe.status != http.StatusOK {
		t.Fatalf("the existence probe = %d: %s", probe.status, probe.body)
	}
	var found scimListBody
	decodeAnswer(t, probe, &found)
	if found.TotalResults != 0 {
		t.Fatalf("a person nobody provisioned was found: %s", probe.body)
	}

	created := directory.send(t, http.MethodPost, "/Users", map[string]any{
		"schemas":    []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName":   "ada@example.test",
		"externalId": "okta-00u1",
		"name":       map[string]any{"givenName": "Ada", "familyName": "Lovelace"},
		"emails":     []map[string]any{{"value": "ada@example.test", "primary": true}},
		"active":     true,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("provisioning a person = %d: %s", created.status, created.body)
	}
	var person scimUserBody
	decodeAnswer(t, created, &person)
	if person.ID == "" || !person.Active || person.ExternalID != "okta-00u1" {
		t.Fatalf("the provisioned person reads back as %+v", person)
	}

	// The same create again. A directory that retried must be told it already exists rather
	// than getting a second person, and the standard's own scimType is what stops it looping.
	again := directory.send(t, http.MethodPost, "/Users", map[string]any{
		"userName": "ada@example.test", "externalId": "okta-00u1", "active": true,
	})
	if again.status != http.StatusConflict {
		t.Errorf("a repeated create = %d, want 409: %s", again.status, again.body)
	}
	if !strings.Contains(again.body, "uniqueness") {
		t.Errorf("the conflict does not carry the standard's scimType: %s", again.body)
	}

	// A person in no mapped group holds no role and reaches nothing. That is the correct
	// default: being in the company is not being in this product.
	if len(person.Roles) != 0 {
		t.Errorf("a person in no mapped group holds %v", person.Roles)
	}

	// A group, and the person in it.
	group := directory.send(t, http.MethodPost, "/Groups", map[string]any{
		"schemas":     []string{"urn:ietf:params:scim:schemas:core:2.0:Group"},
		"displayName": "OpenCluster Investigators",
		"externalId":  "okta-00g1",
		"members":     []map[string]any{{"value": person.ID}},
	})
	if group.status != http.StatusCreated {
		t.Fatalf("creating a group = %d: %s", group.status, group.body)
	}
	var provisionedGroup scimGroupBody
	decodeAnswer(t, group, &provisionedGroup)
	if len(provisionedGroup.Members) != 1 {
		t.Fatalf("the group reads back with %d members", len(provisionedGroup.Members))
	}

	// Still nothing, because nobody has said what the group means here. This is the property
	// that keeps a directory from being able to grant itself access.
	afterGroup := readSCIMUser(t, directory, person.ID)
	if len(afterGroup.Roles) != 0 {
		t.Errorf("membership of an unmapped group granted %v; a directory must not be able to "+
			"decide what its groups mean in this product", afterGroup.Roles)
	}

	// The administrator's decision, on the OPERATOR surface and behind identity.configure.
	mapped := plane.call(t, http.MethodPut,
		plane.base(identityOrg)+"/directory-groups/"+provisionedGroup.ID+"/role",
		map[string]string{"role": "editor"}, asBootstrap)
	if mapped.status != http.StatusOK {
		t.Fatalf("mapping the group = %d: %s", mapped.status, mapped.body)
	}

	// And now the person holds it — without signing in, and without the directory doing
	// anything further.
	afterMapping := readSCIMUser(t, directory, person.ID)
	if len(afterMapping.Roles) != 1 || afterMapping.Roles[0] != string(authz.Editor) {
		t.Errorf("mapping the group produced %v, want the investigator role", afterMapping.Roles)
	}

	// The administrator's view of the same groups, which is where they decide.
	listed := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/directory-groups",
		nil, asBootstrap)
	if !strings.Contains(listed.body, "OpenCluster Investigators") ||
		!strings.Contains(listed.body, "editor") {
		t.Errorf("the administrator's view does not show the group and what it grants: %s",
			listed.body)
	}
}

// Story 14: a person removed in the directory loses access without a manual step — and on their
// NEXT REQUEST rather than at their next sign-in, which is the part a directory cannot observe
// and the part that matters.
func TestOperatorSCIM_DeprovisioningEndsAccessImmediately(t *testing.T) {
	plane := startIdentityPlane(t)
	directory := asDirectory(t, plane)
	idp := newSAMLIdP(t, "https://idp.scim.example.test/entity",
		"https://idp.scim.example.test/sso")

	// The whole arrangement a customer actually has: a directory that provisions, and a
	// provider people sign in through.
	provider := configureSAMLProvider(t, plane, idp, map[string]any{
		"name": "SCIM tenant IdP",
	})

	created := directory.send(t, http.MethodPost, "/Users", map[string]any{
		"userName": "leaver@example.test", "externalId": "okta-leaver", "active": true,
		"emails": []map[string]any{{"value": "leaver@example.test", "primary": true}},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("provisioning = %d: %s", created.status, created.body)
	}
	var person scimUserBody
	decodeAnswer(t, created, &person)

	group := directory.send(t, http.MethodPost, "/Groups", map[string]any{
		"displayName": "Leavers Group", "members": []map[string]any{{"value": person.ID}},
	})
	var provisionedGroup scimGroupBody
	decodeAnswer(t, group, &provisionedGroup)

	if mapped := plane.call(t, http.MethodPut,
		plane.base(identityOrg)+"/directory-groups/"+provisionedGroup.ID+"/role",
		map[string]string{"role": "viewer"}, asBootstrap); mapped.status != http.StatusOK {
		t.Fatalf("mapping = %d: %s", mapped.status, mapped.body)
	}

	// They sign in. The provisioned person and the person signing in are ONE — the directory
	// created a placeholder identity and the sign-in adopted it — which is why this works at
	// all and is the subtlest thing in the whole feature.
	idp.attributes = map[string][]string{
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": {
			"leaver@example.test",
		},
	}
	completed := signInThroughSAML(t, plane, idp, provider, "leaver@example.test")
	if completed.status != http.StatusFound {
		t.Fatalf("a provisioned person could not sign in = %d: %s",
			completed.status, completed.body)
	}
	cookie := sessionCookie(t, completed)

	who := readSession(t, plane, cookie)
	if who.Principal.ID != person.ID {
		t.Errorf("the person who signed in is %s and the directory provisioned %s; a "+
			"provisioned person signing in must be the same person, not a second account",
			who.Principal.ID, person.ID)
	}
	if roster := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays", nil,
		asSession(cookie)); roster.status != http.StatusOK {
		t.Fatalf("the provisioned person cannot read: %d", roster.status)
	}

	t.Run("removed from the group that granted the role", func(t *testing.T) {
		removed := directory.send(t, http.MethodPatch, "/Groups/"+provisionedGroup.ID,
			map[string]any{
				"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
				"Operations": []map[string]any{{
					"op":   "remove",
					"path": `members[value eq "` + person.ID + `"]`,
				}},
			})
		if removed.status != http.StatusOK {
			t.Fatalf("removing from the group = %d: %s", removed.status, removed.body)
		}

		after := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays", nil,
			asSession(cookie))
		if after.status != http.StatusUnauthorized {
			t.Errorf("a person whose last mapped group was taken away answered %d, want 401 — "+
				"losing the role that granted access has to end the session that rested on it",
				after.status)
		}
	})

	t.Run("set inactive by the directory", func(t *testing.T) {
		// Back in, so there is something to take away again.
		back := directory.send(t, http.MethodPatch, "/Groups/"+provisionedGroup.ID,
			map[string]any{
				"Operations": []map[string]any{{
					"op": "add", "path": "members",
					"value": []map[string]any{{"value": person.ID}},
				}},
			})
		if back.status != http.StatusOK {
			t.Fatalf("adding back to the group = %d: %s", back.status, back.body)
		}
		second := signInThroughSAML(t, plane, idp, provider, "leaver@example.test")
		if second.status != http.StatusFound {
			t.Fatalf("signing in again = %d: %s", second.status, second.body)
		}
		reissued := sessionCookie(t, second)

		// The operation Okta sends when somebody is deactivated.
		inactive := directory.send(t, http.MethodPatch, "/Users/"+person.ID, map[string]any{
			"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
			"Operations": []map[string]any{
				{"op": "replace", "path": "active", "value": false},
			},
		})
		if inactive.status != http.StatusOK {
			t.Fatalf("deactivating = %d: %s", inactive.status, inactive.body)
		}

		if gone := plane.call(t, http.MethodGet, plane.base(identityOrg)+"/relays", nil,
			asSession(reissued)); gone.status != http.StatusUnauthorized {
			t.Errorf("a deactivated person answered %d, want 401", gone.status)
		}
		// And the directory can still read them back, which SCIM requires: there is no "gone",
		// there is active false.
		if read := readSCIMUser(t, directory, person.ID); read.Active {
			t.Error("a deactivated person reads back as active")
		}
	})

	t.Run("deleted outright", func(t *testing.T) {
		deleted := directory.send(t, http.MethodDelete, "/Users/"+person.ID, nil)
		if deleted.status != http.StatusNoContent {
			t.Fatalf("deleting = %d: %s", deleted.status, deleted.body)
		}
		if read := directory.send(t, http.MethodGet, "/Users/"+person.ID, nil); read.status !=
			http.StatusNotFound {
			t.Errorf("a deleted person reads back as %d", read.status)
		}
		// The audit trail still names them. Deleting the person would take the meaning of every
		// event they produced with it.
		trail := plane.call(t, http.MethodGet,
			plane.base(identityOrg)+"/audit-events?limit=200", nil, asBootstrap)
		if !strings.Contains(trail.body, person.ID) {
			t.Error("the record no longer names a person the directory deleted; their events " +
				"would name an identifier nothing resolves")
		}
	})
}

// A directory's credential reaches the provisioning endpoints and nothing else. It lives in a
// customer's identity vendor, so what it can do when it leaks is the whole question.
func TestOperatorSCIM_TheDirectoryCredentialReachesNothingElse(t *testing.T) {
	plane := startIdentityPlane(t)
	directory := asDirectory(t, plane)

	if users := directory.send(t, http.MethodGet, "/Users", nil); users.status != http.StatusOK {
		t.Fatalf("the directory cannot list users: %d %s", users.status, users.body)
	}

	for name, path := range map[string]string{
		"the relay roster":   "/relays",
		"the audit trail":    "/audit-events",
		"identity providers": "/identity-providers",
		"members":            "/members",
		"api tokens":         "/api-tokens",
		"integrations":       "/integrations",
		"the type catalog":   "/integration-types",
		"incidents":          "/incidents",
	} {
		t.Run(name, func(t *testing.T) {
			refused := plane.call(t, http.MethodGet, plane.base(identityOrg)+path, nil,
				asToken(directory.secret))
			if refused.status != http.StatusForbidden {
				t.Errorf("a directory credential reached %s: %d %s",
					name, refused.status, refused.body)
			}
		})
	}

	// And it cannot decide what its own groups grant, which is the escalation the whole design
	// turns on.
	group := directory.send(t, http.MethodPost, "/Groups",
		map[string]any{"displayName": "Escalation Attempt"})
	var provisioned scimGroupBody
	decodeAnswer(t, group, &provisioned)

	refused := plane.call(t, http.MethodPut,
		plane.base(identityOrg)+"/directory-groups/"+provisioned.ID+"/role",
		map[string]string{"role": "admin"}, asToken(directory.secret))
	if refused.status != http.StatusForbidden {
		t.Errorf("a directory mapped its own group to a role: %d %s",
			refused.status, refused.body)
	}

	// Nor may an administrator map one to owner. A directory group is edited by whoever
	// administers the customer's identity vendor, and an owner arriving that way is an
	// administrative takeover one directory edit wide.
	byOwner := plane.call(t, http.MethodPut,
		plane.base(identityOrg)+"/directory-groups/"+provisioned.ID+"/role",
		map[string]string{"role": "admin"}, asBootstrap)
	if byOwner.status != http.StatusBadRequest {
		t.Errorf("a group was mapped to the owner role: %d %s", byOwner.status, byOwner.body)
	}
}

// An operation this build does not understand is REFUSED, not ignored. A directory whose
// deprovisioning silently did nothing would leave everybody believing access had been removed,
// which is the worst failure this surface could have.
func TestOperatorSCIM_WhatIsNotUnderstoodIsRefused(t *testing.T) {
	plane := startIdentityPlane(t)
	directory := asDirectory(t, plane)

	created := directory.send(t, http.MethodPost, "/Users", map[string]any{
		"userName": "patched@example.test", "active": true,
	})
	var person scimUserBody
	decodeAnswer(t, created, &person)

	for name, body := range map[string]any{
		"a filter this service does not implement": nil,
		"a patch on an attribute it does not keep": map[string]any{
			"Operations": []map[string]any{
				{"op": "replace", "path": "locale", "value": "en-GB"},
			},
		},
		"a patch operation that is not add, remove or replace": map[string]any{
			"Operations": []map[string]any{{"op": "move", "path": "active", "value": false}},
		},
		"a pathless patch carrying more than it can apply": map[string]any{
			"Operations": []map[string]any{{
				"op":    "replace",
				"value": map[string]any{"active": false, "locale": "en-GB"},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var refused answer
			if body == nil {
				refused = directory.send(t, http.MethodGet,
					`/Users?filter=userName%20co%20%22example%22`, nil)
			} else {
				refused = directory.send(t, http.MethodPatch, "/Users/"+person.ID, body)
			}
			if refused.status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400: %s", name, refused.status, refused.body)
			}
			if !strings.Contains(refused.body, "scimType") {
				t.Errorf("the refusal carries no scimType, so a directory cannot act on it: %s",
					refused.body)
			}
		})
	}

	// The person is untouched by every refusal above, which is what "refused rather than
	// partly applied" means.
	if read := readSCIMUser(t, directory, person.ID); !read.Active {
		t.Error("a refused patch changed the person anyway")
	}
}

func readSCIMUser(t *testing.T, directory *scimClient, id string) scimUserBody {
	t.Helper()

	answered := directory.send(t, http.MethodGet, "/Users/"+id, nil)
	if answered.status != http.StatusOK {
		t.Fatalf("reading a provisioned person = %d: %s", answered.status, answered.body)
	}
	var person scimUserBody
	decodeAnswer(t, answered, &person)
	return person
}
