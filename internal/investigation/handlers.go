package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/describe"
	"github.com/open-cluster/oc-control-plane/internal/table"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

const (
	readTimeout     = 15 * time.Second
	maxRequestBytes = 16 << 10
	// maxQuestionLength bounds an operator's question. Long enough for a sentence with
	// identifiers in it; anything past this is a paste, not a question.
	maxQuestionLength = 1024
	// subjectCandidates bounds how many open episodes a question is matched against.
	subjectCandidates = 20
	// clarifyWith bounds how many incident titles a clarification names.
	clarifyWith = 3
)

// Handlers is this capability's dependencies.
type Handlers struct {
	Store  Store
	Runner *Runner
	Logger *slog.Logger
	// Events replays what an investigation has already emitted, for the stream route.
	// Nil serves that one route a plain refusal and leaves everything else working.
	Events EventReader
	// WindowLead widens an investigation's window before the incident began: the change
	// that caused an incident usually landed before it fired. Configuration, because the
	// right lead follows an organization's deploy cadence, not a constant.
	WindowLead time.Duration
}

// Routes is this capability's contribution to the operator API's index.
func (h Handlers) Routes() authz.Table {
	const base = "/operator/v1/organizations/{organization}"

	return authz.Table{
		authz.Privileged(http.MethodGet, base+"/investigations", authz.InvestigationRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodPost, base+"/investigations", authz.InvestigationOpen,
			http.HandlerFunc(h.open)),
		authz.Privileged(http.MethodGet, base+"/investigations/{investigation}",
			authz.InvestigationRead, http.HandlerFunc(h.read)),
		// Watching an investigation run is reading it. There is no second permission,
		// because the stream says nothing the finished record will not say — it says it
		// while there is still something to watch.
		authz.Privileged(http.MethodGet, base+"/investigations/{investigation}/events",
			authz.InvestigationRead, http.HandlerFunc(h.streamEvents)),
	}
}

// Describe is this capability's contribution to the deployment's self-description.
//
// The event stream carries no body and is not a listing: it is one investigation's own
// events as they happen, not a page of rows a caller narrows.
func (h Handlers) Describe() describe.Contribution {
	const base = "/operator/v1/organizations/{organization}/investigations"

	return describe.Contribution{
		Listings: []describe.Listing{
			{Route: http.MethodGet + " " + base, Spec: listSpec},
		},
		Bodies: []describe.Body{
			{Route: http.MethodPost + " " + base, Example: openRequest{}},
		},
	}
}

// openRequest is what starts an investigation: an episode, or a question in the
// operator's own words.
type openRequest struct {
	EpisodeID string `json:"episodeId"`
	Question  string `json:"question"`
}

// open starts an investigation and answers 202 with the running record; the runner fills
// it in the background. A question that cannot be tied to one incident answers 200 with
// ONE plain-language clarification instead — never an internal scope concept.
func (h Handlers) open(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	if h.Runner == nil || h.Runner.Investigator == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "this deployment has no model provider configured, so it cannot investigate"})
		return
	}
	if h.Runner.AtCapacity() {
		writeJSON(writer, http.StatusTooManyRequests, errorView{
			Error: "this deployment is already running its limit of investigations; wait " +
				"for one to end and open this again"})
		return
	}
	var asked openRequest
	if !h.decode(writer, request, &asked) {
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	trigger, clarification, refusal, err := h.resolveTrigger(ctx, organization, asked)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if refusal != "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
		return
	}
	if clarification != "" {
		writeJSON(writer, http.StatusOK, clarificationView{Clarification: clarification})
		return
	}

	window := windowOf(trigger, h.WindowLead)
	opened, err := h.Store.CreateInvestigation(ctx, principal, organization, NewInvestigation{
		EpisodeID:     trigger.EpisodeID,
		IntegrationID: trigger.IntegrationID,
		Question:      strings.TrimSpace(asked.Question),
		Subject:       subjectOf(trigger),
		WindowFrom:    window.from,
		WindowUntil:   window.until,
		CreatedBy:     principal.ID(),
	})
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "investigation opened",
		slog.String("org_id", organization.String()),
		slog.String("investigation_id", opened.ID.String()),
		slog.String("episode_id", trigger.EpisodeID.String()))

	h.Runner.Start(organization, opened)
	writeJSON(writer, http.StatusAccepted, investigationViewOf(opened))
}

