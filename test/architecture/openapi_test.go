package gates_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/webhooks"
	"gopkg.in/yaml.v3"
)

type openAPIDocument struct {
	Paths      map[string]openAPIPath `yaml:"paths"`
	Components struct {
		Schemas    map[string]openAPISchema    `yaml:"schemas"`
		Parameters map[string]openAPIParameter `yaml:"parameters"`
	} `yaml:"components"`
}

type openAPISchema struct {
	Ref                  string                   `yaml:"$ref"`
	Type                 string                   `yaml:"type"`
	AdditionalProperties any                      `yaml:"additionalProperties"`
	Properties           map[string]openAPISchema `yaml:"properties"`
	Items                *openAPISchema           `yaml:"items"`
	Enum                 []string                 `yaml:"enum"`
	Required             []string                 `yaml:"required"`
	OneOf                []openAPISchema          `yaml:"oneOf"`
	Const                string                   `yaml:"const"`
	Minimum              *int                     `yaml:"minimum"`
	Maximum              *int                     `yaml:"maximum"`
	MinLength            *int                     `yaml:"minLength"`
	MaxLength            *int                     `yaml:"maxLength"`
	Pattern              string                   `yaml:"pattern"`
}

type openAPIParameter struct {
	Name   string        `yaml:"name"`
	In     string        `yaml:"in"`
	Schema openAPISchema `yaml:"schema"`
}

type openAPIPath struct {
	Parameters []openAPIReference `yaml:"parameters"`
	Get        *openAPIOperation  `yaml:"get"`
	Post       *openAPIOperation  `yaml:"post"`
	Put        *openAPIOperation  `yaml:"put"`
	Patch      *openAPIOperation  `yaml:"patch"`
	Delete     *openAPIOperation  `yaml:"delete"`
}

type openAPIOperation struct {
	OperationID          string                      `yaml:"operationId"`
	Tags                 []string                    `yaml:"tags"`
	Security             *[]map[string][]string      `yaml:"security"`
	Parameters           []openAPIReference          `yaml:"parameters"`
	Responses            map[string]openAPIReference `yaml:"responses"`
	Access               string                      `yaml:"x-opencluster-access"`
	OrganizationSelector string                      `yaml:"x-opencluster-organization-selector"`
	CookieCSRF           string                      `yaml:"x-opencluster-cookie-csrf"`
}

type openAPIReference struct {
	Ref string `yaml:"$ref"`
}

