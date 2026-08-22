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
		// The one read that DISCOVERS a tenant instead of being given one. An inbound
		// delivery names its Integration and nothing else, because a path is chosen by the
		// caller and a caller who could name a tenant could try every tenant — so there is
		// no organization in the request to resolve a placement from. Each placement is
		// asked for the identifier and the row that is found is itself the authority for
		// the organization. It is safe for the same reason it exists: nothing the caller
		// sent contributes to the tenant the answer belongs to.
		"IntegrationByID": "resolves a tenant FROM an opaque integration identifier; " +
			"the row found is the authority, and no caller-supplied value selects it",
		// The same case, one hop earlier. An inbound event from a chat vendor names a
		// workspace and not a tenant, so there is nothing in the request to resolve a
		// placement from. The installation key is unique across the whole deployment —
		// deliberately not per organization — so it resolves to at most one row, and that
		// row is the authority for the organization. A vendor identifier from one tenant
		// cannot reach another's records because the lookup never starts from a vendor
		// identifier alone; it starts from the deployment-unique key.
		"IntegrationByInstallation": "resolves a tenant FROM a deployment-unique vendor " +
			"installation key; the row found is the authority, and no caller-supplied " +
			"value selects which tenant is searched",
		// The outbound delivery sweep, and the same case DeclaredRetentions is: finding
		// out which tenants owe an answer IS the question, so there is no organization to
		// be given. Every row it claims carries its own, and every write the worker then
		// makes is scoped by the organization that row named.
		"ClaimSlackDeliveries": "discovers which tenants owe a Slack answer; each row " +
			"carries its own organization and every write it leads to is tenant-scoped",
		// The retention pruner asks which tenants declared a schedule for their own record.
		// It cannot take an organization because finding out which organizations there are
		// IS the question. It reads no tenant data — only which tenants declared a number —
		// and the delete it leads to takes the organization this read discovered.
		"DeclaredRetentions": "discovers which tenants declared an audit retention schedule; " +
			"reads no tenant data, and every prune it leads to is tenant-scoped",
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
		"RedeemConnectFlow": "consumes an installation state that names no tenant; the flow " +
			"row found is the authority for the organization the integration binds to, and " +
			"an organization named in the provider's callback is never read",
		// The change ledger's pruner deletes by AGE across every placement, bounded per
		// statement. It reads no tenant data and takes no caller-supplied value at all — a
		// horizon and a batch size are the whole request — so there is no tenant in the
		// question to resolve a placement from, and nothing selective enough to leak one.
		"PruneChangeLedgerBefore": "age-bounded delete across every placement; reads no tenant " +
			"data and takes no caller-supplied identifier",
		// The investigation claimer asks for WORK, not for a tenant's work. Which
		// organization has something waiting is the answer rather than the question, and a
		// worker that had to name one could only serve tenants somebody had listed for it.
		// It takes no caller-supplied identifier — a worker name and two numbers are the
		// whole request — and the row it claims is itself the authority for the
		// organization, which the caller is then told.
		"ClaimInvestigation": "discovers which tenant has work waiting; takes no " +
			"caller-supplied identifier, and the claimed row is the authority for the " +
			"organization it belongs to",
		// The lease sweeper recovers by EXPIRY across every placement, bounded per call. It
		// reads no tenant data and takes nothing selective — a reason and a batch size —
		// so there is no tenant in the question and nothing precise enough to leak one.
		"RecoverStale": "expiry-bounded recovery across every placement; reads no tenant " +
			"data and takes no caller-supplied identifier",
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

// The integrations core is the thin shared domain, and the composition root is the only
// place that knows every provider. The core importing one of its own provider packages
// would not look like much in review — one import, one field — and it would make every
// later provider change a domain change, which is the whole reason the tree is shaped the
// way it is.
func TestIntegrationsCoreImportsNoProvider(t *testing.T) {
	t.Parallel()

	const surface = "internal/integrations"

	found := false
	for _, loaded := range loadPackages(t) {
		if internalPackagePath(loaded.PkgPath) != surface {
			continue
		}
		found = true
		for imported := range loaded.Imports {
			if strings.HasPrefix(imported, modulePath+"/internal/integrations/") {
				t.Errorf("%s must not import the provider %s; providers import the core, and "+
					"only the composition root assembles them", surface, imported)
			}
			if imported == modulePath+"/internal/storage" {
				t.Errorf("%s must not import persistence; the capability owns its vocabulary "+
					"and persistence depends on it", surface)
			}
		}
	}
	if !found {
		t.Fatalf("%s was not found; the gate would pass vacuously", surface)
	}
}

// The composition root is the ONLY place that knows every provider. A second package
// assembling two of them would be a second catalog — the central-hub shape this tree was
// built to avoid — and it would not look like much in review: two imports in a file that
// already has twenty.
func TestOnlyTheCompositionRootAssemblesProviders(t *testing.T) {
	t.Parallel()

	assembled := false
	for _, loaded := range loadPackages(t) {
		providers := 0
		for imported := range loaded.Imports {
			if strings.HasPrefix(imported, modulePath+"/internal/integrations/") {
				providers++
			}
		}
		if providers < 2 {
			continue
		}
		if loaded.PkgPath == modulePath+"/cmd/controlplane" {
			assembled = true
			continue
		}
		t.Errorf("%s imports %d provider packages; only the composition root assembles the "+
			"catalog, and a second assembly point is a second catalog", loaded.PkgPath, providers)
	}
	if !assembled {
		t.Fatal("no package assembles the providers; the gate would pass vacuously")
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

// The SAML libraries are an ADAPTER's, and the adapter is internal/identity.
//
// It is the same rule the model vendors are held to and it exists for the same reason: a
// library named outside the one package that owns it makes every later change to that library
// a change in several places. It matters more here than usual, because these three are the
// XML signature machinery — the thing this product deliberately did not write — and a second
// package reaching for them would be a second place somebody could verify a signature slightly
// differently.
func TestOnlyIdentityNamesTheSAMLLibraries(t *testing.T) {
	t.Parallel()

	libraries := []string{
		"github.com/crewjam/saml",
		"github.com/russellhaering/goxmldsig",
		"github.com/mattermost/xml-roundtrip-validator",
		"github.com/beevik/etree",
	}
	const adapter = "internal/identity"

	found := false
	for _, loaded := range loadPackages(t) {
		path := internalPackagePath(loaded.PkgPath)
		if path == "" {
			continue
		}
		if path == adapter {
			found = true
			continue
		}
		for imported := range loaded.Imports {
			for _, library := range libraries {
				if strings.HasPrefix(imported, library) {
					t.Errorf("%s imports %s; the XML signature machinery belongs to %s, and a "+
						"second package holding it is a second place a signature could be "+
						"checked differently", path, imported, adapter)
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s was not found; the gate would pass vacuously", adapter)
	}
}
