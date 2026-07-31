# Evidence text reaches the model as contained untrusted data; redaction declares a coverage gap

Status: ACCEPTED (2026-07-30 — founder decision in the architecture grilling session)

Bounded raw evidence text may reach the model, after Relay-side customer-controlled redaction and
structural isolation as untrusted data. A strict tier in which raw text requires explicit
per-investigation human approval is a future enterprise policy, not the default.

The alternative of never showing the model free text was considered seriously and rejected because
it removes the product's value. The single most decisive artifact in a crashloop investigation is
the actual error string — a refused connection to a specific address, a stack trace naming a
configuration key. Those cannot be enumerated in advance, so classifying log messages into a
bounded taxonomy discards exactly the information that explains the incident and leaves a system
that knows a container exited with code 1 and can never say why.

## Containment, not trust

Evidence text originates in customer systems, may be attacker-influenced, and is treated as
untrusted for its entire life. Containment is structural rather than a matter of prompt wording:

- Untrusted content is marked as data at the boundary where it enters the reasoning step, and is
  never presented as instruction.
- **The planner may not derive a capability call from evidence text.** Every adaptive read is
  justified by a typed hypothesis a human can read; the chain from "a log said X" to "therefore
  fetch Y" always passes through that hypothesis. This is the property that stops injected text
  from steering execution, and it composes with ADR-009's per-request scope validation.
- Every claim cites the evidence rows that produced it, so a suspicious conclusion is traceable to
  the exact text that caused it.

Prompt injection is not solved by anyone, and this decision does not claim to solve it. What it
claims is that the damage is bounded to a conclusion a reader can trace and dismiss, rather than
reaching execution or arriving silently. Design and review follow established application-security
guidance for LLM-integrated systems, including the OWASP guidance for large-language-model
applications; the containment obligations above are the minimum, not the ceiling.

## Redaction is a coverage gap

Redaction runs at the Relay under customer-authored, server-immutable local policy — the masking
rules ADR-001 already assigns to that layer. Every masked field records a coverage gap on the
evidence item that carried it.

Redacted evidence is never treated as a complete clean read. The investigator must be able to say
that it could not read a connection target because the customer's policy masks it, and must
explain when masking limited its conclusion. Without this, a customer's own privacy policy
silently degrades their investigations and the product is blamed for the hole.

Masking applies to sensitive values — credentials, tokens, personal data, connection strings. It
does not apply to statuses, reasons, counts, timestamps or identifiers, which carry no secret and
are the substance of an investigation.

## Consequences

- What leaves a customer environment is stateable in one sentence a security team can evaluate:
  redacted evidence for the resources under investigation leaves; credentials, telemetry streams
  and everything outside the investigation scope do not.
- Model provider remains a placement dimension under ADR-002, so the residency answer can differ
  per tier without a code path.
- A too-aggressive redaction rule degrades conclusions visibly rather than silently, which makes
  it a conversation with the customer instead of a defect report.
