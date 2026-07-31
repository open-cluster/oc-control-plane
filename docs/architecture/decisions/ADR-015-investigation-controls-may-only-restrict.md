# A customer policy may only restrict what OpenCluster may do

Status: ACCEPTED (2026-07-31 — founder decision in the frontend architecture grilling session)

**The invariant, and it is the sentence to defend:**

> A customer-controlled policy may only restrict what OpenCluster may do. It may never prescribe
> what OpenCluster should inspect or in which order.

OpenCluster owns the investigation intelligence: the Investigation brief, capability relevance,
hypothesis formation, evidence selection and every adaptive request. Customers own guardrails —
permitted connections and capabilities, namespace and resource and destination exclusions,
time-window limits, execution limits, raw-text and model-placement restrictions, redaction and
retention requirements, and automatic-start permission.

The rejected alternative is a customer-editable data collection plan, and it deserves recording
because it will be requested. It demos well and it would become the most-used feature in the
product, because engineers configure things and it is the one surface where they can express what
they know. Six months later every customer has hand-tuned plans, the planner is decoration, and the
AI SRE is a workflow engine with a language model attached — which is the outcome ADR-009 was
written to prevent, arriving as a feature request rather than as a design decision.

Any field that would tell OpenCluster what it *should* do is a planner change, not a configuration
option. The invariant exists so that the first "can we just add…" conversation has something to
hit.

## Composition

All controls restrict, so they compose by most-restrictive, uniformly, with no exceptions:

| Control | Composes by |
| --- | --- |
| Execution limits — steps, duration, concurrency, result size, evidence window, cost ceiling | minimum |
| Time-window limits | minimum |
| Permitted connections and capabilities | intersection |
| Namespace, resource and destination exclusions | union |
| Redaction, retention and model-placement requirements | strictest applicable |
| Automatic-start permission | logical AND |

No Environment and no Relay may widen an Organization restriction. A Relay may be stricter than the
control plane requested.

**Severity mapping is not a control.** It is a translation, and two conflicting translations have
no most-restrictive answer. It lives beside its trigger source with its attribution intact. An
exception inside a security-shaped rule is where the first breach lives, so the rule has none.

## One configurable layer, not three

There are not three customer-configurable policy layers. The OpenCluster-managed control plane owns
planner behaviour and internal runtime defaults, and those are not customer settings. **The Relay's
local configuration is the customer's surface** — which is right, because the SRE editing it is
already editing it to install the Relay.

Organization and Environment guardrail administration is deferred until a real design partner
requires it. The composition rule is specified now; the administration product is not built
prematurely. When it lands it goes to Settings → Investigation controls, never to top-level
navigation.

## Consequences

- **Every Investigation pins its fully resolved execution-control snapshot**, beside the evidence
  plan snapshot and the component versions it already pins. "Why did this round stop after two
  requests" stays answerable from the case file forever, without access to current configuration.
- **A stricter Relay bound is recorded, and evidence missing because of it becomes a CoverageGap.**
  Hidden truncation is the specific thing SREs distrust; capability results already report the
  effective bound actually applied, so the customer's own configuration becomes visible in their
  investigations rather than invisible in a file.
- **Limits apply per round and cumulatively per Investigation** (ADR-013). Per-round bounds alone
  leave a long episode with sparse rechecks unbounded in aggregate.
- **Structural properties are never configuration keys.** The Relay is read-only by construction —
  a closed compiled capability registry, no command strings, no shell — and credentials never leave
  the customer environment because of where it runs. Neither may appear as a settable field: a key
  implies a false value is reachable, and "it is ignored" is a worse answer to a security reviewer
  than the key not existing. Both belong in the configuration reference as properties.
- **Cost ceilings are an operator fact, never a currency.** The moment the interface says
  "12 investigations remaining this month" the product has acquired the thing its users hate.
- A capability denied by local policy produces a gap reading *capability not permitted by local
  policy*, never *not available*. Those are different sentences and only one tells an operator what
  to do.
- Fine-grained delegation of control administration is blocked on ADR-006. A permission model over
  an anonymous shared token is theatre, and an audit trail that records "shared token" for a year
  is worth nothing later.
