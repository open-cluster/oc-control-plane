package gates_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"golang.org/x/tools/go/packages"
)

const modulePath = "github.com/open-cluster/oc-control-plane"

const moduleRoot = "../.."

func TestBaselineUsesChangesVocabulary(t *testing.T) {
	t.Parallel()

	path := filepath.Join(moduleRoot, "internal", "store", "postgres", "migrations", "0001_schema.sql")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	schema := string(content)
	for _, table := range []string{"change_event", "change_scope"} {
		if !strings.Contains(schema, "CREATE TABLE "+table+" (") {
			t.Errorf("baseline does not create %s", table)
		}
	}
	for _, retired := range []string{"change_ledger", "change_ledger_scope"} {
		if strings.Contains(schema, retired) {
			t.Errorf("baseline still contains retired name %q", retired)
		}
	}
}

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

func internalPackagePath(packagePath string) string {
	if !strings.HasPrefix(packagePath, modulePath) {
		return ""
	}
	trimmed := strings.TrimPrefix(packagePath, modulePath)
	return strings.TrimPrefix(trimmed, "/")
}

func TestOnlyStorageImportsTheDatabaseDriver(t *testing.T) {
	t.Parallel()

	const driver = "github.com/jackc/pgx/v5"
	allowed := map[string]bool{
		"internal/store/postgres": true,
		"test/architecture":       true,
	}

	for _, loaded := range loadPackages(t) {
		path := internalPackagePath(loaded.PkgPath)
		if path == "" || allowed[path] {
			continue
		}
		for imported := range loaded.Imports {
			if imported == driver || strings.HasPrefix(imported, driver+"/") {
				t.Errorf("%s imports %s; database access belongs in internal/store/postgres",
					path, imported)
			}
		}
	}
}

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
			if imported == modulePath+"/internal/store/postgres" {
				t.Error(surface + " must not import internal/store/postgres; " +
					"it depends on a readiness function, not on the storage type")
			}
		}
	}
	if !found {
		t.Fatalf("%s was not found; the gate would pass vacuously", surface)
	}
}

