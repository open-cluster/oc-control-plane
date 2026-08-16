package incident

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// What this surface says on the wire. Kept apart from the handlers because it is a contract: a
// field renamed here is a client broken somewhere else, which is not true of anything in the
// handlers themselves.

// maxRequestBytes bounds a request body. The one write here takes an identity and a sentence, so
// anything larger is a mistake or a probe, and either way it is refused without being held.
const maxRequestBytes = 8 << 10

type episodeView struct {
	ID            string `json:"id"`
	IntegrationID string `json:"integrationId"`
	Title         string `json:"title"`
	Status        string `json:"status"`

	// Grouping is why these Signals are one incident. It is on every episode rather than offered
	// as a separate lookup, because a grouping an operator cannot immediately explain is one they
	// will argue with rather than act on.
	Grouping groupingView `json:"grouping"`

	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	// ResolvedAt is absent while any Signal in this episode is still firing.
	ResolvedAt  *time.Time `json:"resolvedAt"`
	SignalCount int        `json:"signalCount"`

	// Supersession is present only on an episode an operator merged into another. Its absence is
	// the ordinary case and carries no information, which is why it is omitted rather than nulled.
	Supersession *supersessionView `json:"supersededBy,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// groupingView states who decided that these Signals belong together, in the source's own terms
// and in words.
type groupingView struct {
	// Basis is the machine-readable answer, so a console can render a badge without parsing prose.
	Basis string `json:"basis"`
	// Explanation is the same answer for a person. Both are served because the audience is both.
	Explanation string `json:"explanation"`
	// Key is the source's own grouping identity. It is shown because it is what an operator has in
	// front of them when they arrive from their own alerting, and it is untrusted text like
	// everything else a customer's system produced.
	Key string `json:"key"`
}

type supersessionView struct {
	EpisodeID string `json:"episodeId"`
	// Reason is the operator's own words for why these are one incident. A merge without one is
	// refused, so this is never empty on a supersession that exists.
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type signalView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Summary is free text from a customer's systems and is untrusted for its whole life. It
	// reaches a console as data and must never be rendered as markup or followed as an
	// instruction.
	Summary string            `json:"summary"`
	Labels  map[string]string `json:"labels"`
	Status  string            `json:"status"`
	// StartedAt and ResolvedAt are the SOURCE's clock; ReceivedAt is ours. They are separate
	// because collapsing them makes a delayed delivery indistinguishable from a delayed failure.
	StartedAt  time.Time  `json:"startedAt"`
	ResolvedAt *time.Time `json:"resolvedAt"`
	ReceivedAt time.Time  `json:"receivedAt"`
}

type mergeRequest struct {
	// Into names the episode that survives. The one in the path gives way.
	Into string `json:"into"`
	// Reason is why an operator says these are one incident, and is required.
	Reason string `json:"reason"`
}

type errorView struct {
	Error string `json:"error"`
}

func viewOf(episode Episode) episodeView {
	view := episodeView{
		ID:            episode.ID.String(),
		IntegrationID: episode.Integration.String(),
		Title:         episode.Title,
		Status:        episode.Status.String(),
		Grouping: groupingView{
			Basis:       episode.Basis.String(),
			Explanation: episode.Basis.Explain(),
			Key:         episode.GroupingKey,
		},
		FirstSeenAt: episode.FirstSeenAt,
		LastSeenAt:  episode.LastSeenAt,
		SignalCount: episode.SignalCount,
		CreatedAt:   episode.CreatedAt,
		UpdatedAt:   episode.UpdatedAt,
	}
	if !episode.ResolvedAt.IsZero() {
		resolved := episode.ResolvedAt
		view.ResolvedAt = &resolved
	}
	if episode.SupersededBy != nil {
		view.Supersession = &supersessionView{
			EpisodeID: episode.SupersededBy.String(),
			Reason:    episode.SupersedeReason,
			At:        episode.SupersededAt,
		}
	}
	return view
}

func signalViewOf(signal Signal) signalView {
	view := signalView{
		ID:         signal.ID.String(),
		Title:      signal.Title,
		Summary:    signal.Summary,
		Labels:     signal.Labels,
		Status:     "resolved",
		StartedAt:  signal.StartedAt,
		ReceivedAt: signal.ReceivedAt,
	}
	if signal.Firing {
		view.Status = "firing"
	}
	if signal.Labels == nil {
		// An absent label set encodes as {} rather than null, so a client does not have to handle
		// two spellings of "none".
		view.Labels = map[string]string{}
	}
	if !signal.ResolvedAt.IsZero() {
		resolved := signal.ResolvedAt
		view.ResolvedAt = &resolved
	}
	return view
}

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
// anything in front of it: every response carries a named tenant's incidents.
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
