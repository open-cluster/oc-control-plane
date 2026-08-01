package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// The read models, asserted through the assembled process.
//
// Every test here runs in parallel. Each owns its own database, its own control plane on ephemeral
// ports, its own relay and its own investigator, so they share nothing but the Postgres server —
// and in sequence the package came within a minute of the default test timeout, which is a suite
// that starts failing for a reason that has nothing to do with the code.
//
// What is asserted is what a consumer can observe: the version advanced when evidence arrived, a
// conditional read answered unchanged, a superseded outcome is still readable, a request naming
// another organization's investigation was refused. Nothing here asserts query shape or table
// layout, because those will change when the case grows.

// One request renders the top of a case, and polling it when nothing has changed costs one
// primary-key read.
func TestInvestigationRead_TheSummaryIsOneRequestAndPollingItIsCheap(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)

	// Everything needed for a first paint, in one answer.
	if summary.Investigation.ID == "" || summary.Investigation.Lifecycle == "" ||
		summary.CurrentRound == nil || summary.Outcome == nil {
		t.Fatalf("the summary must carry identity, state, the current round and the outcome, "+
			"got %+v", summary)
	}
	if summary.Counts.Evidence == 0 || summary.Counts.Hypotheses == 0 ||
		summary.Counts.Activity == 0 || summary.Counts.Rounds != 1 {
		t.Errorf("the summary must carry per-section counts so tabs can be labelled without "+
			"fetching them, got %+v", summary.Counts)
	}
	if summary.Investigation.CaseVersion == 0 {
		t.Error("the summary must carry the version that governs the whole case")
	}

	// The version travels as an entity tag, and a conditional request holding it is answered
	// "unchanged" without assembling anything.
	status, _, headers := plane.call(t, http.MethodGet,
		plane.base()+"/"+summary.Investigation.ID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reading the summary = %d", status)
	}
	tag := headers.Get("ETag")
	if versionOf(t, tag) != summary.Investigation.CaseVersion {
		t.Errorf("the tag %q does not carry the case version %d",
			tag, summary.Investigation.CaseVersion)
	}

	status, body, _ := plane.call(t, http.MethodGet, plane.base()+"/"+summary.Investigation.ID,
		nil, map[string]string{"If-None-Match": tag})
	if status != http.StatusNotModified {
		t.Errorf("a conditional read at the current version = %d: %s; want unchanged",
			status, body)
	}
	if body != "" {
		t.Errorf("an unchanged answer must carry no body, got %q", body)
	}

	// A version the client does not hold is answered in full.
	status, _, _ = plane.call(t, http.MethodGet, plane.base()+"/"+summary.Investigation.ID,
		nil, map[string]string{"If-None-Match": `"1"`})
	if status != http.StatusOK {
		t.Errorf("a conditional read at an older version = %d, want the summary", status)
	}
}

