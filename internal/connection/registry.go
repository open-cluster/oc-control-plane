package connection

import (
	"sort"
	"strconv"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/capability"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// The registry itself: every Integration this product names, with everything a schema-driven
// setup flow needs to render it and everything an operator needs to decide about it.
//
// TWO OF THESE HAVE ADAPTERS. The other eleven are `planned`, appear in the catalog, and are
// refused at creation. That is the point rather than an embarrassment: a catalog that lists only
// what works cannot show a customer where the product is going, and a catalog that lists
// everything without saying which is which costs somebody an afternoon. Lifecycle is what lets
// the list be both complete and honest, and the gate in registry_test.go is what stops the
// distinction being a copy decision somebody can quietly reverse.
//
// The vocabulary is drawn from CONTEXT.md's own list — "Kubernetes, Prometheus, Loki,
// Alertmanager, PagerDuty, Grafana, Zabbix, Nomad, Proxmox, AWS, a generic webhook" — plus the
// two the architecture record names alongside them (Datadog and New Relic, in
// docs/architecture/customer-execution-plane.md and ADR-007). Nothing here is invented to pad a
// number: a provider this repository's own documents do not name is a provider nobody has
// decided to support, and putting it in the catalog would be the aspirational listing this
// design exists to prevent.

// definitionVersion is the shape version every record below carries. It is one number rather
// than one per entry because the records share a shape: a field added to Definition changes all
// of them at once, and a client detecting staleness is detecting exactly that.
const definitionVersion = 1

// minimumRelayVersionToday is the oldest Relay carrying the compiled capability set. It is a
// constant rather than a literal per entry so that raising the floor is one edit and cannot be
// half-applied.
const minimumRelayVersionToday = "0.1.0"

// centrally and viaRelay name the two locality lists, so an entry states where its work runs
// without repeating a slice literal thirteen times.
var (
	centrally = []storage.ExecutionLocality{storage.LocalityControlPlane}
	viaRelay  = []storage.ExecutionLocality{storage.LocalityRelay}
	// eitherWay is for a provider a customer may run publicly or privately. Where it reaches
	// from is the Connection's decision, not the Capability's, which is exactly what
	// CONTEXT.md's execution locality says.
	eitherWay = []storage.ExecutionLocality{
		storage.LocalityControlPlane, storage.LocalityRelay,
	}
)

// endpointField is the field almost every outbound provider needs, defined once so thirteen
// entries do not each describe a URL slightly differently.
func endpointField(description string) Field {
	return Field{
		Name:        "endpoint",
		Title:       "Endpoint",
		Description: description,
		Type:        FieldString,
		Format:      "uri",
		Required:    true,
	}
}

// definitions is the closed set, keyed by the value a connection row stores.
var definitions = map[string]Definition{
	// ---------------------------------------------------------------------------------------
	// The two with adapters.
	// ---------------------------------------------------------------------------------------

	"kubernetes": {
		Slug:              "kubernetes",
		DefinitionVersion: definitionVersion,
		Name:              "Kubernetes",
		Description: "A cluster, read through a Relay by bounded typed reads: what is running, " +
			"what the cluster said about it, and what a container logged before it died.",
		Category:     CategoryOrchestration,
		LogoAssetKey: "kubernetes",
		DocumentationURL: "https://kubernetes.io/docs/reference/access-authn-authz/" +
			"rbac/#role-and-clusterrole",
		AuthModes: []AuthMode{AuthServiceAccount},
		Roles:     storage.RoleEvidence,
		// Relay only. A cluster's API server is usually private, which is the whole reason the
		// Relay exists; offering a central locality here would offer a configuration that works
		// for the minority of customers whose control plane is on the public internet.
		ExecutionLocalities: viaRelay,
		// There is deliberately no "cluster name" field. The Connection is already named by the
		// operator, and the cluster itself is pinned by the fingerprint the Relay attested at
		// enrolment — a third name for the same thing is a field that can disagree with two
		// others.
		Configuration: []Field{
			{
				Name: "namespaceAllowList",
				Title: "Namespaces this connection may read (comma separated; " +
					"empty means every namespace the Relay's service account can reach)",
				Description: "A narrowing on top of the Relay's own permissions, never a " +
					"widening: this cannot grant access the service account does not already have.",
				Type: FieldString,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{
					Title: "Scope",
					Description: "What this Connection may read. The Relay's service account is " +
						"the ceiling; this narrows below it.",
					Fields: []string{"namespaceAllowList"},
				},
			},
			// The hard case this slot exists for. A Kubernetes Connection is authenticated by
			// the service account the Relay already runs as, so there is no credential to
			// submit and no form field that would collect one — what the operator needs is the
			// manifest to apply and a check that it took effect, which is a component rather
			// than a text box.
			ProviderStep: "kubernetes-service-account",
		},
		Validation: ValidationContract{
			Authenticates:      true,
			ProbesCapabilities: true,
			Note: "Each capability is attempted through the bound Relay. A capability reported " +
				"unauthorized is the cluster's RBAC answering, not this platform's.",
		},
		Capabilities: []string{
			capability.KubernetesWorkloadRuntime,
			capability.KubernetesNamespaceEvents,
			capability.KubernetesContainerLogs,
		},
		MinimumRelayVersion:       minimumRelayVersionToday,
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecycleGeneral,
	},

	"alertmanager": {
		Slug:              "alertmanager",
		DefinitionVersion: definitionVersion,
		Name:              "Prometheus Alertmanager",
		Description: "Delivers alerts to a URL you configure. OpenCluster never calls it, so " +
			"nothing needs to be reachable from here and no Relay is required.",
		Category: CategoryAlerting,
		// No approved mark exists for Alertmanager, so the neutral category icon is what gets
		// drawn. Sourcing one is a design decision with a manifest entry behind it, and
		// fabricating a key here would produce a broken image rather than a missing one.
		LogoAssetKey: "",
		DocumentationURL: "https://prometheus.io/docs/alerting/latest/configuration/" +
			"#webhook_config",
		AuthModes:           []AuthMode{AuthSharedSecret},
		Roles:               storage.RoleTrigger,
		ExecutionLocalities: centrally,
		// NOTHING TO CONFIGURE, and saying so is the honest definition. Alertmanager is reached
		// inbound: the platform mints the secret, the Connection carries its own operator-chosen
		// name, and its Environment is assigned at creation. A "source name" field here would be
		// a third place to write what the Connection is called, and two of the three would
		// eventually disagree.
		Configuration: nil,
		Presentation:  Presentation{},
		Validation: ValidationContract{
			// Nothing to authenticate against: this provider is reached inbound only, and the
			// credential is verified when a delivery arrives rather than when one is configured.
			Authenticates:      false,
			ProbesCapabilities: false,
			Note: "Validation checks that this Connection is configured to accept deliveries. " +
				"It cannot prove your Alertmanager can reach the endpoint — send a test event " +
				"from it, or use the test-event operation, to establish that.",
		},
		MinimumRelayVersion:       "",
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecycleGeneral,
	},

	// ---------------------------------------------------------------------------------------
	// Named, not implemented. Each carries the configuration its adapter will need, so the
	// definition is what the adapter is written against rather than something invented
	// afterwards — but none of them carries a CAPABILITY, because a capability listed against
	// a provider that cannot serve it is the aspirational catalog wearing a different hat.
	// ---------------------------------------------------------------------------------------

	"prometheus": {
		Slug:              "prometheus",
		DefinitionVersion: definitionVersion,
		Name:              "Prometheus",
		Description: "Metrics, queried on demand for the windows an investigation needs. " +
			"Reachable centrally where it is exposed, and through a Relay where it is not.",
		Category:            CategoryMetrics,
		DocumentationURL:    "https://prometheus.io/docs/prometheus/latest/querying/api/",
		AuthModes:           []AuthMode{AuthBearerToken, AuthBasic},
		Roles:               storage.RoleEvidence,
		ExecutionLocalities: eitherWay,
		Configuration: []Field{
			endpointField("The base URL of the Prometheus HTTP API, for example " +
				"https://prometheus.internal:9090."),
			{
				Name:        "credential",
				Title:       "Bearer token or password",
				Description: "Submitted once and never returned. Rotate rather than recover it.",
				Type:        FieldString,
				Secret:      true,
			},
			{
				Name:        "queryTimeoutSeconds",
				Title:       "Query timeout",
				Description: "How long one range query may take before it is abandoned.",
				Type:        FieldInteger,
				Default:     30,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Endpoint", Fields: []string{"endpoint"}},
				{Title: "Authentication", Fields: []string{"credential"}},
				{Title: "Limits", Fields: []string{"queryTimeoutSeconds"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Runs one trivial instant query. A successful validation proves reachability " +
				"and authentication, not that the series an investigation wants exist."},
		MinimumRelayVersion:       minimumRelayVersionToday,
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"loki": {
		Slug:                "loki",
		DefinitionVersion:   definitionVersion,
		Name:                "Grafana Loki",
		Description:         "Logs, queried for bounded windows around an incident.",
		Category:            CategoryLogs,
		DocumentationURL:    "https://grafana.com/docs/loki/latest/reference/loki-http-api/",
		AuthModes:           []AuthMode{AuthBearerToken, AuthBasic},
		Roles:               storage.RoleEvidence,
		ExecutionLocalities: eitherWay,
		Configuration: []Field{
			endpointField("The base URL of the Loki HTTP API."),
			{
				Name:        "tenantId",
				Title:       "Tenant",
				Description: "The X-Scope-OrgID a multi-tenant Loki expects. Leave empty if unused.",
				Type:        FieldString,
			},
			{
				Name:        "credential",
				Title:       "Bearer token or password",
				Description: "Submitted once and never returned.",
				Type:        FieldString,
				Secret:      true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Endpoint", Fields: []string{"endpoint", "tenantId"}},
				{Title: "Authentication", Fields: []string{"credential"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Runs one bounded query over a short window."},
		MinimumRelayVersion:       minimumRelayVersionToday,
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"grafana": {
		Slug:              "grafana",
		DefinitionVersion: definitionVersion,
		Name:              "Grafana",
		Description: "Grafana's own alerting delivers inbound, and its datasource proxy answers " +
			"bounded reads outbound. One Connection may do either or both.",
		Category:            CategoryMetrics,
		DocumentationURL:    "https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/",
		AuthModes:           []AuthMode{AuthSharedSecret, AuthBearerToken},
		Roles:               storage.RoleBoth,
		ExecutionLocalities: eitherWay,
		Configuration: []Field{
			endpointField("The base URL of your Grafana instance."),
			{
				Name:        "credential",
				Title:       "Service account token",
				Description: "Submitted once and never returned.",
				Type:        FieldString,
				Secret:      true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Endpoint", Fields: []string{"endpoint"}},
				{Title: "Authentication", Fields: []string{"credential"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Checks the token against Grafana's own identity endpoint."},
		MinimumRelayVersion:       minimumRelayVersionToday,
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"pagerduty": {
		Slug:              "pagerduty",
		DefinitionVersion: definitionVersion,
		Name:              "PagerDuty",
		Description: "Incidents delivered inbound as they are triggered, acknowledged and " +
			"resolved.",
		Category:            CategoryIncidentResponse,
		DocumentationURL:    "https://developer.pagerduty.com/docs/webhooks-overview",
		AuthModes:           []AuthMode{AuthSharedSecret},
		Roles:               storage.RoleTrigger,
		ExecutionLocalities: centrally,
		// Nothing to configure, for the same reason Alertmanager has nothing: the delivery names
		// its own service, and the Connection is already named by the operator.
		Configuration: nil,
		Presentation:  Presentation{},
		Validation: ValidationContract{Authenticates: false, ProbesCapabilities: false,
			Note: "Reached inbound only; the signature is verified per delivery."},
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"datadog": {
		Slug:                "datadog",
		DefinitionVersion:   definitionVersion,
		Name:                "Datadog",
		Description:         "Monitors delivered inbound; metrics and logs queried outbound.",
		Category:            CategoryMetrics,
		DocumentationURL:    "https://docs.datadoghq.com/api/latest/",
		AuthModes:           []AuthMode{AuthSharedSecret, AuthAPIKey},
		Roles:               storage.RoleBoth,
		ExecutionLocalities: centrally,
		Configuration: []Field{
			{
				Name:        "site",
				Title:       "Datadog site",
				Description: "Which Datadog region this account lives in.",
				Type:        FieldString,
				Required:    true,
				Enum: []string{"datadoghq.com", "datadoghq.eu", "us3.datadoghq.com",
					"us5.datadoghq.com", "ap1.datadoghq.com", "ddog-gov.com"},
			},
			{
				Name:        "credential",
				Title:       "Application key",
				Description: "Submitted once and never returned.",
				Type:        FieldString,
				Secret:      true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Account", Fields: []string{"site"}},
				{Title: "Authentication", Fields: []string{"credential"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Checks the key pair against Datadog's validate endpoint."},
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"newrelic": {
		Slug:                "newrelic",
		DefinitionVersion:   definitionVersion,
		Name:                "New Relic",
		Description:         "Alerts delivered inbound; NRQL answered outbound.",
		Category:            CategoryMetrics,
		DocumentationURL:    "https://docs.newrelic.com/docs/apis/nerdgraph/get-started/introduction-new-relic-nerdgraph/",
		AuthModes:           []AuthMode{AuthSharedSecret, AuthAPIKey},
		Roles:               storage.RoleBoth,
		ExecutionLocalities: centrally,
		Configuration: []Field{
			{
				Name:        "accountId",
				Title:       "Account",
				Description: "The New Relic account these reads are scoped to.",
				Type:        FieldString,
				Required:    true,
			},
			{
				Name:        "credential",
				Title:       "User API key",
				Description: "Submitted once and never returned.",
				Type:        FieldString,
				Secret:      true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Account", Fields: []string{"accountId"}},
				{Title: "Authentication", Fields: []string{"credential"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Runs one trivial NRQL query against the named account."},
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"zabbix": {
		Slug:                "zabbix",
		DefinitionVersion:   definitionVersion,
		Name:                "Zabbix",
		Description:         "Problems delivered inbound; host and item state read outbound.",
		Category:            CategoryAlerting,
		DocumentationURL:    "https://www.zabbix.com/documentation/current/en/manual/api",
		AuthModes:           []AuthMode{AuthSharedSecret, AuthBearerToken},
		Roles:               storage.RoleBoth,
		ExecutionLocalities: eitherWay,
		Configuration: []Field{
			endpointField("The URL of the Zabbix API endpoint, usually ending in /api_jsonrpc.php."),
			{
				Name:        "credential",
				Title:       "API token",
				Description: "Submitted once and never returned.",
				Type:        FieldString,
				Secret:      true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Endpoint", Fields: []string{"endpoint"}},
				{Title: "Authentication", Fields: []string{"credential"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Calls apiinfo.version and then one authenticated read."},
		MinimumRelayVersion:       minimumRelayVersionToday,
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"nomad": {
		Slug:                "nomad",
		DefinitionVersion:   definitionVersion,
		Name:                "HashiCorp Nomad",
		Description:         "Allocations, deployments and their recent history, read through a Relay.",
		Category:            CategoryOrchestration,
		DocumentationURL:    "https://developer.hashicorp.com/nomad/api-docs",
		AuthModes:           []AuthMode{AuthBearerToken, AuthMutualTLS},
		Roles:               storage.RoleEvidence,
		ExecutionLocalities: viaRelay,
		Configuration: []Field{
			endpointField("The address of the Nomad HTTP API."),
			{
				Name:        "region",
				Title:       "Region",
				Description: "Which Nomad region these reads are scoped to.",
				Type:        FieldString,
			},
			{
				Name:        "credential",
				Title:       "ACL token",
				Description: "Submitted once and never returned.",
				Type:        FieldString,
				Secret:      true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Endpoint", Fields: []string{"endpoint", "region"}},
				{Title: "Authentication", Fields: []string{"credential"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Reads the agent's own status through the bound Relay."},
		MinimumRelayVersion:       minimumRelayVersionToday,
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"proxmox": {
		Slug:                "proxmox",
		DefinitionVersion:   definitionVersion,
		Name:                "Proxmox VE",
		Description:         "Guest and node state, read through a Relay inside the customer's network.",
		Category:            CategoryVirtualisation,
		DocumentationURL:    "https://pve.proxmox.com/wiki/Proxmox_VE_API",
		AuthModes:           []AuthMode{AuthAPIKey},
		Roles:               storage.RoleEvidence,
		ExecutionLocalities: viaRelay,
		Configuration: []Field{
			endpointField("The base URL of the Proxmox VE API."),
			{
				Name:        "tokenId",
				Title:       "API token id",
				Description: "The user and token name, for example monitoring@pve!opencluster.",
				Type:        FieldString,
				Required:    true,
			},
			{
				Name:        "credential",
				Title:       "API token secret",
				Description: "Submitted once and never returned.",
				Type:        FieldString,
				Secret:      true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Endpoint", Fields: []string{"endpoint"}},
				{Title: "Authentication", Fields: []string{"tokenId", "credential"}},
			},
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Reads the cluster's own status through the bound Relay."},
		MinimumRelayVersion:       minimumRelayVersionToday,
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"aws": {
		Slug:              "aws",
		DefinitionVersion: definitionVersion,
		Name:              "Amazon Web Services",
		Description: "CloudWatch metrics and logs, and the recent change history of the " +
			"resources an incident names.",
		Category:            CategoryCloud,
		DocumentationURL:    "https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_create_for-user_externalid.html",
		AuthModes:           []AuthMode{AuthRoleAssumption},
		Roles:               storage.RoleEvidence,
		ExecutionLocalities: centrally,
		Configuration: []Field{
			{
				Name:        "roleArn",
				Title:       "Role to assume",
				Description: "The ARN of the role OpenCluster assumes in your account.",
				Type:        FieldString,
				Required:    true,
			},
			{
				Name:        "region",
				Title:       "Region",
				Description: "Which region these reads are scoped to.",
				Type:        FieldString,
				Required:    true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{Title: "Account", Fields: []string{"roleArn", "region"}},
			},
			// The case a generic form cannot serve, and the reason the slot exists. The external
			// id is minted by this platform, the trust policy has to be shown so it can be
			// pasted somewhere else, and the round trip is not a field — it is a conversation.
			ProviderStep: "aws-role-assumption",
		},
		Validation: ValidationContract{Authenticates: true, ProbesCapabilities: true,
			Note: "Assumes the role and calls one read-only API. A successful validation proves " +
				"the trust policy and the external id, not that every permission an " +
				"investigation may need has been granted."},
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},

	"webhook": {
		Slug:              "webhook",
		DefinitionVersion: definitionVersion,
		Name:              "Generic webhook",
		Description: "Anything that can POST JSON. The last resort, and honest about it: " +
			"nothing here knows what your payload means until you map it.",
		Category:            CategoryGeneric,
		DocumentationURL:    "https://www.rfc-editor.org/rfc/rfc8259",
		AuthModes:           []AuthMode{AuthSharedSecret},
		Roles:               storage.RoleTrigger,
		ExecutionLocalities: centrally,
		Configuration: []Field{
			{
				Name:        "titlePath",
				Title:       "Path to the title",
				Description: "A dotted path into the delivered body, for example alert.name.",
				Type:        FieldString,
				Required:    true,
			},
			{
				Name:  "identityPath",
				Title: "Path to the source's own identifier",
				Description: "What the sender calls this alert. Repeated deliveries carrying the " +
					"same value are the same episode rather than a second one.",
				Type:     FieldString,
				Required: true,
			},
		},
		Presentation: Presentation{
			Sections: []Section{
				{
					Title: "Mapping",
					Description: "What in your payload means what. Nothing is guessed: a field " +
						"this platform inferred wrongly would deduplicate two incidents into one.",
					Fields: []string{"titlePath", "identityPath"},
				},
			},
		},
		Validation: ValidationContract{Authenticates: false, ProbesCapabilities: false,
			Note: "Checks the mapping is well formed. Whether it matches what your sender " +
				"actually posts is what the test-event operation is for."},
		SupportsMultipleInstances: true,
		Lifecycle:                 LifecyclePlanned,
	},
}

// Definitions lists what this build knows about, in a stable order: the ones that can be
// configured first, then the rest alphabetically within each state, so a catalog rendered
// straight from this reads as what works followed by what is coming.
func Definitions() []Definition {
	listed := make([]Definition, 0, len(definitions))
	for _, definition := range definitions {
		listed = append(listed, definition)
	}
	sort.Slice(listed, func(i, j int) bool {
		left, right := listed[i], listed[j]
		if left.Lifecycle.Actionable() != right.Lifecycle.Actionable() {
			return left.Lifecycle.Actionable()
		}
		return left.Slug < right.Slug
	})
	return listed
}

// Lookup resolves one definition by the value a connection row stores.
func Lookup(integration Integration) (Definition, bool) {
	definition, ok := definitions[string(integration)]
	return definition, ok
}

// Configurable reports whether a Connection may be created against this Integration, and says
// why not when it may not.
//
// It is the ONE place the lifecycle rule is applied, so making a provider actionable is one
// field in one record. A handler that made this decision for itself would be a second copy of
// it, and the second copy is the one that goes stale.
func Configurable(integration Integration) (Definition, string, bool) {
	definition, known := Lookup(integration)
	if !known {
		return Definition{}, "integration " + strconv.Quote(string(integration)) +
			" is not one this build has", false
	}
	switch definition.Lifecycle {
	case LifecycleGeneral, LifecyclePreview:
		return definition, "", true
	case LifecyclePlanned:
		return Definition{}, definition.Name + " is planned and has no adapter yet, so a " +
			"connection to it could not do anything", false
	case LifecycleDeprecated:
		return Definition{}, definition.Name + " is deprecated; the connections that exist " +
			"keep working and no new one may be created", false
	default:
		return Definition{}, definition.Name + " is in a lifecycle state this build does not " +
			"know how to act on", false
	}
}

// Matches reports whether a definition answers a free-text search. It reads the name, the slug
// and the description, because an operator looking for "logs" is as likely to be searching for
// what a provider does as for what it is called.
func (d Definition) Matches(search string) bool {
	if search == "" {
		return true
	}
	wanted := strings.ToLower(search)
	return strings.Contains(strings.ToLower(d.Name), wanted) ||
		strings.Contains(strings.ToLower(d.Slug), wanted) ||
		strings.Contains(strings.ToLower(d.Description), wanted)
}
