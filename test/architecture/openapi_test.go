package gates_test

import (
	"os"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"gopkg.in/yaml.v3"
)

type openAPIDocument struct {
	Paths map[string]openAPIPath `yaml:"paths"`
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
