# Spec — Kubernetes events and container logs capabilities

Status: IMPLEMENTED. `internal/capability/capability.go` declares
`kubernetes.namespace.events` and `kubernetes.container.logs` beside the workload runtime read, and
the Relay side is `opencluster-relay/plans/kubernetes-events-and-logs.md`. The line below was stale
until 2026-08-05 and is kept for the reasoning, not the status.

Superseded status line: READY FOR IMPLEMENTATION. Prerequisite for `spec-first-investigation.md`.
Date: 2026-07-31 (revision 2 — the language is aligned with ADR-003's second amendment: a job
reaches an **Evidence Connection**, which is one configured instance of the Kubernetes
Integration, and the Relay is where that job runs rather than what it reaches)
Repositories: the protocol contract and the executors live in the Relay (`oc-relay`); dispatch,
validation and evidence minting live in the Go control plane. This document is the shared
specification for both halves and is recorded here with the rest of the planning record.
Decision records: ADR-001 (typed bounded closed capability registry), ADR-012 (untrusted evidence
and redaction), ADR-011 (abstention standard), ADR-010 (read live what does not decay), ADR-003 as
amended (the Connection is the sole environment authority)
Glossary: `CONTEXT.md`

## Problem Statement

One capability exists. `kubernetes.workload.runtime` v1 reports that a pod is in
`CrashLoopBackOff`, that a container exited with code 137, and that it has restarted eleven times.
It cannot report why any of that happened.

