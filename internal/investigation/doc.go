// Package investigation owns what an Investigation is and everything it produces: the case,
// its bounded rounds, the brief that orients each one, the hypotheses it holds, the typed
// requests it makes, the EvidenceItems and CoverageGaps those produce, and the outcome it
// reaches or declines to reach.
//
// It owns the vocabulary rather than borrowing it from persistence, which is ADR-017: a domain
// type belongs to the capability that defines its meaning, and these types carry invariants
// rather than columns. A claim without a citation cannot be constructed, an absence without a
// completeness certificate is a CoverageGap rather than evidence, and a capability request that
// leaves the investigation's scope is refused before anything is dispatched. Storage depends on
// this package and reconstructs these values from rows; this package never depends on storage.
//
// The dependency on the outside world is one interface, Store, and one seam, Reasoner. Store is
// what persistence has to provide; Reasoner is the model boundary, and it is the only component
// in this program that is faked in commit CI — the deviation and its reason are recorded at the
// seam itself, in reasoner.go.
//
// Nothing in this package logs, traces or measures evidence content. Identifiers, counts,
// outcomes and reasons do travel; the text a customer's systems produced does not.
package investigation
