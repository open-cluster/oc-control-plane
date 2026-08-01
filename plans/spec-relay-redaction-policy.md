# Spec — Relay-side redaction policy

Status: BUILT 2026-08-01. The enforcement point, the built-in defaults, add-only customer policy,
fail-closed behaviour, the declared-free-text-field build gate, the dry run, and the control
plane's coverage-gap consumer all exist. The end-to-end negative assertion — a synthetic secret in
a real container's log appears nowhere in the control plane's database — was verified to FAIL with
the enforcement point removed. **The blocking gate it existed to lift is lifted: a cluster
containing real data may be connected once a policy is in force.** Not a prerequisite for the
scenario harness, whose clusters are synthetic.
Date: 2026-07-31 (built 2026-08-01)
Repository: the Relay (`oc-relay`); the coverage-gap consumer is the Go control plane
Decision records: ADR-012 (untrusted evidence and redaction), ADR-001 point 8 (customer-authored,
server-immutable local policy), ADR-011 (abstention standard), ADR-004 (local configuration sets a
floor the server cannot lower)
Glossary: `CONTEXT.md`

## Problem Statement

Applications print secrets into logs constantly, and nobody notices until something reads them.

The container-logs capability is about to ship. It exists because the decisive artifact in most
workload failures is the application's own account of what happened, and that account routinely
contains a database connection string, a bearer token, an API key echoed by a misconfigured client,
or a stack trace carrying credentials in a URL. Today that text would leave the customer's cluster
and reach a model provider unchanged.

ADR-001 assigned masking to the Relay and ADR-012 decided that redaction produces a declared
coverage gap rather than a silent clean read. Neither is built. There is no policy format, no
enforcement point, and no way for a customer to control what leaves.

The second problem is subtler and is the reason the coverage-gap rule exists. A redaction rule that
is too aggressive silently destroys evidence. If a masked connection target is indistinguishable
from an absent one, the customer's own privacy policy quietly degrades their investigations and the
product is blamed for the hole.

## Solution

One enforcement point in the Relay, between a capability producing a result and that result being
serialized onto the session stream. Nothing reaches the wire without passing through it.

Policy has two layers. A **built-in default** masks high-confidence secret shapes and is on from the
first install with no configuration. A **customer-authored local policy** adds patterns and field
rules, and can only make masking stricter — the control plane can request nothing and lower nothing.

Every masked occurrence is counted and reported: which field, which rule matched, how many times.
The value is never sent, never hashed, and never partially revealed. The control plane turns those
counts into CoverageGaps on the EvidenceItem that carried them, so an investigator can say it could
not read something because the customer's policy masks it.

## User Stories

1. As an operator, I want secrets masked before they leave my cluster, so that using this product
   does not become a disclosure channel.
2. As an operator, I want sensible masking without writing any configuration, so that the first
   install is safe by default.
3. As an operator, I want to add my own patterns, so that a secret shape specific to my systems is
   covered.
4. As an operator, I want to mask an entire field, so that I can exclude something categorically
   rather than by pattern.
5. As an operator, I want the control plane to be structurally unable to weaken my policy, so that
   a server-side change can never increase what leaves.
6. As an operator, I want a policy that fails to parse to stop evidence leaving rather than fall
   back to defaults, so that a typo is loud rather than permissive.
7. As an operator, I want to see what my policy masked, so that I can tell whether it is working
   and whether it is too broad.
8. As an operator, I want statuses, reasons, counts, timestamps and identifiers left alone, so that
   masking protects secrets rather than destroying investigations.
9. As an operator, I want to test my policy against sample text before applying it, so that I learn
   it is too broad before it costs me an investigation.
10. As an on-call engineer, I want to be told that a field was masked, so that I do not read a
    policy hole as an absence of evidence.
11. As an on-call engineer, I want to know which rule masked it, so that I can ask the right person
    to adjust the policy.
12. As an on-call engineer, I want an investigation to say when masking limited its conclusion, so
    that I know to look at that field myself.
