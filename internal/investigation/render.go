package investigation

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
)

// Turning what the case holds into what the wire carries.
//
// It is kept apart from the shapes in views.go for the reason the shapes are kept apart from the
// handlers: those are a contract, this is a mapping, and the two change for different reasons. A
// field renamed there breaks a client; a rendering changed here does not.

func identityViewOf(found Investigation) identityView {
	view := identityView{
		ID:            found.ID.String(),
		EnvironmentID: found.Environment.String(),
		EpisodeKey:    found.EpisodeKey,
		Scope: scopeView{
			ConnectionID: found.Scope.Connection.String(),
			Namespace:    found.Scope.Namespace,
			WorkloadKind: found.Scope.WorkloadKind.String(),
			WorkloadName: found.Scope.WorkloadName,
		},
		Window: windowView{Start: found.Window.Start, End: found.Window.End},
		Trigger: triggerView{
			Kind:        found.Trigger.Kind.String(),
			RequestedBy: found.Trigger.RequestedBy,
			At:          found.Trigger.At,
		},
		Lifecycle:    found.Lifecycle.String(),
		Running:      found.Lifecycle.Running(),
		Terminal:     found.Terminal(),
		CaseVersion:  found.CaseVersion,
		CurrentRound: found.CurrentRound,
		CreatedAt:    found.CreatedAt,
		UpdatedAt:    found.UpdatedAt,
	}
	if !found.TerminalAt.IsZero() {
		terminal := found.TerminalAt
		view.TerminalAt = &terminal
	}
	return view
}

func summaryViewOf(summary Summary) summaryView {
	view := summaryView{
		Investigation: identityViewOf(summary.Investigation),
		Counts:        countsViewOf(summary.Counts),
		Spend:         spendViewOf(summary.Spend),
	}
	if summary.CurrentRound.ID != uuid.Nil {
		round := roundViewOf(summary.CurrentRound)
		view.Round = &round
	}
	if summary.Outcome != nil {
		outcome := outcomeViewOf(*summary.Outcome)
		view.Outcome = &outcome
	}
	return view
}

// countsViewOf renames the counts for the wire. It is a conversion rather than a field-by-field
// copy because the two ARE the same shape, and the conversion is what makes a count added to one
// and not the other a compile error instead of a number that is silently always zero.
func countsViewOf(counts SectionCounts) countsView { return countsView(counts) }

func spendViewOf(spend Spend) spendView {
	return spendView{
		Tokens:     spend.Tokens,
		MicroCents: spend.MicroCents,
		DurationMS: spend.Duration.Milliseconds(),
	}
}

func roundViewOf(round Round) roundView {
	view := roundView{
		ID:      round.ID.String(),
		Ordinal: round.Ordinal,
		Controls: controlsView{
			MaxRequests:           round.Controls.MaxRequests,
			MaxAdaptivePasses:     round.Controls.MaxAdaptivePasses,
			MaxResultBytes:        round.Controls.MaxResultBytes,
			DeadlineSeconds:       int(round.Controls.Deadline.Seconds()),
			RequestTimeoutSeconds: int(round.Controls.RequestTimeout.Seconds()),
			MaxMicroCents:         round.Controls.MaxMicroCents,
			PermittedCapabilities: round.Controls.PermittedCapabilities,
		},
		Plan: planView{
			Template: round.Plan.Template,
			Intended: make([]plannedReadView, 0, len(round.Plan.Intended)),
		},
		Versions: versionsView{
			Planner:       round.Versions.Planner,
			Model:         round.Versions.Model,
			PromptVersion: round.Versions.PromptVersion,
			SchemaVersion: round.Versions.SchemaVersion,
			Investigator:  round.Versions.Investigator,
		},
		Spend:     spendViewOf(round.Spend),
		StartedAt: round.StartedAt,
	}
	if round.Outcome != 0 {
		view.Outcome = round.Outcome.String()
	}
	for _, intended := range round.Plan.Intended {
		view.Plan.Intended = append(view.Plan.Intended, plannedReadView(intended))
	}
	if !round.Brief.AssembledAt.IsZero() {
		brief := briefViewOf(round.Brief)
		view.Brief = &brief
	}
	if !round.TerminalAt.IsZero() {
		terminal := round.TerminalAt
		view.TerminalAt = &terminal
	}
	return view
}

