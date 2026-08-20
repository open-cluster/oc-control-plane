// Package kubernetes is the Kubernetes provider: a cluster read through a Relay by bounded
// typed reads — what is running, what the cluster said about it, and what a container
// logged before it died.
//
// The typed capability contracts themselves live in internal/capability today, because the
// Relay dispatch path and its tests are organized around them; they move here when that
// path is next reshaped.
package kubernetes

import (
	"sort"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/capability"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// Definition is what this provider exports to the catalog. Metadata mirrors the seeded
// integration_type row; a test proves the two agree.
func Definition() integrations.Definition {
	return integrations.Definition{
		ID:   integrations.TypeKubernetes,
		Key:  "kubernetes",
		Name: "Kubernetes",
		Description: "Give investigations a read-only inventory of Kubernetes " +
			"workloads through an outbound Relay.",
		Logo:     "kubernetes",
		Category: integrations.CategoryInfrastructure,
		DocumentationURL: "https://kubernetes.io/docs/reference/access-authn-authz/" +
			"rbac/#role-and-clusterrole",
		Capabilities: []string{
			capability.KubernetesWorkloadRuntime,
			capability.KubernetesNamespaceEvents,
			capability.KubernetesContainerLogs,
		},
		// There is deliberately no "cluster name" field. The Integration is already named
		// by the operator, and the cluster itself is pinned by the fingerprint the Relay
		// attested at enrolment — a third name for the same thing is a field that can
		// disagree with two others.
		Config: []integrations.Field{
			{
				Name: "namespaceAllowList",
				Title: "Namespaces this integration may read (comma separated; " +
					"empty means every namespace the Relay's service account can reach)",
				Description: "A narrowing on top of the Relay's own permissions, never a " +
					"widening: this cannot grant access the service account does not already have.",
				Type: integrations.FieldString,
			},
		},
		// Relay only. A cluster's API server is usually private, which is the whole reason
		// the Relay exists.
		RequiresRelay:    true,
		ReceivesWebhooks: false,
		Verify:           verify,
	}
}

// verify judges this integration from the bound Relay's own state: whether it is
// connected right now, and whether it advertised each capability this type declares. A
// capability the Relay did not advertise is the cluster's own configuration answering, not
// this platform's.
func verify(input integrations.VerifyInput) integrations.Verification {
	if !input.Relay.Bound {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "no relay serves this integration; bind one and verify again",
		}
	}
	if !input.Relay.Connected {
		return integrations.Verification{
			Status: integrations.StatusFailed,
			Note:   "the relay serving this integration is not connected",
		}
	}

	advertised := make(map[string]bool, len(input.Relay.Capabilities))
	for _, name := range input.Relay.Capabilities {
		advertised[name] = true
	}
	var missing []string
	for _, needed := range Definition().Capabilities {
		if !advertised[needed] {
			missing = append(missing, needed)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return integrations.Verification{
			Status: integrations.StatusDegraded,
			Note: "the relay is connected and does not advertise: " +
				strings.Join(missing, ", "),
		}
	}
	return integrations.Verification{
		Status: integrations.StatusActive,
		Note:   "the relay is connected and advertises every capability this type declares",
	}
}
