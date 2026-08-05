package connection

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"

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

// roleByName and localityByName resolve the two enumerations from their wire spelling. They
// answer nobody: a body field refuses through parseRole below, and a query filter narrows to
// nothing, and those are different responses to the same unrecognised word.
func roleByName(role string) (storage.ConnectionRole, bool) {
	switch strings.TrimSpace(role) {
	case "trigger":
		return storage.RoleTrigger, true
	case "evidence":
		return storage.RoleEvidence, true
	case "both":
		return storage.RoleBoth, true
	default:
		return 0, false
	}
}

func localityByName(locality string) (storage.ExecutionLocality, bool) {
	switch strings.TrimSpace(locality) {
	case "control_plane":
		return storage.LocalityControlPlane, true
	case "relay":
		return storage.LocalityRelay, true
	default:
		return 0, false
	}
}

// stateByName resolves a lifecycle state from its wire spelling, for the listing that narrows by
// one. It answers nobody: an unrecognised state is refused by the caller, because narrowing to
// nothing looks exactly like a tenant that has none.
func stateByName(state string) (storage.ConnectionState, bool) {
	for _, known := range []storage.ConnectionState{
		storage.ConnectionConfigured, storage.ConnectionValidating, storage.ConnectionActive,
		storage.ConnectionDegraded, storage.ConnectionFailed,
	} {
		if known.String() == strings.TrimSpace(state) {
			return known, true
		}
	}
	return 0, false
}

func parseRole(writer http.ResponseWriter, role string) (storage.ConnectionRole, bool) {
	parsed, ok := roleByName(role)
	if !ok {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: `role must be "trigger", "evidence" or "both"`})
		return 0, false
	}
	return parsed, true
}

func parseLocality(writer http.ResponseWriter, locality string) (storage.ExecutionLocality, bool) {
	parsed, ok := localityByName(locality)
	if !ok {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: `locality must be "control_plane" or "relay"`})
		return 0, false
	}
	return parsed, true
}

// checkedConfiguration validates a submitted configuration against the Integration definition's
// schema and strips anything write-only out of it.
//
// The strip is the important half and it is not a formality: the configuration column is read
// back by every list and every detail read, so a credential that reached it would be a credential
// this surface hands out. The schema says `writeOnly`; this is what makes that true rather than
// advisory.
func (h Handlers) checkedConfiguration(
	writer http.ResponseWriter, definition Definition, submitted map[string]any,
) (map[string]any, bool) {
	checked := make(map[string]any, len(submitted))
	for name, value := range submitted {
		field, declared := definition.Field(name)
		if !declared {
			// Refused rather than dropped. The schema is closed, and a caller who misspelled a
			// field should be told rather than left believing they configured something.
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: definition.Name + " has no configuration field called " + strconv.Quote(name),
			})
			return nil, false
		}
		if field.Secret {
			// A write-only field is accepted and never stored here. Where a provider's credential
			// becomes storable, it goes through the credential path with its own permission; it
			// does not arrive in a configuration edit.
			continue
		}
		if !valueFits(field, value) {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: strconv.Quote(name) + " must be " + describe(field),
			})
			return nil, false
		}
		checked[name] = value
	}
	for _, field := range definition.Configuration {
		if !field.Required || field.Secret {
			continue
		}
		if _, given := checked[field.Name]; !given {
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: definition.Name + " needs " + strconv.Quote(field.Name) + ": " +
					field.Description,
			})
			return nil, false
		}
	}
	return checked, true
}

// valueFits checks one submitted value against the field it claims to be. It is a small type
// check rather than a JSON Schema engine, because the schema this build renders uses exactly
// three types and a closed enum — and a validator that handled more than the schema can express
// would be code with no caller.
func valueFits(field Field, value any) bool {
	switch field.Type {
	case FieldString:
		text, ok := value.(string)
		if !ok {
			return false
		}
		if len(field.Enum) == 0 {
			return true
		}
		return slices.Contains(field.Enum, text)
	case FieldInteger:
		// Every JSON number decodes to float64 through encoding/json, so "is it an integer" is a
		// question about the value rather than about the Go type.
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	case FieldBoolean:
		_, ok := value.(bool)
		return ok
	default:
		return false
	}
}

func describe(field Field) string {
	if len(field.Enum) > 0 {
		return "one of " + strconv.Quote(field.Enum[0]) + " and the other values this " +
			"provider's schema lists"
	}
	return string(field.Type)
}

// mintFingerprint returns an identity for a credential.
//
// It is random rather than derived from the secret, and that is the point: a fingerprint that
// was a truncated hash would let anyone holding a database dump confirm a guessed secret
// offline, which is the exact property digest-only storage exists to deny. What an operator
// needs is to tell one credential from the next, and a minted value does that and leaks nothing.
func mintFingerprint() string {
	return uuid.NewString()
}
