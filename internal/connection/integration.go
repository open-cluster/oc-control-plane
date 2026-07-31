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
	"sort"

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

// offered is which roles each Integration can serve, and it is the reason the vocabulary lives
// in code rather than in a column. Alertmanager pushes and cannot be read from; a Kubernetes
// cluster is read from and pushes nothing. A Connection declaring a role its Integration does
// not offer is a configuration that could never work, and it is refused at creation by the
// product's own knowledge of itself rather than by a customer discovering it later.
var offered = map[Integration]storage.ConnectionRole{
	Alertmanager: storage.RoleTrigger,
	Kubernetes:   storage.RoleEvidence,
}

// Known reports whether this build has an Integration by that name.
func Known(integration Integration) bool {
	_, ok := offered[integration]
	return ok
}

// Offers reports whether an Integration can serve every role asked of it.
func Offers(integration Integration, role storage.ConnectionRole) bool {
	available, ok := offered[integration]
	return ok && available.Includes(role)
}

// Integrations lists what this build can be configured against, in a stable order, so an
// operator can be told what is available rather than having to guess a string.
func Integrations() []Integration {
	names := make([]Integration, 0, len(offered))
	for integration := range offered {
		names = append(names, integration)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}
