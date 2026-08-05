// Package connection owns what a Connection is: one configured instance of an Integration,
// inside one Environment, and the sole authority for the Environment of everything that
// arrives through it.
//
// The distinction this package exists to hold is between the Integration and the Connection.
// An Integration is the KIND of system OpenCluster knows how to speak to — Alertmanager,
// Kubernetes — and is a closed vocabulary compiled into the binary. A Connection is one
// customer's configured instance of one: "Production Alertmanager", "EU Zabbix". A customer
// running two Alertmanager installations creates two Connections against one Integration and
// one adapter, and adding the second is configuration rather than code.
//
// Collapsing the two is the mistake the alert_source table made, and it is not cosmetic: a
// model in which the kind and the instance are one column can give that customer only one
// record, one secret, and one Environment.
package connection

import (
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Integration names a kind of external system. The value is what a connection row stores, so
// it is a readable string rather than an integer: a row read during an incident should say
// `alertmanager`.
type Integration string

const (
	// Alertmanager is Prometheus Alertmanager, reached inbound: it delivers alerts to a URL a
	// customer configures, and OpenCluster never calls it.
	Alertmanager Integration = "alertmanager"
	// Kubernetes is a cluster, reached outbound through a Relay by bounded capability reads.
	Kubernetes Integration = "kubernetes"
)

// Which roles each Integration can serve is a property of its DEFINITION, in registry.go, and
// is read from there rather than kept in a second map here. It used to be a map of its own, and
// two lists of the same fact is one list that goes stale — the one nobody edits is the one a
// customer is refused by.
//
// The rule it enforces has not changed: Alertmanager pushes and cannot be read from, a
// Kubernetes cluster is read from and pushes nothing, and a Connection declaring a role its
// Integration does not offer is a configuration that could never work. It is refused at creation
// by the product's own knowledge of itself rather than by a customer discovering it later.

// Known reports whether this build has an Integration by that name. It says nothing about
// whether one can be CONFIGURED — a planned provider is known and is not configurable — and
// callers deciding that ask Configurable instead.
func Known(integration Integration) bool {
	_, ok := Lookup(integration)
	return ok
}

// Offers reports whether an Integration can serve every role asked of it.
func Offers(integration Integration, role storage.ConnectionRole) bool {
	definition, ok := Lookup(integration)
	return ok && definition.Offers(role)
}

// Integrations lists what this build names, in the order Definitions renders them, so an
// operator can be told what is available rather than having to guess a string.
func Integrations() []Integration {
	listed := Definitions()
	names := make([]Integration, 0, len(listed))
	for _, definition := range listed {
		names = append(names, Integration(definition.Slug))
	}
	return names
}