// resolveTrigger turns the request into the episode the investigation is about. An
// episode id is taken as given; a question is matched against the organization's open
// incidents, and genuine ambiguity produces one clarification in plain language.
func (h Handlers) resolveTrigger(
	ctx context.Context, organization tenancy.Organization, asked openRequest,
) (Trigger, string, string, error) {
	episodeID := strings.TrimSpace(asked.EpisodeID)
	question := strings.TrimSpace(asked.Question)

	switch {
	case episodeID != "" && question != "":
		return Trigger{}, "", "give an episodeId or a question, not both", nil
	case episodeID != "":
		id, parsed := parseIdentity(episodeID)
		if !parsed {
			return Trigger{}, "", "episodeId is not an identity", nil
		}
		trigger, err := h.Store.TriggerEpisode(ctx, organization, id)
		if err != nil {
			return Trigger{}, "", "", err
		}
		return trigger, "", "", nil
	case question == "":
		return Trigger{}, "", "give an episodeId or a question", nil
	case len(question) > maxQuestionLength:
		return Trigger{}, "", "the question must be at most 1024 characters", nil
	}

	candidates, err := h.Store.OpenTriggers(ctx, organization, subjectCandidates)
	if err != nil {
		return Trigger{}, "", "", err
	}
	matched := matchQuestion(question, candidates)
	switch len(matched) {
	case 1:
		trigger := matched[0]
		return trigger, "", "", nil
	case 0:
		return Trigger{}, clarifyNothingMatched(candidates), "", nil
	default:
		return Trigger{}, clarifyAmbiguous(matched), "", nil
	}
}

// parseIdentity reads a caller-supplied identifier, reporting only whether it is one.
func parseIdentity(value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	return id, err == nil
}

// matchQuestion scores the question's terms against each open incident's title and
// labels, returning the best scorers. Deterministic and dumb on purpose: the answer to
// ambiguity is a clarifying question, not a guess.
func matchQuestion(question string, candidates []Trigger) []Trigger {
	terms := subjectTerms(question)
	best := 0
	var matched []Trigger
	for _, candidate := range candidates {
		var haystack strings.Builder
		haystack.WriteString(strings.ToLower(candidate.Title))
		for key, value := range candidate.Labels {
			haystack.WriteString(" " + strings.ToLower(key) + " " + strings.ToLower(value))
		}
		score := 0
		for _, term := range terms {
			if strings.Contains(haystack.String(), term) {
				score++
			}
		}
		switch {
		case score == 0:
			continue
		case score > best:
			best = score
			matched = matched[:0]
			matched = append(matched, candidate)
		case score == best:
			matched = append(matched, candidate)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].LastSeenAt.After(matched[j].LastSeenAt)
	})
	return matched
}

// clarifyNothingMatched asks the one question that gets the investigation somewhere.
func clarifyNothingMatched(candidates []Trigger) string {
	if len(candidates) == 0 {
		return "there are no open incidents to tie that to; which service or alert is " +
			"this about?"
	}
	return "I could not tie that to an open incident; is it about " +
		titleList(candidates) + ", or something else?"
}

func clarifyAmbiguous(matched []Trigger) string {
	return "that could be about more than one open incident; which one do you mean: " +
		titleList(matched) + "?"
}

func titleList(triggers []Trigger) string {
	shown := len(triggers)
	if shown > clarifyWith {
		shown = clarifyWith
	}
	titles := make([]string, 0, shown)
	for _, trigger := range triggers[:shown] {
		titles = append(titles, `"`+trigger.Title+`"`)
	}
	return strings.Join(titles, ", ")
}

// maxSubjectLength mirrors the schema's own bound on the subject column.
const maxSubjectLength = 512

// subjectOf is what the investigation is about, in plain language. An episode may carry
// an empty title — a payload can omit every name — and a subject the schema refuses
// would turn opening into a server error, so the absence is said instead.
func subjectOf(trigger Trigger) string {
	title := strings.TrimSpace(trigger.Title)
	if title == "" {
		return "an unnamed incident"
	}
	return bounded(title, maxSubjectLength)
}

// window is the investigation's time bounds.
type window struct {
	from  time.Time
	until time.Time
}

// windowOf derives the window from the episode: widened backwards by the configured
// lead, and ending now while the incident is still open.
func windowOf(trigger Trigger, lead time.Duration) window {
	until := trigger.LastSeenAt
	if !trigger.Resolved {
		until = time.Now().UTC()
	}
	return window{from: trigger.FirstSeenAt.Add(-lead), until: until}
}

// listSpec is the shared table contract this listing speaks.
var listSpec = table.Spec{
	Sortable:    []string{"createdAt"},
	DefaultSort: table.Sort{Field: "createdAt", Descending: true},
	// "Everything opened from this incident" is the question an operator arrives holding
	// after being paged. Serving it here rather than letting a client narrow a page keeps
	// the answer the same on page one and page nine.
	Filters: []string{"episodeId"},
}