// Every section carries the case version it represents, so a client can tell a stale section from
// a current one. Evidence content is fetched per item, so a listing is not the size of its
// contents.
func TestInvestigationRead_SectionsAreStampedAndContentIsFetchedSeparately(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)
	id := summary.Investigation.ID

	var evidence evidenceSectionBody
	plane.section(t, id, "evidence", &evidence)
	if evidence.CaseVersion != summary.Investigation.CaseVersion {
		t.Errorf("the evidence section is stamped %d and the case is at %d",
			evidence.CaseVersion, summary.Investigation.CaseVersion)
	}
	if len(evidence.Items) == 0 {
		t.Fatal("the case must hold evidence")
	}
	for _, item := range evidence.Items {
		if item.Content != "" {
			t.Errorf("a listing must not carry evidence content, item %s does", item.ID)
		}
		// Everything a Relay produced is relay-attested, and it is never promoted.
		if item.Trust != "relay_attested" {
			t.Errorf("item %s is %q; a relay's attestation must not be promoted",
				item.ID, item.Trust)
		}
	}

	// One item, with its content, on demand.
	status, body, headers := plane.call(t, http.MethodGet,
		plane.base()+"/"+id+"/evidence/"+evidence.Items[0].ID, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reading one evidence item = %d: %s", status, body)
	}
	var single evidenceSectionBody
	decodeInto(t, body, &single)
	if len(single.Items) != 1 {
		t.Fatalf("an item read returned %d items", len(single.Items))
	}
	if versionOf(t, headers.Get("ETag")) != summary.Investigation.CaseVersion {
		t.Error("an item read must carry the case version it represents")
	}
	if len(single.Items[0].Content) > investigation.MaxEvidenceContentBytes {
		t.Errorf("content is %d bytes, above the %d bound the read path applies",
			len(single.Items[0].Content), investigation.MaxEvidenceContentBytes)
	}

	// Every other section is stamped too.
	for _, section := range []string{"timeline", "hypotheses", "coverage-gaps", "activity", "coverage"} {
		var stamped struct {
			CaseVersion int64 `json:"caseVersion"`
		}
		plane.section(t, id, section, &stamped)
		if stamped.CaseVersion == 0 {
			t.Errorf("the %s section carries no case version", section)
		}
	}

	// More than one candidate explanation was held, each with what would disprove it, and the
	// planner's ranking is exposed as an ORDINAL. There is no score anywhere: publishing one would
	// hand a reader at 03:00 a confidence figure to skim instead of the basis to check.
	var hypotheses hypothesisSectionBody
	plane.section(t, id, "hypotheses", &hypotheses)
	if len(hypotheses.Items) < 2 {
		t.Fatalf("the case held %d hypotheses; an engineer must not be anchored on the first "+
			"plausible one", len(hypotheses.Items))
	}
	for position, hypothesis := range hypotheses.Items {
		if hypothesis.Falsifies == "" {
			t.Errorf("hypothesis %q carries no falsification condition", hypothesis.Statement)
		}
		if hypothesis.Ordinal != position+1 {
			t.Errorf("hypothesis %d is ranked %d; the ranking is an ordinal",
				position+1, hypothesis.Ordinal)
		}
		if hypothesis.State == "" {
			t.Errorf("hypothesis %q records no state", hypothesis.Statement)
		}
	}

	// A section read before an update is detectably older than a summary read after it.
	before := evidence.CaseVersion
	status, _, _ = plane.call(t, http.MethodPost, plane.base()+"/"+id+"/reinvestigate", nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("reinvestigating = %d", status)
	}
	if after := plane.summary(t, id).Investigation.CaseVersion; after <= before {
		t.Errorf("the case is at %d after a change and the older section was stamped %d; a stale "+
			"section must be detectable", after, before)
	}
}

// Evidence is navigable in a case with many items, which means filterable.
func TestInvestigationRead_EvidenceIsFilterable(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)
	id := summary.Investigation.ID

	var all evidenceSectionBody
	plane.section(t, id, "evidence", &all)

	for _, filter := range []struct {
		name  string
		query string
		want  func(capabilityID string) bool
	}{
		{"by capability", "?capability=kubernetes.container.logs",
			func(capabilityID string) bool { return capabilityID == "kubernetes.container.logs" }},
		{"by source", "?source=" + plane.connection.String(),
			func(string) bool { return true }},
		{"by stance", "?stance=supports",
			func(string) bool { return true }},
	} {
		t.Run(filter.name, func(t *testing.T) {
			status, body, _ := plane.call(t, http.MethodGet,
				plane.base()+"/"+id+"/evidence"+filter.query, nil, nil)
			if status != http.StatusOK {
				t.Fatalf("filtering %s = %d: %s", filter.name, status, body)
			}
			var filtered evidenceSectionBody
			decodeInto(t, body, &filtered)
			if len(filtered.Items) == 0 {
				t.Fatalf("filtering %s returned nothing", filter.name)
			}
			if len(filtered.Items) > len(all.Items) {
				t.Errorf("filtering %s returned more than the whole case holds", filter.name)
			}
			for _, item := range filtered.Items {
				if !filter.want(item.CapabilityID) {
					t.Errorf("filtering %s returned %s", filter.name, item.CapabilityID)
				}
			}
		})
	}

	// A stance nobody has is refused rather than silently returning everything.
	if status, _, _ := plane.call(t, http.MethodGet,
		plane.base()+"/"+id+"/evidence?stance=maybe", nil, nil); status != http.StatusBadRequest {
		t.Errorf("an unknown stance = %d, want it refused", status)
	}
}