func TestExportedStorageFunctionsTakeAnOrganization(t *testing.T) {
	t.Parallel()

	databaseWide := map[string]string{
		"BootstrapLocalUser": "deployment bootstrap atomically creates the first Organization, User, Membership, password, and session",
		"OpenDatabase":       "constructs the deployment pool; performs no tenant read",
		"Migrate":            "applies schema to the deployment database; touches no tenant row",
		"Ping":               "reports deployment database reachability; reads no tenant data",
		"Close":              "releases the deployment pool",
		"MigrationCount":     "reports how many migrations the binary carries",
		"IntegrationByID": "resolves a tenant FROM an opaque integration identifier; " +
			"the row found is the authority, and no caller-supplied value selects it",
		"IntegrationByInstallation": "resolves a tenant FROM a deployment-unique vendor " +
			"installation key; the row found is the authority, and no caller-supplied " +
			"value selects which tenant is searched",
		"ClaimSlackReplies": "discovers which tenants owe a Slack answer; each row " +
			"carries its own organization and every write it leads to is tenant-scoped",
		"DeclaredRetentions": "discovers which tenants declared an audit retention schedule; " +
			"reads no tenant data, and every prune it leads to is tenant-scoped",
		"SessionByToken":        "resolves a global User and current Membership from an opaque session digest",
		"LocalPasswordHash":     "reads only the authenticated User's local verifier",
		"ChangeLocalPassword":   "changes only the authenticated User's reauthenticated global credential",
		"RecoverLocalPassword":  "deployment operator recovery of an existing local User; no tenant authority applies",
		"DeleteCurrentSession":  "deletes only the authenticated User's current global session",
		"PruneSessions":         "bounded deployment housekeeping of unusable User-owned sessions; no Organization authority applies",
		"StartDeploymentSignIn": "stores an opaque deployment OIDC transaction with no tenant authority",
		"OIDCIdentity":          "resolves a User and current Membership from issuer and subject",
		"LocalIdentityByEmail":  "resolves a local User and current Membership from a deployment-unique email",
		"RehashLocalPassword":   "updates only the resolved local User while a current Membership exists",
		"BearerPrincipal": "resolves a tenant FROM an API token digest; the row found carries " +
			"the organization and the role, and no caller-supplied value selects it",
		"RedeemSignIn": "consumes an authorization state that names no tenant; the flow row " +
			"found is the authority for the organization the sign-in belongs to",
		"RedeemConnectFlow": "consumes an installation state that names no tenant; the flow " +
			"row found is the authority for the organization the integration binds to, and " +
			"an organization named in the provider's callback is never read",
		"PruneChangesBefore": "age-bounded delete across the database; reads no tenant " +
			"data and takes no caller-supplied identifier",
		"ClaimInvestigation": "discovers which tenant has work waiting; takes no " +
			"caller-supplied identifier, and the claimed row is the authority for the " +
			"organization it belongs to",
		"DrainQueuedConversation": "discovers durable queued Conversation work across tenants; " +
			"the selected row is the authority for the Organization used by the capacity lock and turn",
		"ClaimSlackMessageWork": "discovers ready or expired Slack Message work across tenants; each " +
			"claimed row carries the authoritative Organization used by every fenced transition",
		"RecoverStale": "expiry-bounded recovery across the database; reads no tenant " +
			"data and takes no caller-supplied identifier",
		"RedeemDeploymentSignIn": "consumes an opaque OIDC transaction that carries no tenant authority",
	}

	for _, file := range parseProductionFiles(t,
		filepath.Join(moduleRoot, "internal", "store", "postgres")) {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() {
				continue
			}
			if receiver := receiverType(function); receiver != "" && receiver != "Database" {
				continue
			}
			if _, exempt := databaseWide[function.Name.Name]; exempt {
				continue
			}
			if !takesOrganization(function) {
				t.Errorf("storage.%s is exported and tenant-scoped but takes no "+
					"explicitly named Organization UUID; add the parameter or record it as "+
					"database-wide with a reason", function.Name.Name)
			}
		}
	}
}

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
		if typeExpression(parameter.Type) != "uuid.UUID" {
			continue
		}
		for _, name := range parameter.Names {
			if name.Name == "organization" || name.Name == "org" {
				return true
			}
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

func TestSecretEnvironmentInputsHaveFileAlternatives(t *testing.T) {
	keys := make(map[string]bool, len(config.SupportedEnvironmentKeys))
	for _, key := range config.SupportedEnvironmentKeys {
		keys[key] = true
	}
	for key := range keys {
		for _, word := range []string{"PASSWORD", "SECRET", "TOKEN", "APIKEY", "API_KEY", "CREDENTIAL", "ENCRYPTION_KEY", "PRIVATE_KEY", "DATABASE_DSN"} {
			if strings.Contains(key, word) && !strings.HasSuffix(key, "_FILE") && !keys[key+"_FILE"] {
				t.Errorf("%s has no optional file input", key)
			}
		}
	}
}

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
			if imported == modulePath+"/internal/store/postgres" {
				t.Errorf("%s must not import persistence; the capability owns its vocabulary "+
					"and persistence depends on it", surface)
			}
		}
	}
	if !found {
		t.Fatalf("%s was not found; the gate would pass vacuously", surface)
	}
}

