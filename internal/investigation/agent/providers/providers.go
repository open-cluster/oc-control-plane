package providers

import (
	"fmt"
	"net/http"
	"sort"

	reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/anthropic"
	"github.com/open-cluster/oc-control-plane/internal/investigation/agent/zai"
)

type Options struct {
	HTTPClient *http.Client
}

func Open(
	deployment reasoning.Deployment, options Options,
) (reasoning.Model, error) {
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

func Names() []string {
	names := []string{anthropic.Name, zai.Name}
	sort.Strings(names)
	return names
}