// Requests that returned nothing useful are retained with the hypothesis that justified them, so
// evidence selection can be judged apart from the conclusion.
func TestInvestigationRead_ActivityShowsWhatWasAskedAndWhy(t *testing.T) {
	t.Parallel()
	// The container log read comes back empty and complete. It produced no useful finding and must
	// still be in the record.
	answering := healthyCluster()
	answering.logs.Lines = nil
	answering.logs.ReturnedLineCount = 0
	answering.logs.ReturnedByteCount = 0

	transcript := crashLoopTranscript()
	// With no log line there is nothing to cite for the supporting claim, so the recorded
	// conclusion abstains — which is what a real run does when the decisive evidence is missing.
	transcript.Conclusion.Draft = investigation.Draft{
		Kind:       investigation.OutcomeAbstained,
		Statement:  "the container wrote nothing before it exited, so what stopped it is unknown",
		Unresolved: []int{1},
	}
	transcript.Conclusion.Weighings = nil
	transcript.Conclusion.Settlings = nil

	plane := startInvestigationPlane(t, replaying(t, transcript), answering, investigation.Controls{})
	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)

	if summary.Investigation.Lifecycle != "abstained" {
		t.Fatalf("the case is %s, want abstained\nlogs:\n%s",
			summary.Investigation.Lifecycle, plane.logs.String())
	}
	if summary.Outcome == nil || len(summary.Outcome.Unresolved) == 0 {
		t.Error("an abstention must name which hypotheses were left unresolved")
	}

	var activity activitySectionBody
	plane.section(t, summary.Investigation.ID, "activity", &activity)

	var adaptive int
	for _, request := range activity.Items {
		if request.Pass == 0 {
			continue
		}
		adaptive++
		if request.JustifyingHypothesisID == "" {
			t.Errorf("the adaptive read of %s names no justifying hypothesis",
				request.CapabilityID)
		}
		if request.Reason == "" {
			t.Errorf("the adaptive read of %s records no reason", request.CapabilityID)
		}
	}
	if adaptive == 0 {
		t.Fatal("the case must record the adaptive read it made")
	}

	// A complete read that found nothing is a CERTIFIED negative and is usable, which is what makes
	// it distinguishable from a read that never happened.
	var evidence evidenceSectionBody
	plane.section(t, summary.Investigation.ID, "evidence", &evidence)
	var certified bool
	for _, item := range evidence.Items {
		if item.Absence {
			certified = true
			if item.Certificate == nil || !item.Certificate.Certifies {
				t.Errorf("the absence claim %s rests on no completeness certificate", item.ID)
			}
		}
	}
	if !certified {
		t.Error("a complete read that found nothing must produce a usable absence claim")
	}
}

// Coverage is per typed capability with a state and a reason, and a state that is not a gap is not
// reported as one.
func TestInvestigationRead_CoverageIsPerCapabilityWithAReason(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)

	var coverage coverageSectionBody
	plane.section(t, summary.Investigation.ID, "coverage", &coverage)
	if len(coverage.Items) == 0 {
		t.Fatal("a case must report coverage per capability")
	}

	states := map[string]bool{
		"checked": true, "checked_empty": true, "incomplete": true,
		"unavailable": true, "not_applicable": true,
	}
	var checked int
	for _, entry := range coverage.Items {
		if !states[entry.State] {
			t.Errorf("coverage of %s is in state %q, which is not one of the five",
				entry.CapabilityID, entry.State)
		}
		if entry.Reason == "" {
			t.Errorf("coverage of %s records no reason; a state with no basis is a claim",
				entry.CapabilityID)
		}
		// Checked and not-applicable are not gaps. Reporting a capability the stack does not
		// provide as a gap is how a coverage report stops being read.
		if entry.State == "checked" || entry.State == "checked_empty" ||
			entry.State == "not_applicable" {
			if entry.IsGap {
				t.Errorf("coverage of %s is %q and reported as a gap",
					entry.CapabilityID, entry.State)
			}
		}
		if entry.State == "checked" {
			checked++
			if entry.Evidence == 0 {
				t.Errorf("coverage of %s says checked and names no evidence", entry.CapabilityID)
			}
		}
	}
	if checked == 0 {
		t.Error("at least one capability must be reported as checked with evidence")
	}
}

