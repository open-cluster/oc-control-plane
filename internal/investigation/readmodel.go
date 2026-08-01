package investigation

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// ONE LOGICAL AGGREGATE, SEVERAL TRANSPORT SHAPES, ONE VERSION GOVERNING ALL OF THEM.
//
// One aggregate means one case identity, one permission boundary, one versioning model and one
// vocabulary. It does NOT mean one response carrying every section: a case with hundreds of rounds
// would make that unbounded. So there is a small summary a client polls, several paginated sections
// it fetches only when they are opened, and one server-side assembly of the whole thing at a pinned
// version.
//
// The assembly exists so that a shared link, an export and the scenario harness's scored artifact
// are the same bytes. It is tempting to let the browser compose a case file from the sections it
// already holds, and it works until the first share link renders differently from the page that
// produced it.
//
// These types live here rather than in the persistence package because the capability owns its
// vocabulary and persistence reconstructs it (ADR-017). It is also what lets this package serve its
// own routes without importing storage.

// ErrBadCursor reports a resume point that did not come from a previous page. It is this package's
// own rather than the persistence package's, for the same reason the types around it are: the
// handler that answers it belongs here.
var ErrBadCursor = errors.New("cursor is not a page position")

// ErrCaseMoved reports an assembly asked for a version the case has already passed.
var ErrCaseMoved = errors.New("the case has moved past the version asked for")

// ErrCaseTooLarge reports a case with more evidence than one assembly may carry.
var ErrCaseTooLarge = errors.New("the case is larger than one assembly may carry")

// Page is which slice of a section to return.
type Page struct {
	// Limit is how many to return. Zero means the default rather than none.
	Limit int
	// After resumes from a previous page's Next.
	After string
}

// Section is a page of one of a case's collections, stamped with the case version it represents.
//
// The version is ON the page rather than beside it because a client holds pages, not requests. A
// client that refreshes the summary and finds a newer version can then tell which of the sections
// it is holding are stale, and mark them rather than fetching them — an unopened tab costs nothing.
type Section[T any] struct {
	Items       []T
	Next        string
	CaseVersion int64
}

// SectionCounts is how many of each thing a case holds, so a tab can be labelled without fetching
// its contents.
type SectionCounts struct {
	Rounds     int
	Evidence   int
	Timeline   int
	Gaps       int
	Hypotheses int
	Requests   int
	Outcomes   int
}

// Summary is everything needed to render the top of a case in one request, and the only thing a
// client polls.
type Summary struct {
	Investigation Investigation
	// CurrentRound is the round in progress, or the last one to run, with its brief, its resolved
	// controls and the component versions it ran under.
	CurrentRound Round
	// Outcome is the case's present tense — nil while it has reached none. Superseded outcomes are
	// not here; they are read from the case file, where their round and time are visible.
	Outcome *Outcome
	Counts  SectionCounts
	Spend   Spend
}

// Row is one case as a listing shows it: everything a row renders, with no second request per row.
type Row struct {
	Investigation Investigation
	Counts        SectionCounts
	Spend         Spend
	// Severity as its trigger source stated it, with the attribution. Empty for a manual start,
	// because nobody stated one. A secondary sort signal and never a control.
	Severity       string
	SeveritySource string
	// OutcomeKind and OutcomeStatement are the case's present tense, precomputed into the row.
	OutcomeKind      OutcomeKind
	OutcomeStatement string
}

// List is a page of an organization's cases.
type List struct {
	Rows []Row
	Next string
}

// ListFilter narrows a listing.
type ListFilter struct {
	// Environment restricts to one scope. Zero means every Environment this organization has.
	Environment uuid.UUID
	// Running restricts to cases a worker currently holds, which is the distinction between a
	// quiet case and a stalled one.
	Running bool
}

// EvidenceFilter narrows an evidence listing. A case with hundreds of items is otherwise a scroll
// rather than something navigable.
type EvidenceFilter struct {
	CapabilityID string
	// Source selects items that came through one Connection.
	Source uuid.UUID
	// Stance selects items holding a stance towards some hypothesis in this case.
	Stance Stance
}

// CaseFile is the complete case at a pinned version, assembled in one pass.
//
// It carries no assembly timestamp. Two assemblies at one pinned version must be identical, and a
// clock inside the content would make them differ while claiming to be the same.
type CaseFile struct {
	Investigation Investigation
	CaseVersion   int64
	// Rounds are named so a document read next quarter carries its own scope rather than implying
	// it covers whatever the case became.
	Rounds     []Round
	Hypotheses []Hypothesis
	Stances    []Weighed
	Evidence   []Item
	Timeline   []Item
	Gaps       []Gap
	Requests   []Request
	Coverage   []Coverage
	// Outcomes is every outcome this case reached, newest first, with the superseded ones still in
	// place and attributed to their round.
	Outcomes []Outcome
}

// Reader is the read side of a case, as the routes in this package need it.
//
// It is separate from Store because the two have different callers and different obligations: a
// runner writes under a fence and a reader never writes at all. A handler given the writing
// interface is a handler one typo away from mutating what it was asked to display.
type Reader interface {
	// CaseVersion reads only the version, which is what a conditional request needs. It is its own
	// read so that "nothing has changed" stays the cheap answer — assembling a summary and
	// discarding it would make the cheap answer the expensive one.
	CaseVersion(ctx context.Context, org tenancy.Organization, id uuid.UUID) (int64, error)
	InvestigationSummary(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Summary, error)
	ListInvestigations(ctx context.Context, org tenancy.Organization, filter ListFilter, page Page) (List, error)

	InvestigationTimeline(ctx context.Context, org tenancy.Organization, id uuid.UUID, page Page) (Section[Item], error)
	InvestigationEvidence(ctx context.Context, org tenancy.Organization, id uuid.UUID, filter EvidenceFilter, page Page) (Section[Item], error)
	EvidenceItem(ctx context.Context, org tenancy.Organization, id, evidenceID uuid.UUID) (Item, int64, error)
	InvestigationHypotheses(ctx context.Context, org tenancy.Organization, id uuid.UUID, page Page) (Section[Hypothesis], error)
	InvestigationGaps(ctx context.Context, org tenancy.Organization, id uuid.UUID, page Page) (Section[Gap], error)
	InvestigationActivity(ctx context.Context, org tenancy.Organization, id uuid.UUID, page Page) (Section[Request], error)
	InvestigationCoverage(ctx context.Context, org tenancy.Organization, id uuid.UUID) (Section[Coverage], error)

	// AssembleCaseFile builds the whole case at a pinned version, server-side, in one pass. Zero
	// pins whatever the case is at now.
	AssembleCaseFile(ctx context.Context, org tenancy.Organization, id uuid.UUID, pinned int64) (CaseFile, error)
}