func TestOpenAPIDescribesExactlyTheOperatorRoutes(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read the canonical OpenAPI document: %v", err)
	}

	var document openAPIDocument
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse the canonical OpenAPI document: %v", err)
	}

	documented := make(map[string]bool)
	operationIDs := make(map[string]string)
	for path, item := range document.Paths {
		for _, entry := range item.operations() {
			method, operation := entry.method, *entry.operation
			key := method + " " + path
			if operation.OperationID == "" {
				t.Errorf("%s has no stable operationId", key)
			} else if previous := operationIDs[operation.OperationID]; previous != "" {
				t.Errorf("operationId %q is shared by %s and %s", operation.OperationID, previous, key)
			} else {
				operationIDs[operation.OperationID] = key
			}
			if len(operation.Tags) == 0 {
				t.Errorf("%s has no product-resource tag", key)
			}
			if !hasSuccessfulResponse(operation) {
				t.Errorf("%s has no concrete success or redirect response", key)
			}
			if documented[key] {
				t.Errorf("%s appears more than once in OpenAPI", key)
			}
			documented[key] = true
		}
	}

	served := make(map[string]bool)
	composedIntake := webhooks.Handlers{
		Slack: &webhooks.SlackAgent{SigningSecret: "architecture-test-secret"},
	}.Router()
	for _, route := range webhooks.InboundRoutes() {
		served[route.Method+" "+route.Pattern] = true
		path := strings.Replace(route.Pattern, "{integration}",
			"00000000-0000-0000-0000-000000000001", 1)
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		composedIntake.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("composed intake does not register %s: GET probe returned %d",
				route.Method+" "+route.Pattern, response.Code)
		}
	}
	for _, route := range operatorRoutes(t) {
		served[route.Key()] = true
		operation, ok := operationFor(document, route.Method(), route.Pattern())
		if !ok {
			t.Errorf("served route %s is missing from OpenAPI", route.Key())
			continue
		}
		wantAccess := route.Access().String()
		if operation.Access != wantAccess {
			t.Errorf("%s documents access %q, want %q", route.Key(), operation.Access, wantAccess)
		}
		wantSelector := "forbidden"
		if route.OrganizationScoped() {
			wantSelector = "required"
		} else if route.OrganizationOptional() {
			wantSelector = "optional"
		}
		if operation.OrganizationSelector != wantSelector {
			t.Errorf("%s documents Organization selector %q, want %q",
				route.Key(), operation.OrganizationSelector, wantSelector)
		}
		path := document.Paths[route.Pattern()]
		hasRequiredSelector := hasParameter(path, operation, "#/components/parameters/Organization")
		hasOptionalSelector := hasParameter(path, operation, "#/components/parameters/OptionalOrganization")
		switch wantSelector {
		case "required":
			if !hasRequiredSelector || hasOptionalSelector {
				t.Errorf("%s must reference only the required shared Organization selector", route.Key())
			}
		case "optional":
			if hasRequiredSelector || !hasOptionalSelector {
				t.Errorf("%s must reference only the optional shared Organization selector", route.Key())
			}
		case "forbidden":
			if hasRequiredSelector || hasOptionalSelector {
				t.Errorf("%s forbids the Organization selector but declares its parameter", route.Key())
			}
		}
		wantCSRF := "not-required"
		if route.Access() != authz.AccessPublic && unsafeMethod(route.Method()) {
			wantCSRF = "required-for-unsafe-cookie-request"
		}
		if operation.CookieCSRF != wantCSRF {
			t.Errorf("%s documents cookie CSRF %q, want %q", route.Key(), operation.CookieCSRF, wantCSRF)
		}
		switch {
		case operation.Security == nil:
			t.Errorf("%s must declare standard OpenAPI security explicitly", route.Key())
		case route.Access() == authz.AccessPublic && route.Pattern() != "/api/v1/auth/local/bootstrap" && len(*operation.Security) != 0:
			t.Errorf("public route %s must declare an empty security requirement", route.Key())
		case route.Access() != authz.AccessPublic && len(*operation.Security) == 0:
			t.Errorf("protected route %s must declare a security requirement", route.Key())
		}
	}
	for key := range served {
		if !strings.HasPrefix(key, "POST /webhooks/") {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		operation, ok := operationFor(document, method, path)
		if !ok {
			t.Errorf("served intake route %s is missing from OpenAPI", key)
			continue
		}
		if operation.Access != "webhook" || operation.OrganizationSelector != "forbidden" ||
			operation.CookieCSRF != "not-required" {
			t.Errorf("%s does not declare the webhook security boundary", key)
		}
		if operation.Security == nil || len(*operation.Security) == 0 {
			t.Errorf("%s must declare its provider credential scheme", key)
		}
	}
	for key := range documented {
		if !served[key] {
			t.Errorf("OpenAPI operation %s is not served", key)
		}
	}

	for _, generic := range []string{
		"#/components/requestBodies/JsonBody",
		"#/components/responses/Success",
	} {
		if strings.Contains(string(contents), generic) {
			t.Errorf("canonical operations must use resource-specific contracts, found %s", generic)
		}
	}
	for key, expected := range map[string]string{
		"POST /api/v1/auth/local/bootstrap": "#/components/responses/BootstrapCreated",
		"POST /api/v1/auth/local/sign-in":   "#/components/responses/SignIn",
		"DELETE /api/v1/session":            "#/components/responses/SignOut",
	} {
		method, path, _ := strings.Cut(key, " ")
		operation, ok := operationFor(document, method, path)
		if !ok || operation.Responses[successStatus(key)].Ref != expected {
			t.Errorf("%s must use its observable response contract %s", key, expected)
		}
	}
}

