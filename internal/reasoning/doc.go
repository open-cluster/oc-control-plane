// Package reasoning satisfies the investigation capability's model boundary against a real
// provider. It renders the brief into a prompt, declares the output schemas, decodes what
// comes back into the domain's own types, and prices every call in integers.
//
// The dependency runs one way. This package imports internal/investigation because the
// boundary, its types and its invariants belong to the capability that defines their
// meaning; the domain never imports this package, never learns that a provider exists, and
// never names a vendor.
//
// A vendor appears in exactly one place — the adapter subpackages. Everything here is
// expressed in this system's vocabulary, so a second provider is a new directory and a
// configuration entry rather than a change to the domain or to this orchestration. Two
// adapters exist because a provider-neutral contract with one implementation is an
// assertion and one with two is a demonstration; the two chosen disagree in the ways that
// matter, which is what tests the contract.
//
// Nothing here logs, traces or measures what the customer's systems said. Identifiers,
// counts, outcomes and reasons travel; the credential does not — see Secret, whose
// rendered forms are all the same placeholder.
package reasoning
