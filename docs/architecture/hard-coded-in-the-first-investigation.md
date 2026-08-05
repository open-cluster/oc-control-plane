# What the first investigation hard-codes

Status: CURRENT as of 2026-08-01, written with the slice it describes.
Specification: `plans/spec-first-investigation.md`

This list is part of the deliverable rather than an afterthought. The slice's own specification says
so: scope resolution, the hypothesis vocabulary and the brief's contents are all narrow, **a demo
will make them look general**, and the honest thing is a written list of what is narrow rather than
an early generalisation nobody asked for.

Each entry says what is fixed, why it was acceptable to fix it, and what would have to change. None
of it is a defect. All of it is expected to be discarded.

---

## Scope resolution

**Fixed.** A scope is one Kubernetes namespace, one workload kind from a set of three
(`deployment`, `statefulset`, `daemonset`), and one workload name, resolved through exactly one
Kubernetes Connection.

**Why.** Canonical resource identity — that the object Kubernetes names one way and AWS names
another are the same thing — is the largest unsolved question in the product and has one line of
design. Building a general resolver now would be designing against an anticipated failure instead of
an observed one.

**What would change it.** A second integration whose evidence has to appear in the same case. At
that point identity stops being "what the cluster says it is" and needs a model of its own.

**Where it is.** `internal/investigation/scope.go`, and the `namespace` / `workload_kind` /
`workload_name` columns on `investigation`.

---

## The opening plan

**Fixed.** Every round opens with exactly two reads, in this order: `kubernetes.workload.runtime`
then `kubernetes.namespace.events`, both against the case's own scope and window.

**Why.** The brief has to be reproducible or one run is not comparable to another (ADR-009). Two
reads is the smallest set that establishes what is being looked at and what the cluster said about
it.

**What would change it.** More capabilities. The opening plan is a plan *template*, its resolved
snapshot is already pinned per round, and adding to it is a data change rather than a code change
everywhere else.

**Where it is.** `openingReads` and `openingProposals` in `internal/investigation/arguments.go`.

---

## "Recent changes"

**Revised 2026-08-05, and the revision is narrower than the entry predicted.** The change ledger
is built and the brief consumes it: ledger-recorded changes join `RecentChanges` beside the
event-derived ones, and a window the ledger cannot vouch for records a `CoverageGap` naming the
boundary. What this entry promised — that the ledger "replaces this list entirely" — was NOT
done, deliberately.

**What stays fixed.** A live-read change is still a Kubernetes event whose reason is one of:
`ScalingReplicaSet`, `SuccessfulCreate`, `SuccessfulDelete`, `Created`, `Started`, `Killing`,
`Pulled`, `Preempted`, `Evicted`.

**Why the list survives.** Two reasons, both structural. An event-derived change is the only
CITABLE form of one — the ledger is a navigation index whose changes must be revalidated live
before a conclusion rests on them, and the events read IS that revalidation, so removing it would
remove the revalidation path the ledger's own rule depends on. And the opening hypotheses would be
blind wherever the ledger is cold — a fresh install, a first tick still pending, a Relay outage —
which are exactly the moments an investigation is most likely to be running.

**What would change it.** Evidence from the harness that the two sources duplicate each other
enough to confuse the planner; the fix then is deduplication, not deletion.

**Where it is.** `changeReasons` in `internal/investigation/results.go`; the merge in
`internal/investigation/runner.go`.

---

## ReplicaSet revision history is not read at all

**Fixed.** The brief carries no revision history. The specification names it as available live and
bounded by `revisionHistoryLimit`; no capability in this build reads it.

**Why.** There is no capability for it, and adding one was not in this slice. What is available —
generation against observed generation, and the events the cluster still holds — is what the brief
carries.

**What would change it.** Either a capability that reads it, or the change ledger, which makes it
unnecessary.

---

## Log reads are bounded to pods the brief resolved

**Fixed.** A `kubernetes.container.logs` read may name only a pod that appeared in the brief's
topology for this workload.

**Why.** It is the strongest scope check available without a general resource resolver. Without it,
"read the logs of a pod in this namespace" would be a read of any workload the namespace happens to
hold, which is a wider scope than the case was opened under.

**Consequence, stated rather than discovered.** A pod created after the brief was assembled cannot
be read in the same round. Reinvestigation reassembles the brief and can.

**Where it is.** `checkScope` in `internal/investigation/validation.go`.

---

## The hypothesis vocabulary is the model's

**Fixed.** Nothing constrains what a hypothesis may say. What is fixed is its SHAPE: a statement, a
falsification condition, and an ordinal.

**Why.** A closed vocabulary of failure classes is the runbook executor ADR-009 exists to prevent.
The falsification condition is required because an explanation nothing could disprove is a belief.

---

## Execution controls are the product's defaults

**Fixed.** Eight capability requests, two adaptive passes, two megabytes of results, five minutes of
wall clock, ninety seconds per read, no cost ceiling.

**Why.** No customer has authored any, and Organization and Environment guardrail administration is
deferred until a design partner needs it (ADR-015). The composition rule is implemented and tested;
the administration product is not built.

**What would change it.** A customer surface for controls. The snapshot is already pinned per round
and composes by most-restrictive, so what is missing is where a customer writes them, not how they
apply.

**Where it is.** `DefaultControls` in `internal/investigation/controls.go`.

---

## Severity is a column nothing writes

**Fixed.** `investigation.severity` and `investigation.severity_source` exist, are read by the list,
and are always NULL.

**Why.** A manual start has no severity: nobody stated one. The columns exist because severity
arrives with the trigger source and must keep its attribution (ADR-015), and adding the pair after
rows exist is the retrofit the schema was written to avoid.

**What would change it.** Signal-triggered investigation.

---

## Trust is always relay-attested

**Fixed.** Every EvidenceItem this build produces carries `relay_attested`. Nothing is ever
centrally verified.

**Why.** Every capability in this build runs in the customer's own infrastructure. There is
currently no read this control plane makes itself, so there is nothing it could verify centrally.

**What would change it.** A control-plane-local capability — a public SaaS API read, say. The trust
class already exists so that the first one does not have to invent it.

---

## Coverage never reports "not applicable"

**Fixed.** The five coverage states are implemented and one of them is unreachable: this build
carries only Kubernetes reads and only reaches Kubernetes Connections, so no capability is
inapplicable to the stack.

**Why.** The state exists because reporting a missing Nomad capability as a gap is how a coverage
report stops being read, and the shape has to be right before the second integration arrives rather
than after.

---

## The model boundary has no live provider

**Fixed.** The composition root wires the model boundary from what it is given. Given nothing, it
wires the provider-unavailable boundary, and every round fails honestly saying the reasoning step
could not run.

**Why.** Live-model evaluation is the scenario harness's job (`plans/spec-scenario-harness.md`), and
a provider client was not in this slice. What is built is the seam, the transcript replay, the
version keying that refuses a stale recording, and the fail-closed path.

**Consequence, stated rather than discovered.** A transcript is keyed on four dimensions — model,
prompt version, output schema version, investigator version — and only three of them can currently
differ. There is one non-unavailable boundary in this build, so `model` is the constant
`"recorded"`. A recording made against a real provider would be refused by a build carrying a
different one only once that provider names itself here.

**What would change it.** The harness slice, which needs a live provider by definition.

---

## One trigger

**Fixed.** A human naming a Connection, a scope and a window. `TriggerSignal` is declared and
unreachable.

**Why.** Intake already produces Signals and nothing consumes them; that remains true after this
slice and is the natural next one. The manual path does not depend on a customer's alert quality,
which is exactly what makes the product demonstrable.