func TestOpenAPIListOperationsDeclareTheirQueryCapabilities(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document openAPIDocument
	if err = yaml.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}

	paged := []string{"Cursor", "Limit"}
	expected := map[string][]string{
		"listOrganizations":         paged,
		"listEffectivePermissions":  paged,
		"listMembers":               paged,
		"listSessions":              paged,
		"listAuditEvents":           paged,
		"listIntegrationTypes":      paged,
		"listIntegrations":          append(slices.Clone(paged), "IntegrationSearch", "IntegrationSort", "IntegrationTypeFilter", "RelayFilter", "DisabledFilter"),
		"listRelays":                append(slices.Clone(paged), "RelaySearch", "RelaySort", "RelayStateFilter", "RelayVersionFilter", "RelayCapabilityFilter"),
		"listRelayIntegrations":     paged,
		"listRelayFailures":         paged,
		"listRelaySessionConflicts": paged,
		"listIncidents":             append(slices.Clone(paged), "IncidentSearch", "IncidentSort", "IncidentIntegrationFilter", "IncidentStatusFilter"),
		"listIncidentAlertEvents":   paged,
		"listInvestigations":        append(slices.Clone(paged), "InvestigationIncidentFilter"),
		"listConversations":         append(slices.Clone(paged), "ConversationSearch", "ConversationSort", "ConversationIncidentFilter", "ConversationStateFilter"),
		"listWebhookDeliveries":     append(slices.Clone(paged), "WebhookDeliveryStatus"),
	}

	for pathName, path := range document.Paths {
		for _, entry := range path.operations() {
			operation := *entry.operation
			want, listed := expected[operation.OperationID]
			if !listed {
				continue
			}
			var got []string
			for _, parameter := range append(path.Parameters, operation.Parameters...) {
				name := strings.TrimPrefix(parameter.Ref, "#/components/parameters/")
				switch name {
				case "", "Organization", "OptionalOrganization", "UserID", "MembershipID",
					"SessionID", "IntegrationID", "IntegrationType", "RelayRegistrationID",
					"IncidentID", "InvestigationID", "ConversationID", "WebhookDeliveryID":
					continue
				}
				got = append(got, name)
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s %s parameters = %v, want %v", entry.method, pathName, got, want)
			}
			delete(expected, operation.OperationID)
		}
	}
	if len(expected) != 0 {
		t.Errorf("missing list operations: %v", expected)
	}

	limit := document.Components.Parameters["Limit"].Schema
	cursor := document.Components.Parameters["Cursor"].Schema
	if limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != 200 {
		t.Errorf("limit bounds = %v..%v, want 1..200", limit.Minimum, limit.Maximum)
	}
	if cursor.MinLength == nil || *cursor.MinLength != 1 ||
		cursor.MaxLength == nil || *cursor.MaxLength != 512 {
		t.Errorf("cursor bounds = %v..%v, want 1..512", cursor.MinLength, cursor.MaxLength)
	}
	for _, name := range []string{
		"IntegrationSearch", "RelaySearch", "IncidentSearch", "ConversationSearch",
	} {
		search := document.Components.Parameters[name].Schema
		if search.MinLength == nil || *search.MinLength != 1 ||
			search.MaxLength == nil || *search.MaxLength != 256 {
			t.Errorf("%s bounds = %v..%v, want 1..256", name, search.MinLength, search.MaxLength)
		}
	}
	for _, name := range []string{"RelayVersionFilter", "RelayCapabilityFilter"} {
		filter := document.Components.Parameters[name].Schema
		if filter.Pattern != `^\S(?:.*\S)?$` {
			t.Errorf("%s pattern = %q, want no surrounding whitespace", name, filter.Pattern)
		}
	}
}

func TestOpenAPIAdvertisesOnlyShippedProductAuthentication(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read the canonical OpenAPI document: %v", err)
	}
	if strings.Contains(string(contents), "BearerToken") {
		t.Fatal("canonical OpenAPI advertises a general bearer token that v0.1 does not ship")
	}
}

