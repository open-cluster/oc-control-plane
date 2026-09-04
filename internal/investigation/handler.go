package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/api/pagination"
	"github.com/open-cluster/oc-control-plane/internal/audit"
	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
)

const (
	readTimeout     = 15 * time.Second
	maxRequestBytes = 16 << 10
)

// Handlers is this domain surface's dependencies.
type Handlers struct {
	Store      HTTPStore
	Runner     *Runner
	Logger     *slog.Logger
	MaxPending int
	// WindowLead widens an investigation's window before the incident began: the change
	// that caused an incident usually landed before it fired. Configuration, because the
	// right lead follows an organization's deploy cadence, not a constant.
	WindowLead time.Duration
}

// HTTPStore is the durable state used by the Investigation operator surface.
type HTTPStore interface {
	CreateInvestigation(context.Context, authz.Principal, tenancy.Organization,
		NewInvestigation, int) (Investigation, error)
	Investigation(context.Context, tenancy.Organization, uuid.UUID) (Investigation, error)
	InvestigationToolRuns(context.Context, tenancy.Organization, uuid.UUID) ([]ToolRun, error)
	QueryInvestigations(context.Context, authz.Principal, tenancy.Organization, Query) (List, error)
	CancelInvestigation(context.Context, authz.Principal, tenancy.Organization,
		uuid.UUID) (Investigation, error)
	TriggerIncident(context.Context, tenancy.Organization, uuid.UUID) (Trigger, error)
	Events(context.Context, tenancy.Organization, uuid.UUID, int64, int) ([]Event, error)
}

// Routes is this domain surface's contribution to the operator API's index.
func (h Handlers) Routes() authz.Table {
	const base = "/api/v1"

	return authz.Table{
		authz.Privileged(http.MethodGet, base+"/investigations", authz.InvestigationRead,
			http.HandlerFunc(h.list)),
		authz.Privileged(http.MethodPost, base+"/investigations", authz.InvestigationOpen,
			http.HandlerFunc(h.open)),
		authz.Privileged(http.MethodGet, base+"/investigations/{investigation}",
			authz.InvestigationRead, http.HandlerFunc(h.read)),
		authz.Privileged(http.MethodPost, base+"/investigations/{investigation}/cancel",
			authz.InvestigationCancel, http.HandlerFunc(h.cancel)),
		authz.Privileged(http.MethodGet, base+"/investigations/{investigation}/events",
			authz.InvestigationRead, http.HandlerFunc(h.streamEvents)),
	}
}

// openRequest is what starts a direct investigation.
type openRequest struct {
	IncidentID string `json:"incidentId"`
}

// open starts an investigation for one Incident and answers 202 with the running record;
// the runner fills it in the background.
func (h Handlers) open(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, ok := h.organization(writer, request)
	if !ok {
		return
	}
	if h.Runner == nil || h.Runner.Agent == nil {
		writeJSON(writer, http.StatusServiceUnavailable, errorView{
			Error: "this deployment has no model provider configured, so it cannot investigate"})
		return
	}
	var asked openRequest
	if !h.decode(writer, request, &asked) {
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()

	trigger, refusal, err := h.resolveTrigger(ctx, organization, asked)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if refusal != "" {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: refusal})
		return
	}
	window := windowOf(trigger, h.WindowLead)
	opened, err := h.Store.CreateInvestigation(ctx, principal, organization, NewInvestigation{
		IncidentID:  trigger.IncidentID,
		Subject:     subjectOf(trigger),
		WindowFrom:  window.from,
		WindowUntil: window.until,
		CreatedBy:   principal.ID(),
	}, h.MaxPending)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	h.Logger.InfoContext(ctx, "investigation opened",
		slog.String("org_id", organization.String()),
		slog.String("investigation_id", opened.ID.String()),
		slog.String("incident_id", trigger.IncidentID.String()))

	writeJSON(writer, http.StatusAccepted, investigationViewOf(opened))
}

// resolveTrigger turns the required identifier into the Incident the Investigation is about.
func (h Handlers) resolveTrigger(
	ctx context.Context, organization tenancy.Organization, asked openRequest,
) (Trigger, string, error) {
	incidentID := strings.TrimSpace(asked.IncidentID)
	if incidentID == "" {
		return Trigger{}, "give an incidentId", nil
	}
	id, parsed := parseIdentity(incidentID)
	if !parsed {
		return Trigger{}, "incidentId is not an identity", nil
	}
	trigger, err := h.Store.TriggerIncident(ctx, organization, id)
	if err != nil {
		return Trigger{}, "", err
	}
	return trigger, "", nil
}

