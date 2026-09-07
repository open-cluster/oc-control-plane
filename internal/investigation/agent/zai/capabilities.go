package zai

import reasoning "github.com/open-cluster/oc-control-plane/internal/investigation/agent"

var modelCapabilities = map[string]reasoning.ModelCapabilities{
	"glm-4.7": {ContextWindowTokens: 204_800, MaxOutputTokens: 131_072},
}

// ResolveDeployment applies the limits published for an exact Z.AI model identifier.
func ResolveDeployment(deployment reasoning.Deployment) (reasoning.Deployment, error) {
	capabilities, known := modelCapabilities[deployment.Model]
	if !known {
		return reasoning.ResolveModelCapabilities(deployment, nil)
	}
	return reasoning.ResolveModelCapabilities(deployment, &capabilities)
}
