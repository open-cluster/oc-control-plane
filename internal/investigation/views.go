package investigation

import "time"

// What this surface says on the wire. Kept apart from the handlers because it is a contract: a
// field renamed here is a client broken somewhere else.
//
// Three things are absent by construction and are meant to stay absent. There is no confidence
// number, no coverage percentage and no calibrated probability anywhere below — the basis an
// outcome rests on is supporting claims, contradicting claims, relevant checks not made,
// independent sources, and the reasons alternatives were set aside. There is no secret, no
// credential digest and no unredacted payload, because the read surface must not be a disclosure
// path. And there is no field for a figure with no evidence behind it: affected scope is a list of
// cited statements rather than a count, so a number cannot reach the page uncited.

const maxRequestBytes = 16 << 10

type errorView struct {
	Error string `json:"error"`
}

// openRequest is what an engineer sends to start a case. It names a Connection, a scope and a
// window, and nothing else — in particular it cannot name an Environment, because the Environment
// is derived from the Connection and a field for it would imply a value that is honoured.
type openRequest struct {
	ConnectionID string `json:"connectionId"`
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workloadKind"`
	WorkloadName string `json:"workloadName"`
	// WindowStart and WindowEnd are RFC 3339. Both are required: an absent one read as the
	// beginning or the end of time turns a bounded investigation into an unbounded one through an
	// omission rather than a decision.
	WindowStart time.Time `json:"windowStart"`
	WindowEnd   time.Time `json:"windowEnd"`
}

type scopeView struct {
	ConnectionID string `json:"connectionId"`
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workloadKind"`
	WorkloadName string `json:"workloadName"`
}

type windowView struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type triggerView struct {
	Kind        string    `json:"kind"`
	RequestedBy string    `json:"requestedBy"`
	At          time.Time `json:"at"`
}

type countsView struct {
	Rounds     int `json:"rounds"`
	Evidence   int `json:"evidence"`
	Timeline   int `json:"timeline"`
	Gaps       int `json:"coverageGaps"`
	Hypotheses int `json:"hypotheses"`
	Requests   int `json:"activity"`
	Outcomes   int `json:"outcomes"`
}

// spendView is what a case has consumed. Money is an integer count of micro-cents and is labelled
// as such: a cost ceiling is an operator fact and never a currency shown to the person on call.
type spendView struct {
	Tokens     int64 `json:"tokens"`
	MicroCents int64 `json:"microCents"`
	DurationMS int64 `json:"durationMs"`
}

// identityView is the case's identity and scope, shared by the summary and every listing row so
// the two cannot describe the same case differently.
type identityView struct {
	ID            string      `json:"id"`
	EnvironmentID string      `json:"environmentId"`
	EpisodeKey    string      `json:"episodeKey,omitempty"`
	Scope         scopeView   `json:"scope"`
	Window        windowView  `json:"window"`
	Trigger       triggerView `json:"trigger"`
	Lifecycle     string      `json:"lifecycle"`
	// Running distinguishes a case a worker currently holds from a quiet one. An operator needs it
	// to tell a case that is waiting from one that has stalled.
	Running      bool       `json:"running"`
	Terminal     bool       `json:"terminal"`
	CaseVersion  int64      `json:"caseVersion"`
	CurrentRound int        `json:"currentRound"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	TerminalAt   *time.Time `json:"terminalAt,omitempty"`
}

type summaryView struct {
	Investigation identityView `json:"investigation"`
	Round         *roundView   `json:"currentRound,omitempty"`
	Outcome       *outcomeView `json:"outcome,omitempty"`
	Counts        countsView   `json:"counts"`
	Spend         spendView    `json:"spend"`
}

// roundView is one bounded execution with the case pack's pinned inputs: the brief it was oriented
// by, the controls it ran under, the plan it meant to follow, and the components that produced it.
// Together they are what makes "why did this round stop after two requests" answerable from the
// case alone.
type roundView struct {
	ID         string       `json:"id"`
	Ordinal    int          `json:"ordinal"`
	Outcome    string       `json:"outcome,omitempty"`
	Brief      *briefView   `json:"brief,omitempty"`
	Controls   controlsView `json:"controls"`
	Plan       planView     `json:"plan"`
	Versions   versionsView `json:"versions"`
	Spend      spendView    `json:"spend"`
	StartedAt  time.Time    `json:"startedAt"`
	TerminalAt *time.Time   `json:"terminalAt,omitempty"`
}

type controlsView struct {
	MaxRequests           int      `json:"maxRequests"`
	MaxAdaptivePasses     int      `json:"maxAdaptivePasses"`
	MaxResultBytes        int64    `json:"maxResultBytes"`
	DeadlineSeconds       int      `json:"deadlineSeconds"`
	RequestTimeoutSeconds int      `json:"requestTimeoutSeconds"`
	MaxMicroCents         int64    `json:"maxMicroCents,omitempty"`
	PermittedCapabilities []string `json:"permittedCapabilities,omitempty"`
}

type planView struct {
	Template string            `json:"template"`
	Intended []plannedReadView `json:"intended"`
}

type plannedReadView struct {
	CapabilityID      string `json:"capabilityId"`
	CapabilityVersion uint32 `json:"capabilityVersion"`
	Purpose           string `json:"purpose"`
}

type versionsView struct {
	Planner       string `json:"planner"`
	Model         string `json:"model"`
	PromptVersion string `json:"promptVersion"`
	SchemaVersion string `json:"schemaVersion"`
	Investigator  string `json:"investigator"`
}

type briefView struct {
	Resource      resourceView        `json:"resource"`
	RecentChanges []changeView        `json:"recentChanges"`
	Topology      []topologyView      `json:"topology"`
	Available     []capabilityRefView `json:"availableCapabilities"`
	Coverage      []coverageView      `json:"coverage"`
	AssembledAt   time.Time           `json:"assembledAt"`
}

type resourceView struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// UID is the cluster's own identity. Without it a workload deleted and recreated inside the
	// window reads as one continuous thing that briefly went unready.
	UID                string    `json:"uid,omitempty"`
	DesiredReplicas    int32     `json:"desiredReplicas"`
	ReadyReplicas      int32     `json:"readyReplicas"`
	UpdatedReplicas    int32     `json:"updatedReplicas"`
	AvailableReplicas  int32     `json:"availableReplicas"`
	Generation         int64     `json:"generation"`
	ObservedGeneration int64     `json:"observedGeneration"`
	ContainerImages    []string  `json:"containerImages,omitempty"`
	CreatedAt          time.Time `json:"createdAt,omitzero"`
	Resolved           bool      `json:"resolved"`
}

// changeView carries the evidence behind it, because a change on the brief is the first thing an
// engineer acts on and it must not be the one uncited statement in the case.
type changeView struct {
	At       time.Time `json:"at"`
	Summary  string    `json:"summary"`
	Evidence string    `json:"evidenceId"`
}

type topologyView struct {
	Pod      string `json:"pod"`
	Node     string `json:"node,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Ready    bool   `json:"ready"`
	Evidence string `json:"evidenceId"`
}