13. As an investigator, I want a masked field to prevent a certified absence over that field, so
    that a hole is never mistaken for a fact.
14. As a security reviewer, I want redaction enforced at one point, so that adding a capability
    cannot bypass it.
15. As a security reviewer, I want a new capability to be structurally unable to emit unredacted
    text, so that safety is not a checklist item at review time.
16. As a security reviewer, I want the masked value never transmitted in any form, so that a
    reversible encoding does not defeat the purpose.
17. As a security reviewer, I want no hash of a masked value sent, so that a low-entropy secret
    cannot be recovered by brute force from the digest.
18. As a security reviewer, I want the count of masked occurrences to be the only quantitative leak,
    so that the side channel is bounded and stated.
19. As a security reviewer, I want the policy file's location and permissions documented, so that
    the file that governs disclosure is not world-readable by accident.
20. As a security reviewer, I want the effective policy visible in the Relay's own diagnostics, so
    that what is actually in force can be verified rather than assumed.
21. As a security reviewer, I want redaction applied before anything is written to the Relay's own
    logs, so that diagnosing the Relay does not leak what redaction removed.
22. As an engineer, I want redaction to be pure and deterministic, so that the same input always
    produces the same output and a recorded transcript stays valid.
23. As an engineer, I want redaction bounded in cost, so that a pathological pattern cannot make a
    capability time out.
24. As an engineer, I want the policy format versioned, so that a change to its semantics is
    visible rather than inferred.
25. As the founder, I want this to gate real-data installations rather than block the harness, so
    that evaluation proceeds while disclosure risk does not.

## Implementation Decisions

**One enforcement point, structurally.** Redaction runs between a capability executor returning its
typed result and that result being serialized. No capability applies its own masking and no
capability can opt out. The consequence to design for: **free-text fields are declared**, so the
enforcement point knows which fields to sweep rather than guessing from the type. A capability
message adding a free-text field without declaring it is a build failure, in the same shape as the
existing banned-API and schema-shape gates.

**Built-in defaults are on from install and cover high-confidence shapes only.** Private key blocks,
bearer and authorization header values, JSON Web Tokens, cloud provider access key identifiers and
secret keys, connection strings carrying credentials, and `password=`-style assignments. The bar for
a default is that a false positive is nearly impossible — anything with a meaningful false-positive
rate belongs in customer-authored policy, because a default that destroys evidence is worse than no
default.

**Customer-authored policy may only add.** It can add patterns, mark named fields as always-masked,
and lower the volume caps that already exist. It cannot disable a built-in rule, cannot widen a
bound, and is never transmitted from the control plane. This is the same ownership rule ADR-004
applies to intervals and ADR-001 applies to destination allowlists: the server requests, local
configuration sets the floor.

**Masking replaces the value with a fixed marker.** Not a partial reveal, not a length-preserving
placeholder, not a hash. A partial reveal leaks the part that is often enough to identify the
secret; a hash of a low-entropy value is recoverable by brute force; a length-preserving marker
leaks length. The marker carries the rule identifier so a reader knows why.

**Only sensitive values are masked.** Statuses, reasons, phases, exit codes, counts, timestamps,
resource names, namespaces and identifiers are the substance of an investigation and carry no
secret. This is stated as a design constraint on what may become a rule, not merely as a default.

**Reporting is counts and rule identifiers, never values.** Each result carries, per declared
free-text field, the number of occurrences masked and the identifiers of the rules that matched. The
control plane records a CoverageGap per masked field on the EvidenceItem that carried it. The
occurrence count is a deliberate, bounded side channel and is stated as such rather than discovered.

**Fail closed.** A policy file that does not parse, references an unknown rule, or contains a
pattern that fails to compile stops the Relay from executing any capability with a declared
free-text field. It does not fall back to defaults, because a customer who wrote a policy has
signalled that defaults are insufficient for them.

**Redaction runs before the Relay's own logging.** The Relay must not log what it just removed.

