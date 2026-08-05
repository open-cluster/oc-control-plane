// Package gates holds build-failing architecture checks. They are tests rather than lint
// rules on purpose: a lint rule can be suppressed inline, and these properties are the ones
// a reviewer is least likely to notice being violated.
//
// The .golangci.yml depguard rules cover the same ground at the convention level and report
// sooner. Both must stay green; neither replaces the other.
package gates_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

const modulePath = "github.com/open-cluster/oc-control-plane"

// moduleRoot is where this package sits relative to the module, which is what every gate
// resolves its inputs against.
const moduleRoot = "../.."

// loadPackages parses the module's PRODUCTION packages. Test variants are excluded
// deliberately: a test may construct a database connection to arrange a scenario, and the
// property these gates protect is what ships in the binary, not what a fixture does.
func loadPackages(t *testing.T) []*packages.Package {
	t.Helper()

	loaded, err := packages.Load(&packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedImports,
		Tests: false,
		Dir:   moduleRoot,
	}, "./...")
	if err != nil {
		t.Fatalf("loading packages: %v", err)
	}
	if packages.PrintErrors(loaded) > 0 {
		t.Fatal("packages failed to load")
	}
	return loaded
}

// internalPackagePath reports the module-relative package path, or "" for anything outside
// this module.
func internalPackagePath(packagePath string) string {
	if !strings.HasPrefix(packagePath, modulePath) {
		return ""
	}
	trimmed := strings.TrimPrefix(packagePath, modulePath)
	return strings.TrimPrefix(trimmed, "/")
}

// Database access belongs to internal/storage, which owns placement resolution. A pool
// built anywhere else is a pool that bypasses it, and therefore a tenant-isolation defect
// rather than a style violation.
func TestOnlyStorageImportsTheDatabaseDriver(t *testing.T) {
	t.Parallel()

	const driver = "github.com/jackc/pgx/v5"
	allowed := map[string]bool{
		"internal/storage": true,
		// The gates themselves and the composition-root tests may name the driver to prove
		// these properties; neither ships in the binary's non-test build.
		"internal/gates": true,
	}

	for _, loaded := range loadPackages(t) {
		path := internalPackagePath(loaded.PkgPath)
		if path == "" || allowed[path] {
			continue
		}
		for imported := range loaded.Imports {
			if imported == driver || strings.HasPrefix(imported, driver+"/") {
				t.Errorf("%s imports %s; database access belongs in internal/storage",
					path, imported)
			}
		}
	}
}

// The health surface depends on the BEHAVIOUR it needs, not on the type that provides it.
// Letting internal/health reach into internal/storage would put query construction one import
// away from a handler.
// The package is named rather than discovered, so a rename that did not reach this line would
// leave the gate looking for a package nobody has and reporting nothing wrong. It is asserted
// to have been found for the same reason the other gates refuse to read an empty list.
func TestHealthDoesNotImportStorage(t *testing.T) {
	t.Parallel()

	const surface = "internal/health"

	found := false
	for _, loaded := range loadPackages(t) {
		if internalPackagePath(loaded.PkgPath) != surface {
			continue
		}
		found = true
		for imported := range loaded.Imports {
			if imported == modulePath+"/internal/storage" {
				t.Error(surface + " must not import internal/storage; " +
					"it depends on a readiness function, not on the storage type")
			}
		}
	}
	if !found {
		t.Fatalf("%s was not found; the gate would pass vacuously", surface)
	}
}

// internal/tenancy is vocabulary. It performs no I/O, so it must not depend on anything in
// this module — that is what lets every other package use it without an import cycle.
func TestTenancyDependsOnNothingInternal(t *testing.T) {
	t.Parallel()

	for _, loaded := range loadPackages(t) {
		if internalPackagePath(loaded.PkgPath) != "internal/tenancy" {
			continue
		}
		for imported := range loaded.Imports {
			if strings.HasPrefix(imported, modulePath+"/internal/") {
				t.Errorf("internal/tenancy must not import %s; it is vocabulary, not machinery",
					imported)
			}
		}
	}
}

