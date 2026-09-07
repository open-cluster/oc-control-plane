package investigation

type HistoryPage struct {
	NextBefore      int64          `json:"nextBefore"`
	MissingEvidence bool           `json:"missingEvidence,omitempty"`
	Limitations     []string       `json:"limitations,omitempty"`
	Exchange        []BriefMessage `json:"exchange"`
	Truncated       bool           `json:"truncated,omitempty"`
}
