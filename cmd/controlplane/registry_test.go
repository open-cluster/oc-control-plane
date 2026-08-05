package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The Integration catalog and the Connection lifecycle, asserted through the assembled process:
// real HTTP, a real database, a real relay.
//
// What is under test is the OBSERVABLE contract — given a seeded database and a request, what
// status, what body, what changed — rather than which repository method ran. The seam is the
// running process because a boundary proven against a double is a boundary proven against
// nothing.

// The catalog's envelope. It is the shared table contract, so these field names are the same
// ones every other listing on this surface answers with.
type catalogBody struct {
	Items   []catalogEntry `json:"items"`
	Next    *string        `json:"next"`
	Total   *int           `json:"total"`
	Partial []struct {
		Field  string `json:"field"`
		Reason string `json:"reason"`
	} `json:"partial"`
}

type catalogEntry struct {
	ID                  string          `json:"id"`
	Integration         string          `json:"integration"`
	Slug                string          `json:"slug"`
	DefinitionVersion   int             `json:"definitionVersion"`
	Name                string          `json:"name"`
	Description         string          `json:"description"`
	Category            string          `json:"category"`
	LogoAssetKey        string          `json:"logoAssetKey"`
	DocumentationURL    string          `json:"documentationUrl"`
	AuthModes           []string        `json:"authModes"`
	Roles               []string        `json:"roles"`
	ExecutionLocalities []string        `json:"executionLocalities"`
	ConfigurationSchema json.RawMessage `json:"configurationSchema"`
	PresentationSchema  struct {
		Sections []struct {
			Title  string   `json:"title"`
			Fields []string `json:"fields"`
		} `json:"sections"`
		ProviderStep string `json:"providerStep"`
	} `json:"presentationSchema"`
	ValidationContract struct {
		Authenticates      bool   `json:"authenticates"`
		ProbesCapabilities bool   `json:"probesCapabilities"`
		Note               string `json:"note"`
	} `json:"validationContract"`
	Capabilities              []string `json:"capabilities"`
	MinimumRelayVersion       string   `json:"minimumRelayVersion"`
	SupportsMultipleInstances bool     `json:"supportsMultipleInstances"`
	Lifecycle                 string   `json:"lifecycle"`
	Actionable                bool     `json:"actionable"`
	ConfiguredConnections     int      `json:"configuredConnections"`
}

func (c catalogBody) entry(t *testing.T, slug string) catalogEntry {
	t.Helper()
	for _, entry := range c.Items {
		if entry.Slug == slug {
			return entry
		}
	}
	t.Fatalf("the catalog has no entry for %q", slug)
	return catalogEntry{}
}