// parseIdentity reads a caller-supplied identifier, reporting only whether it is one.
func parseIdentity(value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	return id, err == nil
}

// maxSubjectLength mirrors the schema's own bound on the subject column.
const maxSubjectLength = 512

// subjectOf is what the investigation is about, in plain language. An incident may carry
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

// windowOf derives the window from the incident: widened backwards by the configured
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
	Filters: []string{"incidentId"},
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
	if raw := parsed.Filter("incidentId"); raw != "" {
		incident, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			// Refused rather than passed to the database: a value that is not an
			// identifier cannot match anything, and answering an empty page would tell a
			// caller their typo was a real incident with nothing in it.
			writeJSON(writer, http.StatusBadRequest, errorView{
				Error: "incidentId is not an identifier"})
			return
		}
		query.IncidentID = incident
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
	runs, err := h.Store.InvestigationToolRuns(ctx, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, detailViewOf(found, runs))
}

func (h Handlers) cancel(writer http.ResponseWriter, request *http.Request) {
	principal, ok := h.caller(writer, request)
	if !ok {
		return
	}
	organization, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	ctx, done := context.WithTimeout(request.Context(), readTimeout)
	defer done()
	ended, err := h.Store.CancelInvestigation(ctx, principal, organization, id)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	if h.Runner != nil {
		h.Runner.Cancel(id)
	}
	writeJSON(writer, http.StatusOK, investigationViewOf(ended))
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
	organization, ok := authz.ActiveOrganizationFrom(request.Context())
	if !ok {
		h.Logger.ErrorContext(request.Context(),
			"a handler ran with no verified active organization",
			slog.String("path", request.URL.Path))
		writeJSON(writer, http.StatusInternalServerError, errorView{Error: "request failed"})
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
	case errors.Is(err, ErrIncidentUnknown):
		writeJSON(writer, http.StatusNotFound, errorView{Error: "incident not found"})
	case errors.Is(err, ErrQueueFull):
		writeJSON(writer, http.StatusTooManyRequests, errorView{
			Error: "this Organization has reached its pending Investigation limit; wait for work to start and try again"})
	case errors.Is(err, ErrAlreadyEnded):
		writeJSON(writer, http.StatusConflict, errorView{Error: "investigation has already ended"})
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

// What this surface says on the wire. Kept apart from the handlers because it is a
// contract: a field renamed here is a client broken somewhere else.

type errorView struct {
	Error string `json:"error"`
}

type findingView struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	// Kind is the finding's causal role and Confidence its categorical certainty.
	// Absent on findings concluded before the vocabulary existed.
	Kind       string `json:"kind,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Mechanism  string `json:"mechanism,omitempty"`
	// Sources are one-based ordinals among the investigation's runs.
	RunRefs []int `json:"runRefs"`
}

type usageView struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

type investigationView struct {
	ID                        string             `json:"id"`
	Status                    string             `json:"status"`
	Subject                   string             `json:"subject"`
	Question                  string             `json:"question,omitempty"`
	IncidentID                string             `json:"incidentId,omitempty"`
	WindowFrom                string             `json:"windowFrom"`
	WindowUntil               string             `json:"windowUntil"`
	ConclusionStatus          ConclusionStatus   `json:"conclusionStatus,omitempty"`
	Summary                   string             `json:"summary,omitempty"`
	Impact                    ImpactAssessment   `json:"impact"`
	Findings                  []findingView      `json:"findings"`
	Hypotheses                []HypothesisResult `json:"hypotheses"`
	Actions                   []ActionProposal   `json:"actions"`
	Limitations               []Limitation       `json:"limitations"`
	HumanConfirmationRequired bool               `json:"humanConfirmationRequired"`
	// StoppedBy labels a conclusion a ceiling forced — "tool_runs", "reasoner_turns",
	// "wall_clock", "stagnation", "context" — so a stopped
	// investigation never renders as a free diagnosis. Absent when the model concluded
	// freely.
	StoppedBy   string    `json:"stoppedBy,omitempty"`
	Error       string    `json:"error,omitempty"`
	Usage       usageView `json:"usage"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	CreatedAt   string    `json:"createdAt"`
	ConcludedAt string    `json:"concludedAt,omitempty"`
}

type runView struct {
	Ordinal       int            `json:"ordinal"`
	IntegrationID string         `json:"integrationId,omitempty"`
	Tool          string         `json:"tool"`
	Purpose       string         `json:"purpose,omitempty"`
	HypothesisID  string         `json:"hypothesisId,omitempty"`
	Arguments     map[string]any `json:"arguments,omitempty"`
	WindowFrom    string         `json:"windowFrom"`
	WindowUntil   string         `json:"windowUntil"`
	Outcome       string         `json:"outcome"`
	Truncated     bool           `json:"truncated,omitempty"`
	Summary       string         `json:"summary,omitempty"`
	Sources       []string       `json:"sources,omitempty"`
	Error         string         `json:"error,omitempty"`
	StartedAt     string         `json:"startedAt"`
	FinishedAt    string         `json:"finishedAt"`
}

// detailView is one Investigation with the durable Tool Runs that support it.
type detailView struct {
	investigationView
	Runs []runView `json:"runs"`
}

func investigationViewOf(found Investigation) investigationView {
	findings := make([]findingView, 0, len(found.Conclusion.Findings))
	for _, finding := range found.Conclusion.Findings {
		findings = append(findings, findingView{
			ID: finding.ID, Statement: finding.Statement, Kind: finding.Kind,
			Confidence: finding.Confidence, Mechanism: finding.Mechanism, RunRefs: finding.Sources,
		})
	}
	humanConfirmationRequired := false
	for _, action := range found.Conclusion.Actions {
		humanConfirmationRequired = humanConfirmationRequired || action.RequiresApproval
	}
	for _, limitation := range found.Conclusion.Limitations {
		humanConfirmationRequired = humanConfirmationRequired || limitation.Type == LimitationEssentialHumanInput
	}
	view := investigationView{
		ID:               found.ID.String(),
		Status:           publicStatus(found),
		Subject:          found.Subject,
		Question:         found.Question,
		WindowFrom:       stamp(found.WindowFrom),
		WindowUntil:      stamp(found.WindowUntil),
		ConclusionStatus: found.Conclusion.Status,
		Summary:          found.Conclusion.Summary, Impact: found.Conclusion.Impact,
		Findings: findings, Hypotheses: found.Conclusion.Hypotheses,
		Actions: found.Conclusion.Actions, Limitations: found.Conclusion.Limitations,
		HumanConfirmationRequired: humanConfirmationRequired,
		StoppedBy:                 found.StoppedBy,
		Error:                     found.Error,
		Usage: usageView{
			InputTokens: found.Usage.InputTokens, OutputTokens: found.Usage.OutputTokens,
		},
		CreatedBy: found.CreatedBy,
		CreatedAt: stamp(found.CreatedAt),
	}
	if found.IncidentID != uuid.Nil {
		view.IncidentID = found.IncidentID.String()
	}
	if !found.ConcludedAt.IsZero() {
		view.ConcludedAt = stamp(found.ConcludedAt)
	}
	return view
}

func publicStatus(found Investigation) string {
	switch found.Status {
	case StatusRunning:
		if found.Executing {
			return "investigating"
		}
		return "queued"
	case StatusConcluded:
		for _, limitation := range found.Conclusion.Limitations {
			if limitation.Type == LimitationEssentialHumanInput {
				return "needs_input"
			}
		}
		if found.StoppedBy != "" || found.Conclusion.Status == Inconclusive {
			return "partial"
		}
		return "concluded"
	case StatusCancelled:
		return "cancelled"
	case StatusFailed:
		return "failed"
	default:
		return "unrecognised"
	}
}

func detailViewOf(found Investigation, runs []ToolRun) detailView {
	view := detailView{
		investigationView: investigationViewOf(found),
		Runs:              make([]runView, 0, len(runs)),
	}
	for _, run := range runs {
		rendered := runView{
			Ordinal:      run.Ordinal,
			Tool:         run.Tool,
			Purpose:      run.Purpose,
			HypothesisID: run.HypothesisID,
			Arguments:    run.Arguments,
			WindowFrom:   stamp(run.WindowFrom),
			WindowUntil:  stamp(run.WindowUntil),
			Outcome:      outcomeWord(run.Outcome),
			Truncated:    run.Truncated,
			Summary:      run.Summary,
			Sources:      run.Sources,
			Error:        run.Error,
			StartedAt:    stamp(run.StartedAt),
			FinishedAt:   stamp(run.FinishedAt),
		}
		if run.IntegrationID != uuid.Nil {
			rendered.IntegrationID = run.IntegrationID.String()
		}
		view.Runs = append(view.Runs, rendered)
	}
	return view
}

func outcomeWord(outcome RunOutcome) string {
	if outcome == RunSucceeded {
		return "succeeded"
	}
	return "failed"
}

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// writeJSON answers with a body. Nothing this surface returns may be cached: every answer
// concerns a named tenant's record.
func writeJSON(writer http.ResponseWriter, code int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(body)
}
