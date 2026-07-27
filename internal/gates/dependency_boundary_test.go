package gates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// relayModule is the Relay's own module. It reads Kubernetes clusters, so it depends on
// client-go and the rest of the Kubernetes libraries.
const relayModule = "github.com/open-cluster/oc-relay"

// relayProtocolModule is the generated protocol contract, published as a module of its own
// so that speaking the protocol does not require the machinery that implements it. It needs
// only gRPC and protobuf.
const relayProtocolModule = relayModule + "/gen/go"

// The control plane speaks the Relay's protocol and never runs anything against a cluster.
// Reaching for the generated types by requiring the Relay module would pull its entire
// Kubernetes dependency graph into this module's requirements, vulnerability report and
// licence inventory — for types that need two libraries. The contract module exists to make
// that unnecessary, and it is the only part of the Relay this module may ever require.
func TestOnlyTheRelaysProtocolContractMayBeRequired(t *testing.T) {
	t.Parallel()

	for _, required := range requiredModules(t) {
		if required == relayProtocolModule || strings.HasPrefix(required, relayProtocolModule+"/") {
			continue
		}
		if required == relayModule || strings.HasPrefix(required, relayModule+"/") {
			t.Errorf("go.mod requires %s; the control plane may depend on %s and nothing "+
				"else from the Relay, so that its Kubernetes dependencies stay out of this "+
				"module", required, relayProtocolModule)
		}
	}
}

// Reading a customer's cluster is the Relay's job, performed inside the customer's own
// infrastructure over a connection the customer opens outward. A Kubernetes client here
// would mean the control plane reaching into customer infrastructure directly, which is the
// property the whole execution design exists to avoid. It would also be invisible in review
// as an ordinary-looking import.
func TestNoKubernetesLibraryIsRequired(t *testing.T) {
	t.Parallel()

	for _, required := range requiredModules(t) {
		if strings.HasPrefix(required, "k8s.io/") {
			t.Errorf("go.mod requires %s; cluster access belongs to the Relay, and the "+
				"control plane never holds a Kubernetes client", required)
		}
	}
}

// TestNoPackageImportsKubernetes catches the source-level version of the same mistake one
// step earlier than the module requirements do, since an import lands before anyone runs
// `go mod tidy`.
func TestNoPackageImportsKubernetes(t *testing.T) {
	t.Parallel()

	for _, loaded := range loadPackages(t) {
		for imported := range loaded.Imports {
			if strings.HasPrefix(imported, "k8s.io/") {
				t.Errorf("%s imports %s; cluster access belongs to the Relay",
					internalPackagePath(loaded.PkgPath), imported)
			}
		}
	}
}

// requiredModules reports every module path this module requires, direct and indirect. It
// fails rather than returning nothing if the file cannot be parsed or lists no
// requirements, so a gate built on it cannot pass by reading an empty list.
func requiredModules(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "..", "go.mod")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	parsed, err := modfile.Parse(path, content, nil)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if len(parsed.Require) == 0 {
		t.Fatalf("%s lists no requirements; the gate would pass vacuously", path)
	}

	required := make([]string, 0, len(parsed.Require))
	for _, requirement := range parsed.Require {
		required = append(required, requirement.Mod.Path)
	}
	return required
}
