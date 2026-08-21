package investigation

// COMPACTION, AND WHY NO MODEL IS ASKED TO DO IT.
//
// When a conversation's estimated context crosses its budget, older turns are replaced in
// the ASSEMBLED CONTEXT by the next version of the running summary. Nothing in the
// authoritative transcript is edited or deleted; what changes is only what the next turn is
// handed.
//
// The summary is composed HERE, deterministically, from facts the platform already holds.
// A summarising model call was the obvious design and is the wrong one for this job:
//
//   - The contract is "a summary may only restate findings that already exist with the
//     citations they already carry". A function that copies structured findings cannot
//     break that rule. A model asked to summarise can, and the failure — a claim whose
//     citation no longer supports it — is exactly the thing the citation invariant exists
//     to make impossible.
//   - An operator's constraint is kept VERBATIM. "Ignore the database, look at deployments"
//     paraphrased is an instruction somebody did not give, and the one thing a long
//     conversation must not lose is what it was told.
//   - It costs nothing and cannot fail, so compaction never turns into a second way for a
//     turn to die.
//
// What is lost by not asking a model is prose quality in a document no operator reads. The
// summary is working memory, and the transcript beside it is the record.

// compact builds the next summary version from the brief, folding the previous one in
// rather than stacking a second beside it.
//
// Nothing here invents a statement. Every finding is copied with its kind, confidence and
// run ordinals; every constraint is a person's own words; every identifier came from a run
// that actually happened.
func compact(brief Brief, throughSequence int64) Summary {
	previous := brief.Summary
	next := Summary{
		Version:       previous.Version + 1,
		CoversThrough: throughSequence,
		Goal:          previous.Goal,
		Problem:       previous.Problem,
	}
	if next.Goal == "" {
		next.Goal = brief.Subject
	}

	// The operator's own words, oldest first, so the constraint stated at the start of a
	// long incident is the last thing to fall off rather than the first.
	next.Constraints = boundedStrings(
		append(append([]string(nil), previous.Constraints...),
			personSaid(brief.Recent)...),
		BriefMaxConstraints)

	established := append(append([]PriorFinding(nil), previous.Established...),
		previous.RuledOut...)
	established = append(established, previous.Open...)
	established = append(established, brief.Findings...)
	next.Established, next.RuledOut, next.Open = sortFindings(established)

	next.FailedReads = boundedStrings(
		append(append([]string(nil), previous.FailedReads...), brief.FailedReads...),
		BriefMaxConstraints)
	next.Recommended = boundedStrings(
		append(append([]string(nil), previous.Recommended...), brief.Recommended...),
		BriefMaxConstraints)
	next.Identifiers = boundedStrings(
		append(append([]string(nil), previous.Identifiers...), brief.Identifiers...),
		BriefMaxIdentifiers)
	return next
}

// sortFindings files each finding under what it is for. A ruled-out hypothesis and an
// unresolved lead are the two a long conversation loses first and can least afford to: one
// makes the agent loop back to a dead end after ten turns, the other makes it forget the
// question it never answered.
func sortFindings(findings []PriorFinding) (established, ruledOut, open []PriorFinding) {
	seen := map[string]bool{}
	for _, finding := range findings {
		if finding.Statement == "" {
			continue
		}
		// Deduplicated by what is said and where it came from, so folding the previous
		// summary in does not accumulate the same fact once per compaction.
		key := finding.Statement + "|" + finding.Reference()
		if seen[key] {
			continue
		}
		seen[key] = true
		switch finding.Kind {
		case FindingRuledOut:
			ruledOut = append(ruledOut, finding)
		case FindingUnresolvedLead:
			open = append(open, finding)
		default:
			established = append(established, finding)
		}
	}
	return boundedFindings(established), boundedFindings(ruledOut), boundedFindings(open)
}

// personSaid is the operator's own messages, in order. What the agent answered is not kept:
// its findings are, with their citations, and the prose it wrapped them in is not evidence.
func personSaid(recent []BriefMessage) []string {
	var said []string
	for _, message := range recent {
		if !message.FromPerson || message.Text == "" {
			continue
		}
		said = append(said, bounded(message.Text, BriefMessageBound))
	}
	return said
}

// boundedFindings keeps the NEWEST when there are too many — the highest turn ordinals —
// because a conversation that has established forty things has usually moved on from the
// first few.
func boundedFindings(findings []PriorFinding) []PriorFinding {
	if len(findings) <= BriefMaxFindings {
		return findings
	}
	return findings[len(findings)-BriefMaxFindings:]
}

// boundedStrings keeps the OLDEST when there are too many, and drops exact repeats. The
// opposite of the findings rule, and deliberately: an instruction given at the start of an
// incident holds for the whole of it, and dropping it in favour of a newer one is how a
// conversation forgets what it was told.
func boundedStrings(values []string, limit int) []string {
	kept := make([]string, 0, min(len(values), limit))
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		kept = append(kept, value)
		if len(kept) == limit {
			break
		}
	}
	return kept
}