func briefViewOf(brief Brief) briefView {
	view := briefView{
		Resource: resourceView{
			Kind:               brief.Resource.Kind,
			Name:               brief.Resource.Name,
			Namespace:          brief.Resource.Namespace,
			UID:                brief.Resource.UID,
			DesiredReplicas:    brief.Resource.DesiredReplicas,
			ReadyReplicas:      brief.Resource.ReadyReplicas,
			UpdatedReplicas:    brief.Resource.UpdatedReplicas,
			AvailableReplicas:  brief.Resource.AvailableReplicas,
			Generation:         brief.Resource.Generation,
			ObservedGeneration: brief.Resource.ObservedGeneration,
			ContainerImages:    brief.Resource.ContainerImages,
			CreatedAt:          brief.Resource.CreatedAt,
			Resolved:           brief.Resource.Resolved,
		},
		RecentChanges: make([]changeView, 0, len(brief.RecentChanges)),
		Topology:      make([]topologyView, 0, len(brief.Topology)),
		Available:     make([]capabilityRefView, 0, len(brief.Available)),
		Coverage:      make([]coverageView, 0, len(brief.Coverage)),
		AssembledAt:   brief.AssembledAt,
	}
	for _, change := range brief.RecentChanges {
		view.RecentChanges = append(view.RecentChanges, changeView{
			At: change.At, Summary: change.Summary, Evidence: change.Evidence.String(),
		})
	}
	for _, fact := range brief.Topology {
		view.Topology = append(view.Topology, topologyView{
			Pod: fact.Pod, Node: fact.Node, Owner: fact.Owner, Phase: fact.Phase,
			Ready: fact.Ready, Evidence: fact.Evidence.String(),
		})
	}
	for _, available := range brief.Available {
		view.Available = append(view.Available, capabilityRefView{
			CapabilityID: available.ID, CapabilityVersion: available.Version,
		})
	}
	for _, coverage := range brief.Coverage {
		view.Coverage = append(view.Coverage, coverageViewOf(coverage))
	}
	return view
}

func coverageViewOf(coverage Coverage) coverageView {
	return coverageView{
		CapabilityID:      coverage.CapabilityID,
		CapabilityVersion: coverage.CapabilityVersion,
		State:             coverage.State.String(),
		Reason:            coverage.Reason,
		Evidence:          coverage.Evidence,
		IsGap:             coverage.State.IsGap(),
	}
}

func outcomeViewOf(outcome Outcome) outcomeView {
	view := outcomeView{
		ID:                 outcome.ID.String(),
		Round:              outcome.Round,
		Kind:               outcome.Kind.String(),
		Statement:          outcome.Statement,
		Supporting:         []claimView{},
		Contradicting:      []claimView{},
		AffectedScope:      []claimView{},
		RelevantGaps:       identifiers(outcome.RelevantGaps),
		UnresolvedIDs:      identifiers(outcome.UnresolvedHypotheses),
		IndependentSources: outcome.IndependentSources,
		Superseded:         outcome.Superseded,
		ReachedAt:          outcome.ReachedAt,
	}
	for _, claim := range outcome.Claims {
		rendered := claimView{
			ID:        claim.ID.String(),
			Ordinal:   claim.Ordinal,
			Statement: claim.Statement,
			Evidence:  identifiers(claim.Evidence),
		}
		switch claim.Role {
		case ClaimSupporting:
			view.Supporting = append(view.Supporting, rendered)
		case ClaimContradicting:
			view.Contradicting = append(view.Contradicting, rendered)
		case ClaimAffectedScope:
			view.AffectedScope = append(view.AffectedScope, rendered)
		}
	}
	return view
}

// evidenceViewOf renders one item. withContent is false for every listing, so a page is never the
// size of what it lists.
func evidenceViewOf(item Item, withContent bool) evidenceView {
	view := evidenceView{
		ID:                item.ID.String(),
		Ordinal:           item.Ordinal,
		RoundID:           item.RoundID.String(),
		CapabilityID:      item.CapabilityID,
		CapabilityVersion: item.CapabilityVersion,
		Source:            item.Connection.String(),
		Statement:         item.Statement,
		Absence:           item.Absence,
		Trust:             item.Trust.String(),
		OnTimeline:        item.OnTimeline(),
		ReceivedAt:        item.ReceivedAt,
	}
	if item.RequestID != uuid.Nil {
		view.RequestID = item.RequestID.String()
	}
	if withContent {
		view.Content = item.Content
	}
	if item.OnTimeline() {
		observed := item.SourceObservedAt
		view.SourceObservedAt = &observed
	}
	if item.Certificate != nil {
		view.Certificate = &certificateView{
			SearchedScope:      item.Certificate.SearchedScope,
			PaginationComplete: item.Certificate.PaginationComplete,
			FullyAuthorized:    item.Certificate.FullyAuthorized,
			SourceFreshnessMS:  item.Certificate.SourceFreshness.Milliseconds(),
			AttestedBy:         item.Certificate.AttestedBy.String(),
			Certifies:          item.Certificate.Certifies(),
		}
	}
	return view
}

func hypothesisViewOf(hypothesis Hypothesis) hypothesisView {
	return hypothesisView{
		ID:             hypothesis.ID.String(),
		RoundID:        hypothesis.RoundID.String(),
		Ordinal:        hypothesis.Ordinal,
		Statement:      hypothesis.Statement,
		Falsifies:      hypothesis.Falsifies,
		State:          hypothesis.State.String(),
		SetAsideReason: hypothesis.SetAsideReason,
		ProposedAt:     hypothesis.ProposedAt,
		UpdatedAt:      hypothesis.UpdatedAt,
	}
}

