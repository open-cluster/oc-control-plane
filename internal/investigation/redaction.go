package investigation

import (
	"fmt"
	"strings"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// What a customer's own redaction policy withheld, and what that costs this investigation.
//
// Masking arrives as counts and rule identifiers — never a value, never a digest, never a
// fragment — and it becomes a declared CoverageGap rather than an empty field. That distinction
// is the whole reason the report exists: if a masked field were indistinguishable from an absent
// one, a customer's privacy policy would quietly degrade their investigations and the product
// would be blamed for the hole.

// Masking is one read's redaction report, in this package's terms.
type Masking struct {
	fields []maskedField
}

type maskedField struct {
	path        string
	occurrences uint32
	rules       []string
}

// MaskingFrom reads what the Relay reported. An absent report means nothing was masked; it never
// means redaction did not run, because a result that failed to pass the Relay's enforcement point
// is never serialized at all.
func MaskingFrom(report *relayv1.RedactionReport) Masking {
	if report == nil {
		return Masking{}
	}
	masking := Masking{fields: make([]maskedField, 0, len(report.GetFields()))}
	for _, field := range report.GetFields() {
		if field.GetMaskedOccurrenceCount() == 0 {
			continue
		}
		masking.fields = append(masking.fields, maskedField{
			path:        field.GetFieldName(),
			occurrences: field.GetMaskedOccurrenceCount(),
			rules:       field.GetRuleIds(),
		})
	}
	return masking
}

// Fields names what was masked, in the customer's own terms. It goes onto the completeness
// certificate, where it stops an absence being certified over a scope part of which nobody here
// was allowed to see.
func (m Masking) Fields() []string {
	if len(m.fields) == 0 {
		return nil
	}
	named := make([]string, 0, len(m.fields))
	for _, field := range m.fields {
		named = append(named, subjectOf(field.path))
	}
	return named
}

// Gaps turns each masked field into something an engineer can act on: what was withheld, which
// rule withheld it, and what cannot be concluded because of it.
//
// The consequence is the load-bearing half. A gap that says a field was masked and stops there
// tells a reader that something is missing without telling them what it cost, which is an
// apology rather than a finding.
func (m Masking) Gaps(request Request) []Gap {
	if len(m.fields) == 0 {
		return nil
	}
	gaps := make([]Gap, 0, len(m.fields))
	for _, field := range m.fields {
		gaps = append(gaps, Gap{
			Cause:        GapRedactionMasked,
			CapabilityID: request.CapabilityID,
			Subject:      subjectOf(field.path),
			Consequence: fmt.Sprintf(
				"this organization's own redaction policy masked %s here (%s), so what it said "+
					"cannot be weighed and its absence cannot be concluded either; the text is in "+
					"the source and was withheld on the way out, not missing from it",
				occurrences(field.occurrences), rules(field.rules)),
		})
	}
	return gaps
}

// subjects translates a contract field path into what a person would call it. A path is what the
// Relay reports and is exactly right for a policy file; it is the wrong vocabulary for an
// engineer reading a coverage gap at 03:00.
var subjects = map[string]string{
	"kubernetes_container_logs_v1.lines.content":    "the container's own log text",
	"kubernetes_namespace_events_v1.events.message": "the cluster's account of what it did",
}

// subjectOf names a masked field for a reader, falling back to the contract's own path. The
// fallback is deliberate: a field this build has no phrase for is still reported, because a gap
// nobody translated is worth incomparably more than a gap nobody recorded.
func subjectOf(path string) string {
	if named, known := subjects[path]; known {
		return named
	}
	return path
}

func occurrences(count uint32) string {
	if count == 1 {
		return "one value"
	}
	return fmt.Sprintf("%d values", count)
}

// rules names what matched, so a reader can ask the right person to adjust the right rule rather
// than guessing which pattern was too broad.
func rules(identifiers []string) string {
	switch len(identifiers) {
	case 0:
		return "rule unreported"
	case 1:
		return "rule " + identifiers[0]
	default:
		return "rules " + strings.Join(identifiers, ", ")
	}
}
