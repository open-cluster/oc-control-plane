package investigation

import (
	"bytes"
	"encoding/json"

	"github.com/google/uuid"
)

// HistoricalEvidenceRef is the read address found in retained reports and bookmarks.
type HistoricalEvidenceRef struct {
	ToolRunID       uuid.UUID `json:"toolRunId,omitzero"`
	InvestigationID uuid.UUID `json:"investigationId,omitzero"`
	ToolRunOrdinal  int       `json:"toolRunOrdinal,omitempty"`
}

type HistoricalCitationFields struct {
	RunRefs          []int                   `json:"runRefs,omitempty"`
	EvidenceRefs     []HistoricalEvidenceRef `json:"evidenceRefs,omitempty"`
	MessageSequences []int64                 `json:"messageSequences,omitempty"`
}

// HistoricalCitations keeps historical wire fields by section and original entry position.
type HistoricalCitations map[string][]HistoricalCitationFields

func ReadHistoricalCitations(document []byte, sections ...string) (HistoricalCitations, error) {
	if !bytes.Contains(document, []byte(`"runRefs"`)) &&
		!bytes.Contains(document, []byte(`"toolRunOrdinal"`)) &&
		!bytes.Contains(document, []byte(`"messageSequences"`)) {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(document, &raw); err != nil {
		return nil, err
	}
	retained := make(HistoricalCitations)
	for _, section := range sections {
		encoded := raw[section]
		if len(encoded) == 0 {
			continue
		}
		var entries []HistoricalCitationFields
		if encoded[0] == '{' {
			entries = make([]HistoricalCitationFields, 1)
			if err := json.Unmarshal(encoded, &entries[0]); err != nil {
				return nil, err
			}
		} else if err := json.Unmarshal(encoded, &entries); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.hasHistoricalFields() {
				retained[section] = entries
				break
			}
		}
	}
	return retained, nil
}

func (f HistoricalCitationFields) hasHistoricalFields() bool {
	if len(f.RunRefs) > 0 || len(f.MessageSequences) > 0 {
		return true
	}
	for _, ref := range f.EvidenceRefs {
		if ref.ToolRunOrdinal != 0 || ref.InvestigationID != uuid.Nil {
			return true
		}
	}
	return false
}

// WithHistoricalCitations restores only the retained fields that were part of the old wire contract.
func WithHistoricalCitations(document []byte, retained HistoricalCitations) ([]byte, error) {
	if len(retained) == 0 {
		return document, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(document, &raw); err != nil {
		return nil, err
	}
	for section, fields := range retained {
		encoded := raw[section]
		if len(encoded) == 0 {
			continue
		}
		var entries []map[string]any
		single := encoded[0] == '{'
		if single {
			entries = make([]map[string]any, 1)
			if err := json.Unmarshal(encoded, &entries[0]); err != nil {
				return nil, err
			}
		} else if err := json.Unmarshal(encoded, &entries); err != nil {
			return nil, err
		}
		for index, entry := range entries {
			if index >= len(fields) || !fields[index].hasHistoricalFields() {
				continue
			}
			legacy := fields[index]
			if len(legacy.RunRefs) > 0 {
				entry["runRefs"] = legacy.RunRefs
			}
			if len(legacy.EvidenceRefs) > 0 {
				entry["evidenceRefs"] = legacy.EvidenceRefs
			}
			if len(legacy.MessageSequences) > 0 {
				entry["messageSequences"] = legacy.MessageSequences
			}
		}
		var output any = entries
		if single {
			output = entries[0]
		}
		merged, err := json.Marshal(output)
		if err != nil {
			return nil, err
		}
		raw[section] = merged
	}
	return json.Marshal(raw)
}