func (h Handlers) list(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	parsed, err := table.Parse(request.URL.Query(), listSpec)
	if err != nil {
		if table.Refused(err) {
			writeJSON(writer, http.StatusBadRequest, errorView{Error: err.Error()})
			return
		}
		h.Logger.ErrorContext(request.Context(),
			"the investigations listing declares a query it cannot serve",
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	query := Query{Page: Page{Limit: parsed.Limit, After: parsed.Cursor}}
	if raw := parsed.Filter("episodeId"); raw != "" {
		episode, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			// Refused rather than passed to the database: a value that is not an
			// identifier cannot match anything, and answering an empty page would tell a
			// caller their typo was a real episode with nothing in it.
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "episodeId is not an identifier"})
			return
		}
		query.EpisodeID = episode
	}

	listed, err := h.Store.QueryInvestigations(ctx, principal, organization, query)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	views := make([]investigationView, 0, len(listed.Investigations))
	for _, found := range listed.Investigations {
		views = append(views, investigationViewOf(found))
	}
	writeJSON(writer, http.StatusOK, table.Answer(views, listed.Next, nil))
}

func (h Handlers) read(writer http.ResponseWriter, request *http.Request) {
	_, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	found, err := h.Store.Investigation(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	sources, runs, err := h.Store.InvestigationProvenance(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, detailViewOf(found, sources, runs))
}

func (h Handlers) caller(
	writer http.ResponseWriter, request *http.Request,
) (authz.Principal, bool) {
	principal, ok := authz.Of(request)
	if !ok {
		h.Logger.ErrorContext(request.Context(),
			"a handler ran with no principal; the route is mounted outside the permission table",
			slog.String("path", request.URL.Path))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
		return authz.Principal{}, false
	}
	return principal, true
}

func (h Handlers) organization(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, bool) {
	organization, err := tenancy.NewOrganization(request.PathValue("organization"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "organization is not a name"})
		return tenancy.Organization{}, false
	}
	return organization, true
}

func (h Handlers) addressed(
	writer http.ResponseWriter, request *http.Request,
) (tenancy.Organization, uuid.UUID, bool) {
	organization, ok := h.organization(writer, request)
	if !ok {
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	id, err := uuid.Parse(request.PathValue("investigation"))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "investigation is not an identity"})
		return tenancy.Organization{}, uuid.UUID{}, false
	}
	return organization, id, true
}

func (h Handlers) decode(
	writer http.ResponseWriter, request *http.Request, into any,
) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(writer, http.StatusBadRequest,
			errorView{Error: "the request body is not what this operation accepts"})
		return false
	}
	return true
}

func (h Handlers) fail(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, authz.ErrNotAMember):
		// The same answer the authorization middleware gives, byte for byte. A different
		// one would confirm to a caller that a tenant they may not reach exists.
		writeJSON(writer, http.StatusNotFound, errorView{Error: "organization not found"})
	case errors.Is(err, ErrUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "investigation not found"})
	case errors.Is(err, ErrEpisodeUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "episode not found"})
	case errors.Is(err, ErrBadCursor):
		writeJSON(writer, http.StatusBadRequest, errorView{Error: ErrBadCursor.Error()})
	case errors.Is(err, audit.ErrWriteFailed):
		h.Logger.ErrorContext(request.Context(), "an operation was rolled back unrecorded",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "the change was refused because it could not be recorded"})
	default:
		h.Logger.ErrorContext(request.Context(), "investigation request failed",
			slog.String("path", request.URL.Path),
			slog.String("error", err.Error()))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
	}
}

// subjectTerms reduces the subject and question to the words worth matching: lower-cased,
// punctuation split, short noise words dropped. Deliberately dumb — a stemmer or an
// embedding here would make the matching unexplainable, and the explanation is the point.
func subjectTerms(parts ...string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, part := range parts {
		for _, word := range strings.FieldsFunc(strings.ToLower(part), func(r rune) bool {
			letter := 'a' <= r && r <= 'z'
			digit := '0' <= r && r <= '9'
			return !letter && !digit && r != '-' && r != '_'
		}) {
			if len(word) < 3 || noiseWords[word] || seen[word] {
				continue
			}
			seen[word] = true
			terms = append(terms, word)
		}
	}
	return terms
}

// noiseWords are the words that match everything and select nothing.
var noiseWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "what": true, "why": true,
	"how": true, "is": true, "are": true, "was": true, "has": true, "have": true,
	"our": true, "this": true, "that": true, "not": true, "you": true, "about": true,
	"happening": true, "wrong": true, "down": true, "broken": true,
}