// Reinvestigation adds a round to the SAME case, and the earlier outcome stays readable with its
// round and its time rather than being rewritten.
func TestInvestigationRead_ASupersededOutcomeStaysReadableInTheCaseFile(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	first := plane.awaitTerminal(t, opened.Investigation.ID)
	id := first.Investigation.ID

	status, body, _ := plane.call(t, http.MethodPost, plane.base()+"/"+id+"/reinvestigate", nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("reinvestigating = %d: %s", status, body)
	}
	second := plane.awaitTerminal(t, id)

	// One identity, one permalink. Reinvestigation never creates a second case.
	if second.Investigation.ID != id {
		t.Fatalf("reinvestigation produced case %s, want the same case %s",
			second.Investigation.ID, id)
	}
	if second.Investigation.CurrentRound != 2 {
		t.Errorf("the case is on round %d, want 2", second.Investigation.CurrentRound)
	}

	var file caseFileBody
	plane.section(t, id, "case-file", &file)
	if len(file.Rounds) != 2 {
		t.Fatalf("the case file names %d rounds, want both", len(file.Rounds))
	}
	if len(file.Outcomes) != 2 {
		t.Fatalf("the case file holds %d outcomes, want the superseded one and the current one",
			len(file.Outcomes))
	}

	var superseded, current int
	for _, outcome := range file.Outcomes {
		if outcome.Superseded {
			superseded++
			if outcome.Round != 1 {
				t.Errorf("the superseded outcome is attributed to round %d, want 1", outcome.Round)
			}
			continue
		}
		current++
		if outcome.Round != 2 {
			t.Errorf("the current outcome is attributed to round %d, want 2", outcome.Round)
		}
	}
	if superseded != 1 || current != 1 {
		t.Errorf("%d superseded and %d current outcomes, want one of each", superseded, current)
	}
	// The summary shows only the present tense.
	if second.Outcome == nil || second.Outcome.Superseded {
		t.Error("the summary must carry the outcome nothing has superseded")
	}
}

// One assembly serves the shared link, the export and the harness artifact. Two at one pinned
// version must be identical, and a version the case has passed is refused rather than answered
// from the current state.
func TestInvestigationRead_AssemblyAtAPinnedVersionIsRepeatable(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)
	id := summary.Investigation.ID
	pinned := summary.Investigation.CaseVersion

	first := plane.assemble(t, id, pinned)
	second := plane.assemble(t, id, pinned)

	if first != second {
		t.Error("two assemblies at one pinned version differ")
	}

	var file caseFileBody
	decodeInto(t, first, &file)
	if file.CaseVersion != pinned {
		t.Errorf("the assembly is stamped %d, want the pinned %d", file.CaseVersion, pinned)
	}
	if len(file.Rounds) != 1 {
		t.Errorf("an assembly must name the rounds it includes, got %d", len(file.Rounds))
	}
	// It agrees with the summary and the sections at the same version.
	if len(file.Evidence) != summary.Counts.Evidence ||
		len(file.Gaps) != summary.Counts.Gaps ||
		len(file.Activity) != summary.Counts.Activity {
		t.Errorf("the assembly holds %d evidence, %d gaps and %d activity; the summary counts "+
			"%d, %d and %d", len(file.Evidence), len(file.Gaps), len(file.Activity),
			summary.Counts.Evidence, summary.Counts.Gaps, summary.Counts.Activity)
	}
	// The assembly IS the export, so it carries evidence content. An export whose evidence has to
	// be fetched item by item is not one.
	var withContent int
	for _, item := range file.Evidence {
		if item.Content != "" {
			withContent++
		}
	}
	if withContent == 0 {
		t.Error("an assembled case file must carry evidence content")
	}

	// The case moves, and the old pin is refused rather than answered from the new state.
	if status, _, _ := plane.call(t, http.MethodPost,
		plane.base()+"/"+id+"/reinvestigate", nil, nil); status != http.StatusAccepted {
		t.Fatalf("reinvestigating: %d", status)
	}
	status, body, _ := plane.call(t, http.MethodGet,
		plane.base()+"/"+id+"/case-file?version="+strconv.FormatInt(pinned, 10), nil, nil)
	if status != http.StatusConflict {
		t.Errorf("assembling at a version the case has passed = %d: %s; want a refusal",
			status, body)
	}
}