// Every exported function in internal/storage that reaches a tenant's data must take the
// organization explicitly. An ambient organization is how one tenant is served another
// tenant's rows, and the property is invisible in review.
func TestExportedStorageFunctionsTakeAnOrganization(t *testing.T) {
	t.Parallel()

	// Functions whose contract is deliberately placement-wide rather than tenant-scoped.
	// Each is listed with the reason it is safe, so adding to this list is a decision.
	placementWide := map[string]string{
		"OpenPlacements": "constructs pools; performs no tenant read",
		"Migrate":        "applies schema to every placement; touches no tenant row",
		"Ping":           "reachability of every placement; reads no data",
		"Close":          "releases pools",
		"MigrationCount": "reports how many migrations the binary carries",
		// The one read that DISCOVERS a tenant instead of being given one, and the reason it
		// has to is recorded in ADR-003's second amendment. An inbound delivery names its
		// Connection and nothing else, because a path is chosen by the caller and a caller who
		// could name a tenant could try every tenant — so there is no organization in the
		// request to resolve a placement from. Each placement is asked for the identifier and
		// the row that is found is itself the authority for the organization and the
		// environment. It is safe for the same reason it exists: nothing the caller sent
		// contributes to the tenant the answer belongs to.
		"ConnectionByID": "resolves a tenant FROM an opaque connection identifier; " +
			"the row found is the authority, and no caller-supplied value selects it",
		// The investigator's worker asks which tenants have a claimable round. It cannot take an
		// organization because finding out which organizations there are work for IS the question,
		// and it reads no tenant data — only which tenants have work. Every claim it leads to is
		// tenant-scoped, and each one takes the organization this read discovered.
		"InvestigationsAwaitingWork": "discovers which tenants have a claimable investigation " +
			"round; reads no tenant data, and every claim it leads to is tenant-scoped",
		// The three credential lookups, and they are the same case ConnectionByID is. A session
		// cookie, an API token and an OAuth state parameter name no tenant — a caller who could
		// name one could try every one — so there is nothing in the request to resolve a
		// placement from. Each placement is asked for the DIGEST in a fixed order and the row
		// that is found is itself the authority for the organization. Nothing the caller sent
		// contributes to the tenant the answer belongs to, which is why they are safe and why
		// they have to exist.
		"SessionByToken": "resolves a tenant FROM an opaque session digest; the row found is " +
			"the authority, and no caller-supplied value selects it",
		"BearerPrincipal": "resolves a tenant FROM an API token digest; the row found carries " +
			"the organization and the role, and no caller-supplied value selects it",
		"RedeemSignIn": "consumes an authorization state that names no tenant; the flow row " +
			"found is the authority for the organization the sign-in belongs to",
	}

	for _, file := range parseProductionFiles(t, filepath.Join("..", "storage")) {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() {
				continue
			}
			// Tenant data is reached only through the placement handle, so a method on any
			// other receiver — a refusal reason rendering itself, say — cannot reach a row
			// and has no organization to take. Plain functions are still checked, because a
			// new one could take a pool directly.
			if receiver := receiverType(function); receiver != "" && receiver != "Placements" {
				continue
			}
			if _, exempt := placementWide[function.Name.Name]; exempt {
				continue
			}
			if !takesOrganization(function) {
				t.Errorf("storage.%s is exported and tenant-scoped but takes no "+
					"tenancy.Organization; add the parameter or record it as "+
					"placement-wide with a reason", function.Name.Name)
			}
		}
	}
}

// parseProductionFiles parses the non-test Go files in one directory. go/parser.ParseDir is
// deprecated, and go/packages is the recommended replacement but overkill for a
// single-directory syntax walk that needs no type information.
func parseProductionFiles(t *testing.T, directory string) []*ast.File {
	t.Helper()

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("reading %s: %v", directory, err)
	}

	fileSet := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		t.Fatalf("%s contains no production Go files; the gate would pass vacuously", directory)
	}
	return files
}

// receiverType reports the bare type name a method is declared on, or "" for a plain
// function. Pointer receivers report the pointed-to name.
func receiverType(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return ""
	}
	return strings.TrimPrefix(typeExpression(function.Recv.List[0].Type), "*")
}

func takesOrganization(function *ast.FuncDecl) bool {
	if function.Type.Params == nil {
		return false
	}
	for _, parameter := range function.Type.Params.List {
		if strings.Contains(typeExpression(parameter.Type), "tenancy.Organization") {
			return true
		}
	}
	return false
}

