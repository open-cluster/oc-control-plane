package identity

import (
	"net/http"
	"strings"
)

// What a directory reads before it does anything else.
//
// Okta and Entra both fetch these three at configuration time and branch on what they say — in
// particular on whether PATCH and filtering are supported, and on how large a page may be. They
// are answered honestly rather than optimistically: this build does not support the whole of
// PATCH, it supports the operations named in scim.go, and a service provider configuration that
// claimed otherwise would produce a directory sending documents this surface refuses.

func (h Handlers) scimServiceProviderConfig(writer http.ResponseWriter, request *http.Request) {
	_, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas": []string{scimConfigSchema},
		"documentationUri": strings.TrimSuffix(h.ConsoleURL, "/") +
			"/settings/identity/provisioning",
		"patch":  map[string]any{"supported": true},
		"filter": map[string]any{"supported": true, "maxResults": scimMaxResults},
		// Not supported, and each says so rather than being omitted. A directory that read an
		// absent capability as present would send a document this surface refuses, and the
		// administrator would see a synchronisation error with no cause.
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"changePassword": map[string]any{"supported": false},
		"sort":           map[string]any{"supported": false},
		"etag":           map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "An API token this organization issued, holding the directory synchroniser role and nothing else.",
			"primary":     true,
		}},
		"meta": map[string]any{
			"resourceType": "ServiceProviderConfig",
			"location":     h.scimBase(organization) + "/ServiceProviderConfig",
		},
	})
}

func (h Handlers) scimResourceTypes(writer http.ResponseWriter, request *http.Request) {
	_, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	base := h.scimBase(organization)
	resources := []map[string]any{
		{
			"schemas":  []string{scimResourceType},
			"id":       "User",
			"name":     "User",
			"endpoint": "/Users",
			"schema":   scimUserSchema,
			"meta": map[string]any{
				"resourceType": "ResourceType", "location": base + "/ResourceTypes/User",
			},
		},
		{
			"schemas":  []string{scimResourceType},
			"id":       "Group",
			"name":     "Group",
			"endpoint": "/Groups",
			"schema":   scimGroupSchema,
			"meta": map[string]any{
				"resourceType": "ResourceType", "location": base + "/ResourceTypes/Group",
			},
		},
	}
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListSchema},
		"totalResults": len(resources),
		"startIndex":   1,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

// scimSchemas reports the attributes this service actually keeps.
//
// It is deliberately narrower than the standard's User and Group schemas. A directory reading
// it learns that this product stores a userName, an address, a display name and whether the
// person is active — and that everything else it might send is discarded. Publishing the full
// schema would be claiming to keep fields that are dropped.
func (h Handlers) scimSchemas(writer http.ResponseWriter, request *http.Request) {
	_, organization, ok := h.scimCaller(writer, request)
	if !ok {
		return
	}
	base := h.scimBase(organization)
	resources := []map[string]any{
		{
			"id":          scimUserSchema,
			"name":        "User",
			"description": "A person who may reach this organization.",
			"attributes": []map[string]any{
				scimAttribute("userName", "string", true, "server"),
				scimAttribute("externalId", "string", false, "none"),
				scimAttribute("displayName", "string", false, "none"),
				scimAttribute("active", "boolean", false, "none"),
				scimAttribute("emails", "complex", false, "none"),
			},
			"meta": map[string]any{
				"resourceType": "Schema", "location": base + "/Schemas/" + scimUserSchema,
			},
		},
		{
			"id":          scimGroupSchema,
			"name":        "Group",
			"description": "A directory group. What it grants here is an administrator's decision, not this service's input.",
			"attributes": []map[string]any{
				scimAttribute("displayName", "string", true, "server"),
				scimAttribute("externalId", "string", false, "none"),
				scimAttribute("members", "complex", false, "none"),
			},
			"meta": map[string]any{
				"resourceType": "Schema", "location": base + "/Schemas/" + scimGroupSchema,
			},
		},
	}
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListSchema},
		"totalResults": len(resources),
		"startIndex":   1,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

func scimAttribute(name, kind string, required bool, uniqueness string) map[string]any {
	return map[string]any{
		"name":        name,
		"type":        kind,
		"multiValued": kind == "complex",
		"required":    required,
		"caseExact":   false,
		"mutability":  "readWrite",
		"returned":    "default",
		"uniqueness":  uniqueness,
	}
}
