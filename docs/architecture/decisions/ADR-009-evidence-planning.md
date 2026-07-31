# A deterministic Investigation brief, then a bounded adaptive planner

Status: ACCEPTED (2026-07-30 — founder decision in the architecture grilling session)
Amended 2026-07-31: the deterministic opening is orientation, not evidence, and is named the
Investigation brief. The bounded adaptive planner is in the first slice, not deferred.

Every investigation begins by assembling a fixed, versioned **Investigation brief**, and only then
may a bounded planner select typed read-only capabilities in response to competing hypotheses.
OpenCluster ships the operational investigation knowledge through capability metadata,
investigation policy and planner logic; customers and on-call engineers never author evidence
plans for their incidents.

**The brief is orientation, not evidence.** It carries resource identity, the trigger, the time
window, recent changes, live topology context, the capabilities available in scope, and current
coverage. It states what is being looked at and what may be asked. It deliberately does not
prescribe the investigation, and it is not expected to contain the answer.

The distinction is load-bearing. A fixed bundle of *evidence* would make this a runbook executor
and an evaluation of it would validate the wrong thesis: whether a predetermined bundle happened
to contain the answer, rather than whether the system chooses useful next evidence. A fixed
briefing is what any investigator receives before forming a first hypothesis and constrains
nothing about where the investigation goes.

What the brief still buys is real: a reproducible opening that makes one run comparable to
another, a planner that knows what it can ask before it asks, and a guaranteed orientation
regardless of how the reasoning behaves.

## The plan is data, never control flow

A versioned evidence-plan template is a declared artifact. The **fully resolved plan snapshot** is
persisted on every Investigation, together with every initial and adaptive capability call, its
arguments, its result, the policy version and the planner version. A run's intended reads must be
recoverable without reading the source at the commit that produced it.

This is the seam that decides whether the planner can be replaced without a rewrite. The chooser
is one component that emits a plan; dispatch, evidence validation, the truth chain and the case
pack never learn whether a table or a model produced it.

## Bounds on every adaptive request

An adaptive capability request is validated against organization, Environment, Connection,
resource scope, permission, time range, query cost, result size, step count and timeout before
dispatch. There is no shell, no unrestricted query, and no generic remote procedure mechanism —
ADR-001's closed typed capability registry is the only execution surface, and adaptivity does not
widen it.

## Consequences

- Cost and latency are bounded by construction, not by a prompt asking the model to be brief. A
  storm cannot become an unbounded spend through the planner.
- **Choice and conclusion are scored separately.** Because the planner both selects evidence and
  reasons over it, a bad outcome has two candidate causes. The harness therefore scores whether
  the next-evidence choice was one a senior engineer would have made, independently of whether
  the conclusion was right and supported. Without that separation, adaptivity destroys the
  attribution a deterministic opening would have given.
- **Abstention gets harder, deliberately.** With a fixed bundle, missing evidence means abstain.
  With rounds of looking, the system can search in the wrong direction and then conclude from a
  partial picture that feels thorough. ADR-011's standard is unchanged and this is the specific
  way it is most likely to be violated, so the exhaustion path — rounds spent, still unsupported
  — must terminate in an abstention that names what was still missing.
- Reads that returned nothing decisive are recorded, not discarded. That record is what tells
  which briefing fields earn their place and which adaptive requests were wasted — a dataset an
  agent-first architecture never accumulates because it never repeats itself.
- The planner never derives a capability call from evidence text. The chain from an observation to
  a further read always passes through a typed hypothesis a human can read. See ADR-012.

## Considered and rejected

**Declared evidence plans only, with the model never choosing a read.** Reproducible, cheap and
fully evaluable, and rejected: it makes the product's ceiling one person's SRE intuition packaged
as a lookup table, requires a hand-written plan per failure class, and would have the evaluation
answer the wrong question — whether a predetermined bundle contained the answer rather than
whether the system investigates. It is a runbook executor with a language model attached.

**A purely agentic loop with no deterministic opening.** Most adaptive and worst to evaluate. Two
runs of one scenario produce different evidence, so a regression is indistinguishable from
variance, and the case pack's promise of replay without live sources holds only if the provider
guarantees determinism, which none does.

**Deferring adaptivity past the first slice.** Considered and rejected by the founder: a first
slice with no adaptive step would validate a runbook executor rather than the thesis the product
exists to prove. The first implementation is limited to two adaptive rounds under strict scope,
step, cost, timeout and result-size budgets — enough to test whether the system chooses useful
next evidence, bounded enough that it cannot become an unpriced loop.