func TestAlertAdaptersDoNotImportPersistence(t *testing.T) {
	t.Parallel()

	providers := map[string]bool{
		"internal/integrations/alertmanager":   false,
		"internal/integrations/genericwebhook": false,
	}
	for _, loaded := range loadPackages(t) {
		provider := internalPackagePath(loaded.PkgPath)
		if _, inspected := providers[provider]; !inspected {
			continue
		}
		providers[provider] = true
		if _, importsPersistence := loaded.Imports[modulePath+"/internal/store/postgres"]; importsPersistence {
			t.Errorf("%s must not import persistence; alert adapters return domain values", provider)
		}
	}
	for provider, found := range providers {
		if !found {
			t.Errorf("%s was not found; the gate would pass vacuously", provider)
		}
	}
}

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
		if loaded.PkgPath == modulePath+"/internal/app" {
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

func TestReasoningOrchestrationDependsOnNoAdapter(t *testing.T) {
	t.Parallel()

	const surface = "internal/investigation/agent"

	found := false
	for _, loaded := range loadPackages(t) {
		if internalPackagePath(loaded.PkgPath) != surface {
			continue
		}
		found = true
		for imported := range loaded.Imports {
			if strings.HasPrefix(imported, modulePath+"/internal/investigation/agent/") &&
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

func TestAgentRunOwnsTheOnlyOrchestrationLoop(t *testing.T) {
	t.Parallel()

	forbiddenTypes := map[string]bool{"loop": true, "session": true}
	forbiddenMethods := map[string]bool{"converse": true, "nextmove": true}
	files := parseProductionFiles(t,
		filepath.Join(moduleRoot, "internal", "investigation", "agent"))

	for _, file := range files {
		for _, declaration := range file.Decls {
			switch declared := declaration.(type) {
			case *ast.GenDecl:
				for _, specification := range declared.Specs {
					typeSpec, ok := specification.(*ast.TypeSpec)
					if ok && forbiddenTypes[strings.ToLower(typeSpec.Name.Name)] {
						t.Errorf("agent declares %s; Agent.Run must own the only orchestration loop",
							typeSpec.Name.Name)
					}
				}
			case *ast.FuncDecl:
				name := strings.ToLower(declared.Name.Name)
				receiver := strings.ToLower(receiverType(declared))
				if forbiddenMethods[name] || name == "next" && receiver == "session" {
					t.Errorf("agent declares %s.%s; Agent.Run must own turn advancement",
						receiverType(declared), declared.Name.Name)
				}
				if receiver == "runstate" {
					t.Errorf("agent declares behavior on runState; it must remain data-only")
				}
				if declared.Name.Name != "Run" && takesRunState(declared) &&
					resultCount(declared) > 1 {
					t.Errorf("agent declares controller-shaped helper %s over runState; "+
						"Agent.Run must own orchestration decisions", declared.Name.Name)
				}
			}
		}
	}
}

func TestInvestigationRuntimeHasNoRetiredBillingOrPolicyMachinery(t *testing.T) {
	t.Parallel()

	paths := []string{
		filepath.Join(moduleRoot, "internal", "investigation"),
		filepath.Join(moduleRoot, "internal", "app"),
		filepath.Join(moduleRoot, "internal", "store", "postgres"),
		filepath.Join(moduleRoot, "api", "openapi.yaml"),
		filepath.Join(moduleRoot, "docs", "api", "openapi.yaml"),
		filepath.Join(moduleRoot, "test", "eval"),
	}
	forbidden := []string{
		"type Spend struct", "MicroCents", "type Tariff struct", "type Rate struct",
		"type Consent struct", "type DeploymentResolver interface",
		"type StaticDeploymentResolver", "func ContextBudget", "contextWindows",
		"func AgentRevision", "spend_micro_cents", "oc.reasoning.spend", "- spend",
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			assertNoRetiredInvestigationTerms(t, path, forbidden)
			continue
		}
		err = filepath.WalkDir(path, func(candidate string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || strings.HasSuffix(candidate, "_test.go") ||
				(!strings.HasSuffix(candidate, ".go") &&
					!strings.HasSuffix(candidate, ".json") && !strings.HasSuffix(candidate, ".sql")) {
				return nil
			}
			assertNoRetiredInvestigationTerms(t, candidate, forbidden)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func assertNoRetiredInvestigationTerms(t *testing.T, path string, forbidden []string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range forbidden {
		if strings.Contains(string(body), term) {
			t.Errorf("%s still contains retired Investigation term %q", path, term)
		}
	}
}

func takesRunState(function *ast.FuncDecl) bool {
	if function.Type.Params == nil {
		return false
	}
	for _, parameter := range function.Type.Params.List {
		if strings.EqualFold(typeExpression(parameter.Type), "runState") {
			return true
		}
	}
	return false
}

func resultCount(function *ast.FuncDecl) int {
	if function.Type.Results == nil {
		return 0
	}
	total := 0
	for _, result := range function.Type.Results.List {
		if len(result.Names) == 0 {
			total++
		} else {
			total += len(result.Names)
		}
	}
	return total
}

func TestAgentDoesNotAliasInvestigationTypes(t *testing.T) {
	t.Parallel()

	files := parseProductionFiles(t,
		filepath.Join(moduleRoot, "internal", "investigation", "agent"))
	for _, file := range files {
		for _, declaration := range file.Decls {
			declared, ok := declaration.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, specification := range declared.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok || !typeSpec.Assign.IsValid() {
					continue
				}
				selector, ok := typeSpec.Type.(*ast.SelectorExpr)
				owner, owned := selector.X.(*ast.Ident)
				if ok && owned && owner.Name == "investigation" {
					t.Errorf("agent aliases investigation.%s as %s; qualify the durable type at use sites",
						selector.Sel.Name, typeSpec.Name.Name)
				}
			}
		}
	}
}

var vendorModules = []string{"anthropic-sdk-go", "openai", "generative-ai-go", "openrouter"}
