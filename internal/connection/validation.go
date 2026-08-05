package connection

import (
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// What a caller may ask for, and every way of asking that could never work.
//
// It is its own file because it is its own subject: a reviewer asking "what does this surface
// refuse, and does it refuse it before the database has to" reads this and nothing else. Each
// of these answers the caller itself on a refusal, so a handler either has a valid value or has
// already returned.

func validName(writer http.ResponseWriter, name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "name must not be empty"})
		return "", false
	case len(trimmed) > maxNameLength:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "name must be at most " + strconv.Itoa(maxNameLength) + " characters"})
		return "", false
	}
	for _, character := range trimmed {
		if unicode.IsControl(character) {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "name must not contain control characters"})
			return "", false
		}
	}
	return trimmed, true
}

// validLabels bounds optional metadata. Labels are never an authorization, a credential or a
// tenant boundary and are consumed by nothing in this slice; these bounds exist so the column
// cannot become somewhere a caller stores whatever they like.
func validLabels(writer http.ResponseWriter, labels map[string]string) (map[string]string, bool) {
	if len(labels) > maxLabels {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "at most " + strconv.Itoa(maxLabels) + " labels"})
		return nil, false
	}
	for key, value := range labels {
		if key == "" || len(key) > maxLabelKeyLength || len(value) > maxLabelValueLength {
			writeJSON(writer, http.StatusBadRequest,
				errorView{Error: "a label key must be 1-" + strconv.Itoa(maxLabelKeyLength) +
					" characters and its value at most " + strconv.Itoa(maxLabelValueLength)})
			return nil, false
		}
	}
	return labels, true
}

func parseRole(writer http.ResponseWriter, role string) (storage.ConnectionRole, bool) {
	switch strings.TrimSpace(role) {
	case "trigger":
		return storage.RoleTrigger, true
	case "evidence":
		return storage.RoleEvidence, true
	case "both":
		return storage.RoleBoth, true
	default:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: `role must be "trigger", "evidence" or "both"`})
		return 0, false
	}
}

func parseLocality(writer http.ResponseWriter, locality string) (storage.ExecutionLocality, bool) {
	switch strings.TrimSpace(locality) {
	case "control_plane":
		return storage.LocalityControlPlane, true
	case "relay":
		return storage.LocalityRelay, true
	default:
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: `locality must be "control_plane" or "relay"`})
		return 0, false
	}
}
