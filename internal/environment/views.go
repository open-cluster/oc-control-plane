package environment

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// What this surface says on the wire. Kept apart from the handlers because it is a contract:
// a field renamed here is a client broken somewhere else, which is not true of anything in
// the handlers themselves.

// maxRequestBytes bounds a request body. These endpoints take a name and nothing more, so
// anything larger is a mistake or a probe, and either way it is refused without being held.
const maxRequestBytes = 8 << 10

type listView struct {
	Environments []environmentView `json:"environments"`
	// Next resumes the next page, and is absent when this is the last one. Its presence is also
	// how a caller knows rows were left out.
	Next string `json:"next,omitempty"`
}

type environmentView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// IsDefault marks the one created with the organization. It is stated rather than left to
	// be worked out from the name, because the name can be changed and the fact cannot.
	IsDefault bool      `json:"isDefault"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type nameRequest struct {
	Name string `json:"name"`
}

type errorView struct {
	Error string `json:"error"`
}

func viewOf(environment storage.Environment) environmentView {
	return environmentView{
		ID:        environment.ID.String(),
		Name:      environment.Name,
		IsDefault: environment.IsDefault,
		CreatedAt: environment.CreatedAt,
		UpdatedAt: environment.UpdatedAt,
	}
}

// decode reads a bounded request body. It reports false having already answered, so a caller
// returns immediately rather than deciding again what an unreadable body means.
func decode(writer http.ResponseWriter, request *http.Request, into any) bool {
	body := http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(body)
	// A field this build does not know is refused rather than ignored. An operator who
	// misspelled one should be told, not left believing they configured something.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	// Exactly one JSON value. A second one is a caller sending something this did not read.
	if _, err := decoder.Token(); err != io.EOF {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "request body is not understood"})
		return false
	}
	return true
}

// writeJSON sends a response body. An encoding failure cannot be reported to the caller — the
// status is already written — so it is dropped here and would surface as a truncated body,
// which is visibly wrong rather than quietly wrong.
//
// Nothing this surface returns may be stored or re-typed by anything in front of it: every
// response carries data from a named tenant, and a cache holding one operator's answer is a
// cross-tenant disclosure waiting for the next request.
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