func typeExpression(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.SelectorExpr:
		return typeExpression(typed.X) + "." + typed.Sel.Name
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return typeExpression(typed.X)
	case *ast.ArrayType:
		return typeExpression(typed.Elt)
	default:
		return ""
	}
}

// A secret must never be carried in an environment value; configuration names a FILE and
// reads it. A new environment variable whose name suggests a secret is a review failure
// worth catching mechanically.
func TestNoEnvironmentVariableNamesASecret(t *testing.T) {
	t.Parallel()

	forbidden := []string{"PASSWORD", "SECRET", "TOKEN", "APIKEY", "API_KEY", "CREDENTIAL"}

	// Variables that name a credential's SCOPE rather than carrying one. Each is listed with
	// the reason it is safe, so adding to this list is a decision rather than a way around the
	// gate — and the value each holds is one an operator would put in a deployment manifest
	// without thinking twice, which is the test.
	namesTheScope := map[string]string{
		"OC_OPERATOR_TOKEN_ORGANIZATION": "the one tenant the bootstrap credential reaches; " +
			"an organization identifier, not a secret",
		"OC_OPERATOR_TOKEN_ROLE": "the one role it holds there; a role name this build compiles",
	}

	for _, file := range parseProductionFiles(t, filepath.Join("..", "config")) {
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil || !strings.HasPrefix(value, "OC_") {
				return true
			}
			if _, scoped := namesTheScope[value]; scoped {
				return true
			}
			for _, word := range forbidden {
				if strings.Contains(strings.ToUpper(value), word) &&
					!strings.HasSuffix(strings.ToUpper(value), "_FILE") {
					t.Errorf("%s names a secret; configuration must reference a file path instead",
						value)
				}
			}
			return true
		})
	}
}

// The investigation capability owns the model boundary and must never learn that a provider
// exists. It depends on one interface it declares itself; internal/reasoning satisfies that
// interface, and the dependency runs one way.
//
// A vendor type reaching the domain is the failure this gate exists to catch. It would not look
// like much in review — one import, one field — and it would make every later provider change a
// domain change, which is the whole reason the boundary is shaped the way it is.
func TestInvestigationDependsOnNoProvider(t *testing.T) {
	t.Parallel()

	const surface = "internal/investigation"

	found := false
	for _, loaded := range loadPackages(t) {
		if internalPackagePath(loaded.PkgPath) != surface {
			continue
		}
		found = true
		for imported := range loaded.Imports {
			if strings.HasPrefix(imported, modulePath+"/internal/reasoning") {
				t.Errorf("%s must not import %s; the domain declares the boundary and "+
					"infrastructure satisfies it, never the reverse", surface, imported)
			}
			for _, vendor := range vendorModules {
				if strings.Contains(imported, vendor) {
					t.Errorf("%s must not import %s; a vendor named in the domain makes every "+
						"provider change a domain change", surface, imported)
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s was not found; the gate would pass vacuously", surface)
	}
}

// The shared reasoning orchestration must not import an adapter either. It talks to vendors
// through the contract it declares, and a package that reached for one adapter would be a package
// the second adapter has to be bolted onto rather than dropped beside.
func TestReasoningOrchestrationDependsOnNoAdapter(t *testing.T) {
	t.Parallel()

	const surface = "internal/reasoning"

	found := false
	for _, loaded := range loadPackages(t) {
		if internalPackagePath(loaded.PkgPath) != surface {
			continue
		}
		found = true
		for imported := range loaded.Imports {
			if strings.HasPrefix(imported, modulePath+"/internal/reasoning/") &&
				!strings.HasSuffix(imported, "/providers") {
				t.Errorf("%s must not import the adapter %s; it knows the contract and nothing "+
					"about who implements it", surface, imported)
			}
			for _, vendor := range vendorModules {
				if strings.Contains(imported, vendor) {
					t.Errorf("%s must not import %s; a vendor's types stop at its adapter",
						surface, imported)
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s was not found; the gate would pass vacuously", surface)
	}
}

// vendorModules are the third-party model clients this build may hold. Each one is permitted in
// exactly one adapter package and nowhere else.
var vendorModules = []string{"anthropic-sdk-go", "openai", "generative-ai-go", "openrouter"}