An investigation built on it alone would reach the same conclusion for every failure — "the
container is failing" — which is the fact the engineer already had when they were paged. The two
reads that carry the answer in the overwhelming majority of Kubernetes workload failures are the
cluster's own account of what it did (Events) and the application's own account of what happened
(container logs, particularly the *previous* container's, which is the one that died).

Assumption 4 in the implementation status asked whether one capability is enough to prove an
investigation. It is not, and the answer did not require an experiment.

Both reads are also the two most dangerous things the platform will do. Events carry text from
controllers and admission webhooks. Logs carry text an attacker can write from outside the system,
knowing an AI SRE reads them, and they carry secrets that were never meant to be printed and were
printed anyway.

## Solution

Two new capabilities in the closed compiled registry, each with frozen typed argument and result
messages, each bounded, each returning a completeness basis the control plane consumes.

**`kubernetes.namespace.events` v1** — bounded, time-windowed Events in one namespace, optionally
narrowed to one involved object. Answers what the cluster says it did.

**`kubernetes.container.logs` v1** — bounded log lines from one container of one pod, with an
explicit selector for the previous terminated container. Answers what the application said before
it died.

Neither introduces a command string, a shell, a path, a label selector expressed as free text, or
any generic query surface. Both re-validate their arguments on receipt, both apply the customer's
local caps, and both report the effective bound they actually applied rather than the one they were
asked for.

## User Stories

1. As an on-call engineer, I want to see what the cluster said about my workload, so that a
   scheduling or image failure explains itself without me guessing.
2. As an on-call engineer, I want to see the logs of the container that died, not the one that
   replaced it, so that I read the failure rather than the restart.
3. As an on-call engineer, I want events narrowed to the object I care about, so that a busy
   namespace does not bury the one that matters.
4. As an on-call engineer, I want events bounded to my investigation window, so that last week's
   failures do not appear as this morning's.
5. As an on-call engineer, I want to be told when the cluster had already expired the events I
   asked for, so that I do not read an empty result as nothing having happened.
6. As an on-call engineer, I want to be told when a log read was truncated, so that I do not treat
   a partial view as the whole story.
7. As an on-call engineer, I want log lines to carry their timestamps, so that they can be placed
   on a timeline against everything else.
8. As an investigator, I want a read that found nothing and completed to be usable as evidence of
   absence, so that a certified negative is possible.
9. As an investigator, I want a read that was truncated to be unusable as evidence of absence, so
   that a bound is never mistaken for a fact.
10. As an investigator, I want both capabilities to report the bound actually applied, so that a
    customer's local cap is visible rather than silent.
11. As an investigator, I want a missing pod, missing container or missing namespace reported as a
    typed outcome rather than a failure, so that the difference between "not there" and "could not
    look" survives.
12. As an operator, I want to grant the Relay only the permissions these reads need, so that adding
    a capability does not widen my cluster's exposure more than the capability requires.
13. As an operator, I want a read the Relay is not authorised for to be reported as unauthorised
    rather than empty, so that an RBAC mistake is diagnosable and never looks like an absence.
14. As an operator, I want local hard caps on log volume that the control plane cannot raise, so
    that a server-side change can never increase what leaves my cluster.
15. As an operator, I want the Relay to refuse a log request naming a namespace outside my local
    allowlist, so that policy is enforced where I control it.
16. As a security reviewer, I want log content treated as untrusted for its whole life, so that
    text an attacker wrote cannot become an instruction.
17. As a security reviewer, I want log volume bounded in bytes as well as lines, so that one very
    long line cannot exhaust a bound expressed only in lines.
18. As a security reviewer, I want no capability to accept a free-text query, a path, a command or
    a field selector, so that the closed registry stays closed.
19. As a security reviewer, I want log streaming and follow modes to be structurally absent, so
    that no read can become an open channel out of the cluster.
20. As a security reviewer, I want these capabilities usable against real customer data only once
    Relay-side redaction exists, so that secrets printed into logs are not shipped before there is
    a mechanism to mask them.
21. As a security reviewer, I want the log capability to record how much it withheld, so that a cap
    or a policy is visible as a coverage gap rather than as a clean read.
22. As an engineer, I want each capability's messages frozen at v1, so that a semantic change mints
    a new version rather than silently changing meaning.
23. As an engineer, I want the control plane to validate arguments before dispatch and the Relay to
    re-validate on receipt, so that neither trusts the other.
24. As an engineer, I want a capability version the Relay does not advertise to be refused rather
    than executed under a different version's semantics, so that the failure found by the existing
    end-to-end proof cannot recur.
25. As an engineer, I want both capabilities to reuse the existing job, lease and fencing path, so
    that adding a read does not add a concurrency model.
26. As an engineer, I want the field caps enforced on both ends, so that a bound is a contract
    rather than a hope.
27. As an engineer, I want timestamps from these reads treated as provenance only, so that a
    cluster's clock can never influence a lease, an expiry or a dedup comparison.
28. As the founder, I want these two and no more, so that the first investigation is proven before
    the capability surface grows.

## Implementation Decisions

**Two capabilities, named general-to-specific**, matching the existing convention:
`kubernetes.namespace.events` and `kubernetes.container.logs`, both at schema version 1. Both are
frozen on release; a semantic change mints v2 messages and never edits v1.

**Arguments are typed and closed.** Events take a namespace, an optional involved-object reference
(kind, name, uid), a bounded time window and a maximum event count. Logs take a namespace, a pod
name, a container name, a boolean selecting the previous terminated container, a maximum line
count, a maximum byte count, and an optional since-time. There is no free-text field selector, no
label selector expressed as a string, no path, no command, and no follow or stream flag. The
absence of a follow flag is structural, not a default.

**Both bounds on logs are enforced, and the byte bound wins.** A line cap alone is defeated by one
very long line; a byte cap alone makes the result unpredictable in shape. Both are applied and both
effective values are reported.

**Reading the previous container is an explicit argument, not an inference.** The container that
died is the one that explains the failure, and the container that replaced it is usually silent.
Making this a boolean the planner sets deliberately keeps the decision visible in the record.

**Outcome taxonomy mirrors the existing capability's,** because the distinctions it draws are the
ones the truth chain depends on: success, unreachable, unauthorized, and the specific not-found
cases — namespace, pod, container. A not-found is never absence evidence. An unauthorized read is
never reported as empty; conflating them is how an RBAC misconfiguration becomes a certified
negative, which is the worst outcome the truth model can produce.

**Completeness basis travels with every result.** For events: the number returned, whether the
bounded list completed without a continuation token, the effective count bound applied, and the
source read time. For logs: lines returned, bytes returned, whether the read reached the beginning
of the available log or was truncated by a bound, the effective bounds applied, and the source read
time.

**Certified absence is minted centrally and only from a complete read** — and that minting does
not exist yet, for these capabilities or for the existing one. It belongs to the truth chain in
`plans/spec-first-investigation.md`, along with CoverageGap. What this slice owes that slice is
the basis, carried intact and stored: a completeness flag that is false whenever a bound bound or
a read was refused, a retention flag that is true whenever an empty events window cannot be
distinguished from an expired one, and the effective bounds actually applied. The end-to-end proof
asserts all of it arrives in the recorded result, because a field lost between the cluster and the
column would look, from the control plane's side, exactly like a cluster where nothing happened —
and that is the one input a certified negative must never be built on. Reading this document as a
claim that certification is implemented would be a misreading; it is a claim that certification
will have something honest to read.

**Event expiry is reported, not inferred.** A cluster's event TTL means an empty window is
ambiguous. Where the requested window begins earlier than the oldest event the read could have
seen, the result says so, and the control plane records a CoverageGap rather than an absence.

**Local caps lower, never raise.** The customer-authored local configuration sets a floor the
control plane cannot go below, in the same ownership shape ADR-001 and ADR-004 already establish
for destination allowlists and volume caps. The result reports the bound actually applied, so a
lowered cap is visible in the evidence rather than invisible in the config.

**RBAC additions are minimal and stated.** Events require read access to events in the namespaces
in scope. Logs require access to the pods log subresource. Both are namespace-scopeable, and the
Helm chart offers namespace-scoped roles as the default with cluster-wide as an explicit choice.
Adding a capability must not silently widen the Relay's cluster permissions.

**These capabilities do not redact.** Redaction is ADR-012's Relay-side policy and a separate slice.
**Consequence, stated as a gate rather than a caveat: these capabilities may be used against
synthetic scenario clusters immediately, and against no cluster containing real data until
redaction exists.** The scenario harness is unblocked; a design-partner installation is not.

**Nothing here changes the job path.** Both capabilities are dispatched, leased, fenced and recorded
exactly as `kubernetes.workload.runtime` is. Adding a read must not add a concurrency model.

**A job reaches a Connection, and runs on a Relay.** Both capabilities are executed against the
Evidence Connection the job names — one configured instance of the Kubernetes Integration, inside
one Environment — and the Relay bound to that Connection is where the execution happens. The
distinction matters here for one concrete reason: a customer with two clusters served by one Relay
has two Connections, and a result must be attributable to the cluster it was read from rather than
to the installation that read it. That attribution is the `connection_id` on the job, specified in
`plans/spec-environments-and-connections.md` and a prerequisite for these capabilities being usable
in an investigation rather than in a test.

**No parity oracle exists for either.** The frozen .NET reference has no events reader and no log
reader; container logs were a planned Stage 1B slice that was never built. Confidence therefore
comes from specification and adversarial tests rather than from differential comparison, which is a
weaker form of assurance and is worth compensating for with more attention to the refusal paths
than a happy-path suite would suggest.

## Testing Decisions

**What makes a good test here.** It asserts what the control plane can observe about a result — the
outcome was `unauthorized` rather than empty, the completeness flag was false after truncation, the
effective bound reported was the local one rather than the requested one. It does not assert how
the executor called client-go.

**Three levels, all existing.**

- **Relay unit tests**, beside the existing `internal/capabilities/kubernetes/runtime` suite:
  argument validation, mapping, bound application, outcome classification.
- **The end-to-end process harness in `test/e2e`**, with real Kubernetes: a namespace is broken in a
  known way, both capabilities are dispatched through the real protocol, and the recorded results
  are asserted against durable state. This is the level that proves the completeness basis arrives
  intact, which is what the central certificate depends on.
- **The scenario harness**, where these capabilities are exercised as part of whole investigations.

**Refusal paths get first-class attention, because there is no oracle.** Specifically: a namespace
the Relay cannot read, a pod that does not exist, a container name that does not exist on a pod that
does, a previous-container read on a container that has never restarted, a window entirely older
than the cluster's event TTL, a log longer than both bounds, a single line longer than the byte
bound, and a capability version the Relay does not advertise.

**Scenarios.**

- Events for a namespace return within the window and report a complete read.
- Events narrowed to an involved object exclude other objects in the same namespace.
- A window older than the cluster's event TTL returns empty and reports the truncation, and the
  control plane records a CoverageGap rather than an absence.
- An event read the Relay is not authorised for reports `unauthorized`, never empty.
- Logs from a running container return lines with timestamps.
- Logs with the previous-container flag return the terminated container's output, and a container
  that never restarted returns the typed not-found rather than an error.
- A log longer than the line bound is truncated and reports incomplete.
- One line longer than the byte bound is truncated by bytes and reports incomplete.
- A local cap lower than the requested bound is applied and the effective bound is reported.
- A namespace outside the local allowlist is refused by the Relay.
- A truncated result cannot mint a certified absence centrally.
- A job naming v2 of either capability is refused and never executed.
- Both capabilities survive the Relay and the control plane restarting mid-job.
- No log line, trace attribute or metric label emitted by either half contains evidence content.

**Prior art.** `kubernetes.workload.runtime` is the template for all of it: its proto file
documents the freezing rule and the provenance-only timestamp rule, its executor establishes the
outcome taxonomy and the field caps, and the existing end-to-end tests establish how a completeness
basis is asserted against durable state. The capability-version refusal test exists because that
test found a real defect once; both new capabilities get the same test.

## Out of Scope

- Any log follow, stream or tail-forever mode. Structurally absent, permanently.
- Redaction and masking (ADR-012) — a separate Relay slice, and a gate on real data.
- Metrics, traces, or any Prometheus capability.
- Cross-namespace or cluster-wide event queries.
- Log search, filtering by content, or regular expressions. The bound is time and volume, not
  content, because a content filter is a query language and a query language is the generic surface
  ADR-001 exists to prevent.
- Any third capability. Two are enough to prove an investigation; the surface grows after that is
  demonstrated, not before.
- The persisted change ledger, which is a different mechanism (ADR-004, ADR-010).

## Further Notes

The log capability is the highest-risk thing in the program so far, on two independent axes.

It is the largest prompt-injection surface, because log content is written by software an attacker
may control and is read by a model. ADR-012's containment — untrusted marking at the boundary, a
planner structurally unable to derive a request from evidence text, and mandatory citations — is
what bounds the damage to a traceable conclusion. That containment is specified in the investigation
document and is a prerequisite for using this capability in a run, not an enhancement.

It is also the largest accidental-disclosure surface, because applications print secrets into logs
constantly and nobody notices until something reads them. That is why redaction gates real-data use
rather than merely being scheduled after it. The order — capability first on synthetic clusters,
redaction before any real one — is deliberate and should be written into the release checklist
rather than remembered.
