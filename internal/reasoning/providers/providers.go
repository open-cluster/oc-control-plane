// Package providers resolves a configured deployment to the adapter that serves it.
//
// It exists so that the mapping from a provider name to an implementation lives in exactly one
// place, and so that the orchestration cannot reach an adapter: internal/reasoning knows nothing
// about who implements its contract, and adding a vendor means a case here and a directory beside
// the other two. Nothing else in the program imports an adapter package directly.
package providers

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
	"github.com/open-cluster/oc-control-plane/internal/reasoning/anthropic"
	"github.com/open-cluster/oc-control-plane/internal/reasoning/zai"
)

// Options is what a caller may put in place of the real thing, passed through to whichever adapter
// serves the deployment. There is one entry and it is the HTTP round-tripper.
type Options struct {
	HTTPClient *http.Client
}

// Open builds the adapter for one deployment.
//
// A provider nobody implements is refused by name, with the ones that exist listed beside it. The
// alternative — a nil provider that fails on the first round — moves the error hours away from the
// person who typed the name.
func Open(
	deployment reasoning.Deployment, options Options,
) (reasoning.Provider, error) {
	switch deployment.Provider {
	case anthropic.Name:
		return anthropic.New(deployment, anthropic.Options{HTTPClient: options.HTTPClient})
	case zai.Name:
		return zai.New(deployment, zai.Options{HTTPClient: options.HTTPClient})
	default:
		return nil, fmt.Errorf(
			"%q is not a model provider this build serves; it serves %v",
			deployment.Provider, Names())
	}
}

// Names lists every provider this build can serve, so a refusal can say what the alternatives are
// rather than only what was wrong.
func Names() []string {
	names := []string{anthropic.Name, zai.Name}
	sort.Strings(names)
	return names
}