func gapViewOf(gap Gap) gapView {
	return gapView{
		ID:           gap.ID.String(),
		Ordinal:      gap.Ordinal,
		RoundID:      gap.RoundID.String(),
		Cause:        gap.Cause.String(),
		CapabilityID: gap.CapabilityID,
		Subject:      gap.Subject,
		Consequence:  gap.Consequence,
		RecordedAt:   gap.RecordedAt,
	}
}

// activityViewOf renders one read. The encoded arguments are deliberately NOT on the wire: they
// are a protocol payload, they are recoverable from the case pack for replay, and a read model
// that published them would be publishing something no reader can check and every scanner would
// have to treat as opaque.
func activityViewOf(request Request) activityView {
	view := activityView{
		ID:                request.ID.String(),
		Ordinal:           request.Ordinal,
		RoundID:           request.RoundID.String(),
		Pass:              request.Pass,
		CapabilityID:      request.CapabilityID,
		CapabilityVersion: request.CapabilityVersion,
		Reason:            request.Reason,
		State:             request.State.String(),
		ResultBytes:       request.ResultBytes,
		ProposedAt:        request.ProposedAt,
	}
	if request.Justification != uuid.Nil {
		view.JustifyingHypothesisID = request.Justification.String()
	}
	if request.Refusal != 0 {
		view.Refusal = request.Refusal.String()
	}
	if !request.SettledAt.IsZero() {
		settled := request.SettledAt
		view.SettledAt = &settled
	}
	return view
}

func rowViewOf(row Row) rowView {
	view := rowView{
		Investigation:    identityViewOf(row.Investigation),
		Counts:           countsViewOf(row.Counts),
		Spend:            spendViewOf(row.Spend),
		Severity:         row.Severity,
		SeveritySource:   row.SeveritySource,
		OutcomeStatement: row.OutcomeStatement,
	}
	if row.OutcomeKind != 0 {
		view.OutcomeKind = row.OutcomeKind.String()
	}
	return view
}

func caseFileViewOf(file CaseFile) caseFileView {
	view := caseFileView{
		Investigation: identityViewOf(file.Investigation),
		CaseVersion:   file.CaseVersion,
		Rounds:        make([]roundView, 0, len(file.Rounds)),
		Hypotheses:    make([]hypothesisView, 0, len(file.Hypotheses)),
		Stances:       make([]stanceView, 0, len(file.Stances)),
		Evidence:      make([]evidenceView, 0, len(file.Evidence)),
		Timeline:      make([]evidenceView, 0, len(file.Timeline)),
		Gaps:          make([]gapView, 0, len(file.Gaps)),
		Activity:      make([]activityView, 0, len(file.Requests)),
		Coverage:      make([]coverageView, 0, len(file.Coverage)),
		Outcomes:      make([]outcomeView, 0, len(file.Outcomes)),
	}
	for _, round := range file.Rounds {
		view.Rounds = append(view.Rounds, roundViewOf(round))
	}
	for _, hypothesis := range file.Hypotheses {
		view.Hypotheses = append(view.Hypotheses, hypothesisViewOf(hypothesis))
	}
	for _, stance := range file.Stances {
		view.Stances = append(view.Stances, stanceView{
			HypothesisID: stance.HypothesisID.String(),
			EvidenceID:   stance.EvidenceID.String(),
			Stance:       stance.Stance.String(),
			Reason:       stance.Reason,
			RecordedAt:   stance.RecordedAt,
		})
	}
	// The assembled case file carries content: it IS the export, and an export whose evidence has
	// to be fetched item by item is not one.
	for _, item := range file.Evidence {
		view.Evidence = append(view.Evidence, evidenceViewOf(item, true))
	}
	for _, item := range file.Timeline {
		view.Timeline = append(view.Timeline, evidenceViewOf(item, true))
	}
	for _, gap := range file.Gaps {
		view.Gaps = append(view.Gaps, gapViewOf(gap))
	}
	for _, request := range file.Requests {
		view.Activity = append(view.Activity, activityViewOf(request))
	}
	for _, coverage := range file.Coverage {
		view.Coverage = append(view.Coverage, coverageViewOf(coverage))
	}
	for _, outcome := range file.Outcomes {
		view.Outcomes = append(view.Outcomes, outcomeViewOf(outcome))
	}
	return view
}

func identifiers(values []uuid.UUID) []string {
	rendered := make([]string, 0, len(values))
	for _, value := range values {
		rendered = append(rendered, value.String())
	}
	return rendered
}

// decode reads a bounded request body, refusing a field this build does not know rather than
// ignoring it: an engineer who misspelled one should be told, not left believing they scoped an
// investigation they did not.
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
// anything in front of it: every response carries a named tenant's investigation, and evidence text
// from a customer's own systems travels inside it.
func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
