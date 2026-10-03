package gates_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

const relayModule = "github.com/open-cluster/oc-relay"

const relayProtocolModule = relayModule + "/gen/go"

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

func TestNoKubernetesLibraryIsRequired(t *testing.T) {
	t.Parallel()

	for _, required := range requiredModules(t) {
		if strings.HasPrefix(required, "k8s.io/") {
			t.Errorf("go.mod requires %s; cluster access belongs to the Relay, and the "+
				"control plane never holds a Kubernetes client", required)
		}
	}
}

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

func requiredModules(t *testing.T) []string {
	t.Helper()

	path := filepath.Join(moduleRoot, "go.mod")
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
