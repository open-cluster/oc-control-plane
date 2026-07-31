# Abstention is a first-class outcome; a confident unsupported conclusion is a design failure

Status: ACCEPTED (2026-07-30 — founder decision in the architecture grilling session)

The investigator must be able to decline to conclude. When evidence is insufficient, absent or
contradictory, the terminal outcome is an explicit abstention that names what is missing, what
contradicts what, and what could not be checked — not a low-confidence guess.

The evaluation standard that follows is zero-tolerance. Across the scenario harness, **no run may
state a wrong explanation without surfacing a coverage gap or a contradiction alongside it**. One
violation is treated as a design failure, not as a tuning problem to be resolved by adjusting a
threshold.

The asymmetry is the reason. Five abstentions cost almost nothing: the on-call engineer does what
they would have done anyway. One confident wrong root cause that sends an engineer down a
forty-minute dead end at 03:00 ends the product's credibility with that team permanently, and
being right the other nine times repairs none of it. The buyer is a platform or SRE leader and the
daily user is on call; trust is the entire asset.

## What this constrains

- Every claim in an investigation's output cites the EvidenceItems that support it. An uncited
  claim is rejected by the output schema, not caught in review.
- Confidence is never sufficient on its own to promote an explanation. `CONTEXT.md` already
  separates the most supported explanation from a VerifiedCause; this decision gives that
  separation teeth by making the unsupported-but-confident case a failure rather than a
  permitted state.
- Contradictory evidence is retained and shown, not resolved silently in favour of the leading
  hypothesis.
- Missing evidence is reported as a coverage gap with its consequence — what could not be
  concluded because of it — rather than as an error or a silent omission.

## Consequences

- Scoring the harness needs ground truth, which is why the scenarios are failures deliberately
  created rather than incidents found in the wild.
- The product will sometimes say nothing useful, visibly. That is the accepted cost and it must
  not be engineered away by lowering the standard for what counts as support.
- Measured outcomes are time to first useful hypothesis, specialist escalation rate, and engineers
  involved per incident. None of them improve if the output cannot be trusted without checking,
  so the kill criterion protects the metrics rather than competing with them.

## Considered and rejected

**Tolerating wrong conclusions when flagged low-confidence.** It moves the calibration burden onto
a reader who, at 03:00, reads the top-ranked line and skips the score. The confidence number then
functions as liability transfer rather than as information.

**Setting the bar after the first harness run.** Avoids committing to a number before knowing what
is achievable, and in practice sets the bar to whatever the first implementation happened to
reach. The standard is a product decision, not a measurement.
