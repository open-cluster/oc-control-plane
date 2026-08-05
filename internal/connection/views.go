package connection

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// What this surface says on the wire. Kept apart from the handlers because it is a contract:
// a field renamed here is a client broken somewhere else.

const maxRequestBytes = 16 << 10

type listView struct {
	Connections []connectionView `json:"connections"`
	Next        string           `json:"next,omitempty"`
}

// connectionView is what a Connection looks like to an operator. It carries no secret and no
// digest: the digest is not a credential but publishing it would let anyone holding a database
// dump confirm a guess offline, which is exactly the property digest-only storage exists for.
type connectionView struct {
	ID          string `json:"id"`
	Environment string `json:"environmentId"`
	// Integration is the KIND of system this is an instance of, and Name is which instance.
	// Both are shown because an operator with two Alertmanager Connections needs to see that
	// they share an integration and differ in everything else.
	Integration string `json:"integration"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Locality    string `json:"locality"`
	// RelayRegistrationID is absent for a control-plane connection, which no Relay serves.
	RelayRegistrationID string            `json:"relayRegistrationId,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	// DisabledAt is absent while the connection is live, so its presence is the state rather
	// than a field an operator has to compare against a zero value.
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// createdView is the one response that carries a secret, and the only time one exists in a
// readable form anywhere in this system.
type createdView struct {
	Connection   connectionView `json:"connection"`
	Secret       string         `json:"secret,omitempty"`
	SecretNotice string         `json:"secretNotice,omitempty"`
}

type rotatedView struct {
	Secret       string `json:"secret"`
	SecretNotice string `json:"secretNotice"`
}

type createRequest struct {
	// EnvironmentID is the scope this Connection belongs to. It moved out of the path when the
	// collection became Organization-wide; it is still assigned exactly once, here, and the
	// composite foreign key still refuses one belonging to another tenant.
	EnvironmentID string `json:"environmentId"`
	Integration   string `json:"integration"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	Locality      string `json:"locality"`
	// RelayRegistrationID names the installation that serves a relay-local connection, and is
	// refused on a central one rather than ignored.
	RelayRegistrationID string `json:"relayRegistrationId"`
	// Secret is optional. Omitted, the platform mints one; supplied, it must clear the same
	// floor a minted one clears by construction.
	Secret string            `json:"secret"`
	Labels map[string]string `json:"labels"`
}

type secretRequest struct {
	Secret string `json:"secret"`
}

type integrationsView struct {
	Integrations []integrationView `json:"integrations"`
}

// integrationView tells an operator what exists and what each kind can do, so configuring one
// is not a matter of guessing a string and a role that might not go together.
type integrationView struct {
	Integration string   `json:"integration"`
	Roles       []string `json:"roles"`
	// ConfiguredConnections is how many Connections of this Integration the tenant has. It is
	// the reason this route is Organization-scoped: a catalog nobody can count against is a
	// list of what the product supports rather than a view of what the customer runs.
	ConfiguredConnections int `json:"configuredConnections"`
}

type errorView struct {
	Error string `json:"error"`
}

func viewOf(found storage.Connection) connectionView {
	view := connectionView{
		ID:          found.ID.String(),
		Environment: found.Environment.String(),
		Integration: found.Integration,
		Name:        found.Name,
		Role:        found.Role.String(),
		Locality:    found.Locality.String(),
		Labels:      found.Labels,
		CreatedAt:   found.CreatedAt,
		UpdatedAt:   found.UpdatedAt,
	}
	if found.RelayRegistration != uuid.Nil {
		view.RelayRegistrationID = found.RelayRegistration.String()
	}
	if found.Disabled() {
		disabled := found.DisabledAt
		view.DisabledAt = &disabled
	}
	return view
}

// roleNames renders a role set for the integrations listing.
func roleNames(role storage.ConnectionRole) []string {
	names := make([]string, 0, 2)
	if role.Includes(storage.RoleTrigger) {
		names = append(names, storage.RoleTrigger.String())
	}
	if role.Includes(storage.RoleEvidence) {
		names = append(names, storage.RoleEvidence.String())
	}
	return names
}

// decode reads a bounded request body, refusing a field this build does not know rather than
// ignoring it: an operator who misspelled one should be told, not left believing they
// configured something.
func decode(writer http.ResponseWriter, request *http.Request, into any) bool {
	body := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	return true
}

// writeJSON sends a response body. Nothing this surface returns may be stored or re-typed by
// anything in front of it: one response carries a newly minted credential, and every other
// carries a named tenant's configuration.
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

// enabledRequest is the body of the one operation that replaced the enable and disable pair.
//
// The field is a POINTER so that "I did not say" is distinguishable from "I said false". A
// missing body defaulting to one direction would be the API deciding, during an incident, what
// an operator meant about a Connection an investigation may be reading through.
type enabledRequest struct {
	Enabled *bool `json:"enabled"`
}