**Bounded cost.** Patterns are evaluated with a per-result time budget and a total-input bound. A
pattern exceeding the budget is reported as a policy fault and fails closed, rather than being
skipped — skipping is silent disclosure.

**Deterministic and pure.** The same input and policy always produce the same output. Recorded model
transcripts depend on this, and so does any comparison between two runs of one scenario.

**Configuration is nested, under a `redaction:` root.** ADR-004 already recorded that nested
configuration is a paradigm change for a Relay configured by environment variables and that the
committed golden-file configuration-reference gate applies either way. This is the second consumer
of that change; if the inventory slice lands first it establishes the mechanism and this reuses it.

**A dry-run mode exists.** The Relay can evaluate a policy against operator-supplied sample text and
report what would be masked, locally, without a control plane. A policy whose breadth is only
discoverable by losing an investigation is a policy nobody will tune.

## Testing Decisions

**What makes a good test here.** It asserts what leaves: given a result containing a known secret
and a known policy, the serialized message contains the marker and not the secret, and the reported
counts match. It does not assert which regular expression matched.

**The seam is the Relay's own suite, plus the end-to-end harness.** Unit tests cover rule matching,
policy parsing, fail-closed behaviour and bounds. The end-to-end harness covers the property that
matters most and cannot be unit tested: **a secret placed in a real container's log output does not
appear anywhere in the control plane's durable state.** That assertion is over the database, not
over a function's return value, because the claim is about what leaves the cluster.

**The negative assertion is the important one and must be written to fail if enforcement is
removed.** A test that only checks the marker is present passes against an implementation that
sends both.

**Scenarios.**

- A log line containing a bearer token is serialized with the marker and not the token.
- The same line's masked count and rule identifier arrive intact.
- The control plane records a CoverageGap for the masked field on the resulting EvidenceItem.
- A masked field cannot support a certified absence over that field.
- Statuses, exit codes and timestamps in the same result are untouched.
- A customer pattern masks a secret shape the defaults miss.
- A customer policy cannot disable a built-in rule; an attempt is refused at parse time.
- A policy sent from the control plane is refused; there is no such message.
- An unparseable policy stops execution of capabilities with free-text fields, and the refusal
  names the fault.
- A pattern exceeding the time budget fails closed rather than being skipped.
- The Relay's own logs contain neither the secret nor the raw policy.
- Redaction is deterministic across repeated runs on identical input.
- A capability message adding an undeclared free-text field fails the build.
- End to end: a pod printing a synthetic secret is read by the logs capability, and the secret
  appears nowhere in the control plane's database.

**Prior art.** The Relay's existing gates package establishes build-time enforcement of a
structural rule, which is the shape the declared-free-text-field check takes. The capability
executor establishes where a result is produced, which is where the enforcement point sits. The
end-to-end harness already asserts on durable control-plane state, which is exactly the seam the
negative assertion needs.

## Out of Scope

- Redaction of anything the control plane produces. This is about what leaves a customer's cluster.
- Machine-learned or model-based secret detection. Non-deterministic detection breaks transcript
  replay and cannot fail closed meaningfully.
- Reversible tokenisation, format-preserving encryption, or any scheme that lets the platform
  recover a masked value. If the platform can recover it, it left the cluster.
- Per-investigation redaction overrides. Policy is the customer's and does not vary by run.
- Customer-managed encryption keys, which are a different control at a different layer.
- The strict enterprise tier from ADR-012 in which raw text requires per-investigation approval.

## Further Notes

The ordering here is deliberate and should be in the release checklist rather than remembered: the
logs capability ships first and is used against synthetic scenario clusters; redaction ships before
any cluster containing real data is connected. That is not a caveat on the capability — it is a gate
on installations, and the person who onboards the first design partner is the one who has to know
it.

The occurrence-count side channel deserves one honest sentence. Reporting that a field contained
four masked values tells the platform something it would not otherwise know. The alternative —
reporting nothing — makes masking indistinguishable from absence, which ADR-012 rejects as the more
serious failure. The channel is bounded, stated, and cheaper than the confusion it prevents.