// The list carries what a row renders, in one request, and nothing on any read model is a secret.
func TestInvestigationRead_TheListCarriesRowsAndNoReadModelLeaksASecret(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)

	status, body, _ := plane.call(t, http.MethodGet, plane.base(), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("listing investigations = %d: %s", status, body)
	}
	var list investigationListBody
	decodeInto(t, body, &list)
	if len(list.Investigations) != 1 {
		t.Fatalf("listed %d cases, want 1", len(list.Investigations))
	}

	row := list.Investigations[0]
	if row.Counts.Evidence != summary.Counts.Evidence || row.Counts.Rounds != 1 {
		t.Errorf("the row counts %+v; a row must carry them without a second request", row.Counts)
	}
	if row.OutcomeKind == "" || row.OutcomeStatement == "" {
		t.Error("a row must carry the case's present tense without a second request")
	}

	// Nothing any read model returns is a credential, a digest, or a field that only exists to
	// hold one. The read surface must not be a disclosure path.
	surfaces := []string{body}
	for _, section := range []string{
		"", "/timeline", "/evidence", "/hypotheses", "/coverage-gaps", "/activity", "/coverage",
		"/case-file",
	} {
		_, answered, _ := plane.call(t, http.MethodGet,
			plane.base()+"/"+summary.Investigation.ID+section, nil, nil)
		surfaces = append(surfaces, answered)
	}
	// The check is over FIELD NAMES rather than over the whole body, because a substring match
	// would flag "tokens" — the model cost figure, which is an operator fact and belongs here — and
	// a check that has to be argued with is one that gets deleted.
	for _, answered := range surfaces {
		for _, forbidden := range []string{
			`"secret"`, `"secretDigest"`, `"digest"`, `"credential"`, `"password"`,
			`"token"`, `"apiKey"`, `"authorization"`, `"bearer"`,
		} {
			if strings.Contains(answered, forbidden+":") {
				t.Errorf("a read model carries the field %s:\n%s", forbidden, answered)
			}
		}
		// And nothing looks like the raw payload a Relay would have withheld. Evidence content is
		// here by design; a credential inside it would be a redaction failure upstream, and this is
		// the last place it could be noticed.
		if strings.Contains(answered, "BEGIN PRIVATE KEY") ||
			strings.Contains(answered, "Bearer ") {
			t.Errorf("a read model carries something credential-shaped:\n%s", answered)
		}
	}
}

// A request naming one organization's identity and another's investigation is refused, with both
// organizations on the same placement so the refusal is the tenant boundary rather than an
// unresolvable placement.
func TestInvestigationRead_ANeighbourCannotReadThisCase(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)
	id := summary.Investigation.ID

	for _, section := range []string{
		"", "/timeline", "/evidence", "/hypotheses", "/coverage-gaps", "/activity", "/coverage",
		"/case-file",
	} {
		url := plane.baseFor(investigationNeighbour) + "/" + id + section
		if status, body, _ := plane.call(t, http.MethodGet, url, nil, nil); status != http.StatusNotFound {
			t.Errorf("a neighbour read %s and got %d: %s; it must be refused",
				plane.base()+"/"+id+section, status, body)
		}
	}

	// And so is acting on it.
	if status, _, _ := plane.call(t, http.MethodPost,
		plane.baseFor(investigationNeighbour)+"/"+id+"/cancel", nil, nil); status != http.StatusNotFound {
		t.Errorf("a neighbour cancelled the case with %d; it must be refused", status)
	}

	// The case is untouched.
	if plane.summary(t, id).Investigation.Lifecycle != summary.Investigation.Lifecycle {
		t.Error("a refused cross-tenant request must change nothing")
	}
}

// A read model's cursor is opaque and a value that did not come from one is refused, rather than
// silently showing the first page again.
func TestInvestigationRead_AnInventedCursorIsRefused(t *testing.T) {
	t.Parallel()
	plane := startInvestigationPlane(t,
		replaying(t, crashLoopTranscript()), healthyCluster(), investigation.Controls{})

	opened := plane.openInvestigation(t, time.Hour)
	summary := plane.awaitTerminal(t, opened.Investigation.ID)

	status, body, _ := plane.call(t, http.MethodGet,
		plane.base()+"/"+summary.Investigation.ID+"/evidence?after=not-a-cursor", nil, nil)
	if status != http.StatusBadRequest {
		t.Errorf("an invented cursor = %d: %s; want it refused", status, body)
	}

	// An investigation identifier that is not one is refused before anything is read.
	if status, _, _ = plane.call(t, http.MethodGet,
		plane.base()+"/not-an-identity", nil, nil); status != http.StatusBadRequest {
		t.Errorf("an identifier that is not one = %d, want it refused", status)
	}
	// One that is an identity but names no case of this organization's is not found.
	if status, _, _ = plane.call(t, http.MethodGet,
		plane.base()+"/"+uuid.NewString(), nil, nil); status != http.StatusNotFound {
		t.Errorf("an unknown case = %d, want not found", status)
	}
}

// assemble reads the case file at a pinned version and returns the raw bytes, so two assemblies can
// be compared as the documents they are rather than as decoded structures.
func (p *investigationPlane) assemble(t *testing.T, id string, pinned int64) string {
	t.Helper()

	status, body, _ := p.call(t, http.MethodGet,
		p.base()+"/"+id+"/case-file?version="+strconv.FormatInt(pinned, 10), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("assembling the case file = %d: %s", status, body)
	}
	return body
}