func TestIntegrationCatalog(t *testing.T) {
	plane := startConnectionPlane(t)
	base := plane.base(surfaceOrg)
	catalog := base + "/integrations"

	var listed catalogBody
	t.Run("the catalog carries everything a setup flow needs to render a provider", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, catalog, nil)
		if status != http.StatusOK {
			t.Fatalf("listing integrations = %d: %s", status, body)
		}
		decodeInto(t, body, &listed)
		if len(listed.Items) < 2 {
			t.Fatalf("the catalog has %d entries: %s", len(listed.Items), body)
		}
		if listed.Total == nil || *listed.Total != len(listed.Items) {
			t.Errorf("a compiled catalog answered a total of %v for %d entries; it can count "+
				"itself and should", listed.Total, len(listed.Items))
		}
		if listed.Next != nil {
			t.Errorf("a compiled catalog offered a next page: %v", *listed.Next)
		}

		kubernetes := listed.entry(t, "kubernetes")
		if kubernetes.Name == "" || kubernetes.Description == "" || kubernetes.Category == "" {
			t.Errorf("kubernetes renders as %+v; a tile cannot be drawn from it", kubernetes)
		}
		if kubernetes.DefinitionVersion == 0 {
			t.Error("no definition version, so a stale client cannot detect that it is stale")
		}
		if kubernetes.DocumentationURL == "" {
			t.Error("no setup documentation link (story 8)")
		}
		if len(kubernetes.Capabilities) == 0 {
			t.Error("kubernetes declares no capabilities, so an operator cannot see what " +
				"evidence connecting it makes available (story 5)")
		}
		if len(kubernetes.ExecutionLocalities) == 0 {
			t.Error("no execution locality, so an operator cannot tell whether they need to " +
				"install anything (story 6)")
		}
		if kubernetes.PresentationSchema.ProviderStep == "" {
			t.Error("kubernetes has no provider step; a service-account binding is exactly the " +
				"case a generic form cannot serve")
		}
		if len(kubernetes.ConfigurationSchema) == 0 {
			t.Error("no configuration schema, so a setup form has nothing to render from")
		}
	})

	t.Run("a provider with no adapter is listed, labelled, and not actionable", func(t *testing.T) {
		planned := listed.entry(t, "prometheus")
		if planned.Lifecycle != "planned" {
			t.Errorf("prometheus is %q, want planned", planned.Lifecycle)
		}
		if planned.Actionable {
			t.Error("prometheus reports as actionable; the frontend would render it clickable " +
				"and somebody would spend an afternoon on a connection that cannot work")
		}
		if planned.LogoAssetKey != "" {
			t.Errorf("prometheus names the logo %q and has no adapter; a mark beside a provider "+
				"nobody can connect reads as a product further along than it is",
				planned.LogoAssetKey)
		}
	})

	t.Run("creating a connection to a planned provider is refused with a reason", func(t *testing.T) {
		environment := plane.defaultEnvironment(t, surfaceOrg)
		status, body := plane.call(t, http.MethodPost, base+"/connections", map[string]any{
			"environmentId": environment.ID,
			"integration":   "prometheus",
			"name":          "Staging Prometheus",
			"role":          "evidence",
			"locality":      "control_plane",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("creating a connection to a planned provider = %d, want 400: %s", status, body)
		}
		if !containsString(strings.ToLower(body), "planned") {
			t.Errorf("the refusal does not say the provider is planned: %s", body)
		}
	})

	t.Run("the catalog counts what this tenant has configured", func(t *testing.T) {
		environment := plane.defaultEnvironment(t, surfaceOrg)
		for _, name := range []string{"Counted One", "Counted Two"} {
			status, body := plane.call(t, http.MethodPost, base+"/connections", map[string]any{
				"environmentId": environment.ID,
				"integration":   "alertmanager",
				"name":          name,
				"role":          "trigger",
				"locality":      "control_plane",
			})
			if status != http.StatusCreated {
				t.Fatalf("creating %s = %d: %s", name, status, body)
			}
		}

		status, body := plane.call(t, http.MethodGet, catalog, nil)
		if status != http.StatusOK {
			t.Fatalf("listing integrations = %d: %s", status, body)
		}
		var counted catalogBody
		decodeInto(t, body, &counted)
		if got := counted.entry(t, "alertmanager").ConfiguredConnections; got < 2 {
			t.Errorf("alertmanager reports %d configured connections, want at least the two "+
				"just created (story 4)", got)
		}
		if got := counted.entry(t, "prometheus").ConfiguredConnections; got != 0 {
			t.Errorf("prometheus reports %d configured connections and cannot be configured", got)
		}
	})

	t.Run("the catalog is searchable and filterable", func(t *testing.T) {
		status, body := plane.call(t, http.MethodGet, catalog+"?actionable=true", nil)
		if status != http.StatusOK {
			t.Fatalf("filtering the catalog = %d: %s", status, body)
		}
		var actionable catalogBody
		decodeInto(t, body, &actionable)
		if len(actionable.Items) == 0 {
			t.Fatal("filtering to actionable providers returned nothing")
		}
		for _, entry := range actionable.Items {
			if !entry.Actionable {
				t.Errorf("%s came back from actionable=true and is %q", entry.Slug, entry.Lifecycle)
			}
		}

		status, body = plane.call(t, http.MethodGet, catalog+"?search=kubernetes", nil)
		if status != http.StatusOK {
			t.Fatalf("searching the catalog = %d: %s", status, body)
		}
		var searched catalogBody
		decodeInto(t, body, &searched)
		if len(searched.Items) == 0 {
			t.Fatal("searching for kubernetes found nothing")
		}
	})

	t.Run("an unoffered sort or filter is refused rather than ignored", func(t *testing.T) {
		for _, query := range []string{"?sort=-secretDigest", "?organization=somebody-else"} {
			status, body := plane.call(t, http.MethodGet, catalog+query, nil)
			if status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400: a sort or filter silently dropped returns rows in "+
					"an order nobody chose, or everything while looking narrowed", query, status)
			}
			_ = body
		}
	})
}