func TestOpenAPITypesEveryInvestigationResult(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read the canonical OpenAPI document: %v", err)
	}
	var document openAPIDocument
	if err = yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse the canonical OpenAPI document: %v", err)
	}

	investigation := document.Components.Schemas["Investigation"]
	for _, retired := range []string{"integrationId", "spend"} {
		if _, present := investigation.Properties[retired]; present {
			t.Errorf("Investigation schema still contains retired field %s", retired)
		}
	}
	usage := investigation.Properties["usage"]
	if _, input := usage.Properties["inputTokens"]; !input {
		t.Error("Investigation usage omits inputTokens")
	}
	if _, output := usage.Properties["outputTokens"]; !output {
		t.Error("Investigation usage omits outputTokens")
	}
	for field, schemaName := range map[string]string{
		"impact": "ImpactAssessment", "findings": "Finding",
		"hypotheses": "Hypothesis", "actions": "ActionProposal",
		"limitations": "Limitation",
	} {
		property := investigation.Properties[field]
		if field == "impact" {
			if property.Ref != "#/components/schemas/"+schemaName {
				t.Errorf("Investigation.%s = %q, want typed %s", field, property.Ref, schemaName)
			}
			continue
		}
		if property.Items == nil || property.Items.Ref != "#/components/schemas/"+schemaName {
			t.Errorf("Investigation.%s does not contain typed %s items", field, schemaName)
		}
	}
	for _, name := range []string{"ImpactAssessment", "Finding", "Hypothesis", "ActionProposal", "Limitation"} {
		schema, present := document.Components.Schemas[name]
		closed, isBoolean := schema.AdditionalProperties.(bool)
		if !present || !isBoolean || closed {
			t.Errorf("%s must exist and reject undeclared fields", name)
		}
	}
	request := document.Components.Schemas["OpenInvestigationRequest"]
	if !slices.Equal(request.Required, []string{"incidentId"}) || len(request.Properties) != 1 {
		t.Errorf("direct Investigation creation is not incident-only: %+v", request)
	}
}

func TestOpenAPIDiscriminatesEveryShippedInvestigationEvent(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read the canonical OpenAPI document: %v", err)
	}
	var document openAPIDocument
	if err = yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse the canonical OpenAPI document: %v", err)
	}

	event := document.Components.Schemas["InvestigationEvent"]
	if len(event.OneOf) != 9 {
		t.Fatalf("InvestigationEvent has %d variants, want eight active events and one fallback", len(event.OneOf))
	}
	for _, name := range []string{
		"StartedInvestigationEvent", "ProgressInvestigationEvent", "ToolStartedInvestigationEvent",
		"ToolCompletedInvestigationEvent", "HypothesesUpdatedInvestigationEvent", "ConcludedInvestigationEvent",
		"FailedInvestigationEvent", "CancelledInvestigationEvent", "UnknownInvestigationEvent",
	} {
		found := false
		for _, variant := range event.OneOf {
			found = found || variant.Ref == "#/components/schemas/"+name
		}
		if !found {
			t.Errorf("InvestigationEvent is missing %s", name)
		}
		variant := document.Components.Schemas[name]
		for _, field := range []string{
			"schemaVersion", "organizationId", "investigationId", "sequence", "at", "type", "payload",
		} {
			if _, present := variant.Properties[field]; !present {
				t.Errorf("%s omits shipped envelope field %s", name, field)
			}
			if !slices.Contains(variant.Required, field) {
				t.Errorf("%s does not require shipped envelope field %s", name, field)
			}
		}
	}
}

func hasSuccessfulResponse(operation openAPIOperation) bool {
	for status := range operation.Responses {
		if strings.HasPrefix(status, "2") || strings.HasPrefix(status, "3") {
			return true
		}
	}
	return false
}

func hasParameter(path openAPIPath, operation openAPIOperation, reference string) bool {
	parameters := append(append([]openAPIReference(nil), path.Parameters...), operation.Parameters...)
	for _, parameter := range parameters {
		if parameter.Ref == reference {
			return true
		}
	}
	return false
}

func successStatus(key string) string {
	if strings.Contains(key, "bootstrap") {
		return "201"
	}
	return "200"
}

type operationEntry struct {
	method    string
	operation *openAPIOperation
}

func (path openAPIPath) operations() []operationEntry {
	candidates := []operationEntry{
		{"GET", path.Get}, {"POST", path.Post}, {"PUT", path.Put},
		{"PATCH", path.Patch}, {"DELETE", path.Delete},
	}
	operations := make([]operationEntry, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.operation != nil {
			operations = append(operations, candidate)
		}
	}
	return operations
}

func operationFor(document openAPIDocument, method, path string) (openAPIOperation, bool) {
	item, ok := document.Paths[path]
	if !ok {
		return openAPIOperation{}, false
	}
	for _, entry := range item.operations() {
		if entry.method == strings.ToUpper(method) {
			return *entry.operation, true
		}
	}
	return openAPIOperation{}, false
}

func unsafeMethod(method string) bool {
	switch method {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}