type capabilityRefView struct {
	CapabilityID      string `json:"capabilityId"`
	CapabilityVersion uint32 `json:"capabilityVersion"`
}

// coverageView is one typed capability's readiness. A state and a reason, never a percentage: a
// percentage over unlike capabilities is a number nobody can act on and every reader takes for a
// score.
type coverageView struct {
	CapabilityID      string `json:"capabilityId"`
	CapabilityVersion uint32 `json:"capabilityVersion"`
	State             string `json:"state"`
	Reason            string `json:"reason"`
	Evidence          int    `json:"evidence"`
	// IsGap says whether this state is something missing. Not-applicable is not, and a client that
	// had to work that out from the state name would eventually work it out wrongly.
	IsGap bool `json:"isGap"`
}

// outcomeView is the answer with what it rests on. Every claim carries the identifiers of the
// evidence supporting it, so checking one is a lookup rather than a search.
type outcomeView struct {
	ID        string `json:"id"`
	Round     int    `json:"round"`
	Kind      string `json:"kind"`
	Statement string `json:"statement"`
	// Supporting, Contradicting and AffectedScope are the same shape because they are the same
	// thing: a cited statement. Affected scope is not a set of numbers.
	Supporting         []claimView `json:"supporting"`
	Contradicting      []claimView `json:"contradicting"`
	AffectedScope      []claimView `json:"affectedScope"`
	RelevantGaps       []string    `json:"relevantCoverageGapIds"`
	UnresolvedIDs      []string    `json:"unresolvedHypothesisIds"`
	IndependentSources int         `json:"independentSources"`
	Superseded         bool        `json:"superseded"`
	ReachedAt          time.Time   `json:"reachedAt"`
}

type claimView struct {
	ID        string   `json:"id"`
	Ordinal   int      `json:"ordinal"`
	Statement string   `json:"statement"`
	Evidence  []string `json:"evidenceIds"`
}

// evidenceView is one item as a listing shows it: without its content, which is fetched per item.
type evidenceView struct {
	ID                string `json:"id"`
	Ordinal           int    `json:"ordinal"`
	RoundID           string `json:"roundId"`
	RequestID         string `json:"requestId,omitempty"`
	CapabilityID      string `json:"capabilityId"`
	CapabilityVersion uint32 `json:"capabilityVersion"`
	// Source is the Connection this came through, which is what a reader filters by.
	Source    string `json:"sourceConnectionId"`
	Statement string `json:"statement"`
	// Content is present only on the single-item read. A listing carrying it would be the size of
	// its contents.
	Content string `json:"content,omitempty"`
	Absence bool   `json:"absence"`
	// Trust is how completeness was established. A relay-attested claim is never promoted, and a
	// reader can see which it is.
	Trust       string           `json:"trust"`
	Certificate *certificateView `json:"completenessCertificate,omitempty"`
	// SourceObservedAt is absent when the source gave no defensible time, which is exactly when the
	// item is listed beside the timeline rather than placed on it.
	SourceObservedAt *time.Time `json:"sourceObservedAt,omitempty"`
	OnTimeline       bool       `json:"onTimeline"`
	ReceivedAt       time.Time  `json:"receivedAt"`
}

type certificateView struct {
	SearchedScope      string `json:"searchedScope,omitempty"`
	PaginationComplete bool   `json:"paginationComplete"`
	FullyAuthorized    bool   `json:"fullyAuthorized"`
	SourceFreshnessMS  int64  `json:"sourceFreshnessMs,omitempty"`
	AttestedBy         string `json:"attestedBy"`
	// Certifies says whether an absence may rest on this. Without it a client would have to
	// reimplement the rule, and the one place that rule lives is the point of the type.
	Certifies bool `json:"certifiesAbsence"`
}

type hypothesisView struct {
	ID      string `json:"id"`
	RoundID string `json:"roundId"`
	// Ordinal is the planner's ranking as an ORDINAL. There is deliberately no score: an internal
	// ranking may order these, and publishing the number would hand a reader a confidence figure.
	Ordinal        int       `json:"ordinal"`
	Statement      string    `json:"statement"`
	Falsifies      string    `json:"falsifies"`
	State          string    `json:"state"`
	SetAsideReason string    `json:"setAsideReason,omitempty"`
	ProposedAt     time.Time `json:"proposedAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type gapView struct {
	ID           string `json:"id"`
	Ordinal      int    `json:"ordinal"`
	RoundID      string `json:"roundId"`
	Cause        string `json:"cause"`
	CapabilityID string `json:"capabilityId,omitempty"`
	Subject      string `json:"subject"`
	// Consequence is what could not be concluded because of it. A gap without one tells a reader
	// something is missing without telling them what it cost.
	Consequence string    `json:"consequence"`
	RecordedAt  time.Time `json:"recordedAt"`
}

// activityView is one read the case asked for, with the hypothesis that justified it. Requests
// that returned nothing useful are here with everything else: evidence selection is scored
// independently of the conclusion, which is only possible if what was asked and why survives.
type activityView struct {
	ID                string `json:"id"`
	Ordinal           int    `json:"ordinal"`
	RoundID           string `json:"roundId"`
	Pass              int    `json:"pass"`
	CapabilityID      string `json:"capabilityId"`
	CapabilityVersion uint32 `json:"capabilityVersion"`
	// JustifyingHypothesisID is empty only for the opening plan's reads, which precede every
	// hypothesis.
	JustifyingHypothesisID string     `json:"justifyingHypothesisId,omitempty"`
	Reason                 string     `json:"reason,omitempty"`
	State                  string     `json:"state"`
	Refusal                string     `json:"refusal,omitempty"`
	ResultBytes            int64      `json:"resultBytes"`
	ProposedAt             time.Time  `json:"proposedAt"`
	SettledAt              *time.Time `json:"settledAt,omitempty"`
}

// sectionView is a page of a section with the case version it represents, so a client can tell a
// stale section from a current one without guessing.
type sectionView[T any] struct {
	Items       []T    `json:"items"`
	Next        string `json:"next,omitempty"`
	CaseVersion int64  `json:"caseVersion"`
}

type rowView struct {
	Investigation identityView `json:"investigation"`
	Counts        countsView   `json:"counts"`
	Spend         spendView    `json:"spend"`
	// Severity as its trigger source stated it, with the attribution intact. It is a secondary
	// signal for ordering and never a control.
	Severity       string `json:"severity,omitempty"`
	SeveritySource string `json:"severitySource,omitempty"`
	// Outcome is the case's present tense, precomputed so rendering a list is one request.
	OutcomeKind      string `json:"outcomeKind,omitempty"`
	OutcomeStatement string `json:"outcomeStatement,omitempty"`
}

type listView struct {
	Investigations []rowView `json:"investigations"`
	Next           string    `json:"next,omitempty"`
}

// caseFileView is the whole case at a pinned version, assembled server-side. The shared route,
// both export formats and the harness artifact all take this, so a share, an export and a scored
// artifact are the same bytes.
type caseFileView struct {
	Investigation identityView     `json:"investigation"`
	CaseVersion   int64            `json:"caseVersion"`
	Rounds        []roundView      `json:"rounds"`
	Hypotheses    []hypothesisView `json:"hypotheses"`
	Stances       []stanceView     `json:"stances"`
	Evidence      []evidenceView   `json:"evidence"`
	Timeline      []evidenceView   `json:"timeline"`
	Gaps          []gapView        `json:"coverageGaps"`
	Activity      []activityView   `json:"activity"`
	Coverage      []coverageView   `json:"coverage"`
	Outcomes      []outcomeView    `json:"outcomes"`
}

type stanceView struct {
	HypothesisID string    `json:"hypothesisId"`
	EvidenceID   string    `json:"evidenceId"`
	Stance       string    `json:"stance"`
	Reason       string    `json:"reason"`
	RecordedAt   time.Time `json:"recordedAt"`
}
