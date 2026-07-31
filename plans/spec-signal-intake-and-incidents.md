# Spec — Signal intake and incidents

Status: FIRST INCREMENT IMPLEMENTED 2026-07-29, and REBUILT ON CONNECTIONS 2026-07-31. Intake to
a durable Signal is built and in CI. The second increment — incidents, grouping and the
human-initiated path — is no longer blocked, and is deferred by ADR-008 rather than by a missing
model: it waits on evidence from the first investigation, not on anything intake needs.
Date: 2026-07-27, first source chosen 2026-07-29
Repository: the Go control plane

> **The blocker recorded here on 2026-07-30 is RESOLVED as of 2026-07-31.** It read: ADR-003
> states that incidents and investigations inherit their environment from the connection that
> discovered them and are never assigned one directly, and neither Environment nor Connection
> existed — so an Incident built then would have had to be given a scope some other way, creating
> the second grouping authority ADR-003 was written to prevent. The `alert_source` this increment
> introduced was itself a Connection under another name, introduced without reference to the model
> because the model was not there.
>
> Both now exist (`spec-environments-and-connections.md`). `alert_source` is gone; a delivery
> names its Connection and nothing else, and every Signal carries the Environment of the
> Connection it arrived through. What this document describes below as an "alert source" is a
> **trigger Connection**, and the two are the same thing under the name the model now uses.
>
> **First increment, delivered 2026-07-29: intake to a durable Signal.** An Alertmanager
> delivery authenticated by its source's shared secret becomes a normalised Signal, or is
> refused with a status that says whether to retry.
>
> **Asserted by tests at the composition root** — stories 1, 7, 8, 11, 12, 18, 20, 23 and 28:
> normalisation; the credential check in both directions; idempotent redelivery; a resolution
> updating the episode it resolves without erasing when that episode began; the same alert
> firing again recorded as a NEW episode rather than overwriting the resolved record of the
> last one; a late redelivery of a firing not resurrecting a resolved episode; malformed,
> oversized and truncated payloads handled without a partial write; a delivery naming another
> organization refused while holding the real secret; a database outage answered as retryable
> rather than as a refusal; and that a delivery is logged with its reason and origin but never
> with its payload or a presented credential.
>
> **True by construction but asserted by nothing** — stories 16, 17, 21, 26. The secret is
> stored only as a digest, verification runs before the body is read, vendor shapes exist only
> inside an adapter, and a new source is one adapter. Each is visible in the code and none has
> a test that would fail if it stopped being true.
>
> **Partly met, and the gap matters** — story 10 asks to verify the SIGNATURE of incoming
> alerts; what is implemented is a shared secret, for the reason set out under the Alertmanager
> decision below. Story 19 asks intake be bounded in size AND rate; only size is bounded, so a
> holder of a valid secret can still deliver as fast as it likes.
>
> **Not built.** Story 9 — a source is configured by inserting a row, because there is no API
> yet; that is a gap in the operator surface rather than in intake. Story 22 — the received
> payload is retained only as a digest, which identifies a redelivery but diagnoses nothing, so
> a normalisation mistake still cannot be checked against what actually arrived. Incidents and
> grouping (5, 6, 24, 25), the human-initiated path (3, 4, 30), storm shedding (14, 15),
> delivery health as an operator surface (13), and intake metrics (27). Story 2 waits on the
> investigator, which this document puts out of scope.

## Problem Statement

Nothing creates an incident.

The product's pipeline begins with an alert, becomes an incident, and triggers an
investigation. The alerting engine that produced the first stage is out of scope — building
detection would rebuild what every customer already runs, and would contradict the position
that this is an investigation platform rather than a monitoring one. So the first stage is
simply absent, and the rest of the pipeline has nothing to act on.

Customers already have alerting. It fires into a webhook and they route it to a pager. What
they do not have is anything that starts investigating before a human arrives. That is the
gap, and reaching it requires accepting their alerts rather than replacing them.

Accepting them is not a thin adapter, because each system has its own payload, its own
signing scheme, its own retry behaviour and its own idea of what constitutes the same alert
firing twice. Handled naively, every one of those differences leaks into the incident model
and the investigator ends up reasoning about vendor quirks.

There is also a second trigger that matters more than its volume suggests. The external path
inherits the customer's alert quality: if their alerting is noisy or absent for a failure,
the product looks blind for exactly the incident where it would be most useful. An engineer
who suspects something and wants it investigated has no alert to wait for.

## Solution

Two ways in, one model out.

Alerts arrive by webhook from the systems customers already run. Each source has an adapter
that verifies that source's signature, deduplicates by that source's notion of identity, and
normalises into one internal Signal. Past the adapter, nothing knows which system sent it.

Signals group into Incidents — the operational problem an investigation attaches to — with
grouping conservative enough that a mistaken merge is rarer than a mistaken split, because a
wrong merge produces an investigation with an incoherent scope.

An engineer can also open an investigation directly, naming a scope and a window, with no
alert involved. That path exists to escape the ceiling the external path inherits.

## User Stories

1. As an on-call engineer, I want alerts from my existing alerting to reach the platform, so
   that I do not have to replace my monitoring to use it.
2. As an on-call engineer, I want an investigation already under way when I open a page, so
   that I start from evidence rather than from nothing.
3. As an on-call engineer, I want to request an investigation directly when I suspect
   something, so that I am not limited to what my alerting happened to catch.
4. As an on-call engineer, I want to request an investigation over a past window, so that I
   can investigate something I noticed after it resolved.
5. As an on-call engineer, I want related alerts grouped into one incident, so that a single
   failure does not open twenty investigations.
6. As an on-call engineer, I want conservative grouping, so that two unrelated failures are
   not investigated as one and given an incoherent scope.
7. As an on-call engineer, I want an alert that resolves to be reflected on its incident, so
   that I can see what recovered.
8. As an on-call engineer, I want a late-arriving alert to attach to its incident without
   rewriting what was already recorded, so that the history stays honest.
9. As an operator, I want to connect an alerting source without writing code, so that
   onboarding is configuration.
10. As an operator, I want the platform to verify the signature of incoming alerts, so that
    an unauthenticated caller cannot inject incidents.
11. As an operator, I want a rejected delivery to return a status my alerting will retry on
    when appropriate and not otherwise, so that retries do not amplify a permanent failure.
12. As an operator, I want redelivery of the same alert to be idempotent, so that at-least-once
    webhooks do not create duplicate incidents.
13. As an operator, I want to see that a source is delivering successfully, so that I can tell
    a quiet night from a broken integration.
14. As an operator, I want a burst of alerts not to open an unbounded number of investigations,
    so that a storm does not become a cost incident.
15. As an operator, I want intake to keep accepting during a storm even when investigation is
    shed, so that the record is complete even when the response is throttled.
16. As a security reviewer, I want the signing secret for each source stored so that it cannot
    be read back, so that a disclosure does not yield the ability to forge alerts.
17. As a security reviewer, I want an unverified payload rejected before it is parsed as far as
    possible, so that the parser is not an attack surface for unauthenticated callers.
18. As a security reviewer, I want the alert payload treated as untrusted throughout, so that
    text a customer's system emitted cannot become an instruction downstream.
19. As a security reviewer, I want intake bounded in size and rate, so that it cannot be used
    to exhaust storage or compute.
20. As a security reviewer, I want an alert to be attributed to the organization whose source
    delivered it and no other, so that one tenant cannot create incidents in another.
21. As an engineer, I want vendor payload shapes confined to their adapter, so that adding a
    source does not touch the incident model.
22. As an engineer, I want the normalised Signal to retain a reference to what was received,
    so that a normalisation mistake is diagnosable after the fact.
23. As an engineer, I want both the source's timestamp and our receipt time recorded, so that
    a delayed delivery is distinguishable from a delayed failure.
24. As an engineer, I want incident grouping to be explainable, so that a surprising grouping
    can be understood rather than argued about.
25. As an engineer, I want grouping decisions to be revisable without rewriting history, so
    that correcting a mistake does not destroy the record of it.
26. As an engineer, I want adding a source to be a bounded, well-shaped piece of work, so that
    breadth does not require redesign.
27. As an on-call engineer for the platform itself, I want intake volume, rejection reasons and
    grouping outcomes visible as metrics, so that a broken integration is diagnosable.
28. As an on-call engineer for the platform, I want a rejected delivery logged with its reason
    and source but not its payload, so that diagnosis does not become a disclosure channel.
29. As the founder, I want one source supported well before several are supported poorly, so
    that the shape is proven before it is repeated.
30. As the founder, I want the human-initiated path available from the start, so that the
    product is demonstrable without depending on a customer's alerting quality.

## Implementation Decisions

**One source first, done properly.** The first adapter establishes the shape: signature
verification, deduplication identity, normalisation, and its own tests. Subsequent sources
are then a known quantity. Building three adapters at once produces three half-shapes and a
model that accommodates all of them badly.

**The first source is Prometheus Alertmanager** (founder decision, 2026-07-29). It is what
the target customer already runs, its payload is a documented and stable contract, and it
carries its own notion of identity — a per-alert `fingerprint` within a `groupKey` — so
deduplication does not have to be invented.

**Alertmanager has no signing scheme, and that changes what "verify the signature" can mean.**
It can attach static headers to a webhook and nothing more: there is no HMAC over the body and
no timestamp to bind. So authentication is a per-source shared secret presented in a header,
stored only as a digest and compared in constant time — the same shape the operator surface
uses. This is weaker than a signature in two specific ways, and both are worth stating rather
than discovering: the secret is replayable by anyone who captures one request, and it
authenticates the sender but attests nothing about the body.

What it buys is narrower than story 10 asks for, and worth stating as such rather than
declaring sufficient. An unauthenticated caller cannot inject signals, which is the property
that matters most. What is lost is body integrity: the payload is authenticated by nobody, so
story 18's "treat the alert payload as untrusted" now has to carry weight it was not written
to carry alone.

Two mitigations are real and one is a deployment obligation rather than code. Deduplication
makes a replayed delivery idempotent rather than a second signal — implemented and tested. The
secret is per-source, so a disclosure is bounded to one source — implemented. Transport
security is NOT provided by this process: intake serves plain HTTP and is expected behind a
TLS-terminating edge, exactly as the relay endpoint is. That is a deployment requirement, and
a deployment that publishes intake without terminating TLS in front of it is handing out a
replayable credential in cleartext.

Two things are consequently owed and not yet built: a minimum length enforced on a source's
secret at the point it is created (the operator token has one; this has no creation path to
enforce it in yet), and a rate limit, which is the only remaining defence once a credential
leaks. A source that can sign gets signature verification when its adapter is written; the
verification step is per-adapter for exactly this reason.

**A vendor payload shape exists only inside its adapter.** Nothing downstream of
normalisation can tell which system delivered a Signal. This is the boundary that keeps
breadth cheap, and it is worth enforcing structurally rather than by convention.

**Deduplication uses the source's own notion of identity**, because each system already has
one and inventing a different one guarantees disagreement about what "the same alert" means.
Redelivery is idempotent at the intake boundary, before anything downstream observes it.

**Verification precedes parsing** as far as the signing scheme allows. An unauthenticated
caller should reach as little of the parser as possible.

**Signing secrets are write-only.** They are configured and used; no path reads them back.

**Both timestamps are retained** — when the source says it happened, and when it arrived here.
Collapsing them makes a delayed delivery indistinguishable from a delayed failure, which
matters because the investigator reasons about ordering.

**The received payload is retained by reference**, bounded and redacted, so a normalisation
mistake can be diagnosed against what actually arrived rather than against what was inferred.

**Grouping is conservative and explainable.** A wrong merge is worse than a wrong split: a
split produces two investigations, one of which is redundant, while a merge produces one
investigation with an incoherent scope, which is the failure the truth model treats most
seriously. Every grouping decision records why.

**Grouping is revisable without rewriting history.** A merge creates a new grouping
relationship and preserves the original identities and their records; a split marks what was
reassigned and what remains ambiguous. Nothing is silently rewritten.

**Alert text is untrusted for its whole life.** It originates in a customer's systems, may be
attacker-influenced, and must never become an instruction, a destination, or an authorisation
claim downstream.

**Intake and response are separated under load.** A storm may shed investigation, but intake
keeps recording, because losing the record of a storm is worse than delaying the response to
it.

**The human-initiated path names a scope and a window and needs no incident.** It is not an
afterthought: it is what makes the product useful when a customer's alerting is silent, which
is precisely when investigation is most valuable.

**Correctness comes from specification and tests, not comparison.** There is no reference
implementation for any of this. The existing incident model was produced by alert evaluation
and shaped around rule-series episodes; it is not the thing being built, and treating it as
an oracle would carry that shape forward.

## Testing Decisions

**What makes a good test here.** It asserts what an operator or an engineer could observe: a
delivery was accepted or rejected with a particular status, a Signal appeared with normalised
content, two deliveries produced one Signal, alerts landed on one incident or two. It does not
assert how normalisation is implemented, because adding a source will change that.

**The seam is unchanged: the composition root**, with real HTTP and a real database. Webhook
deliveries are made as real signed requests. There is no mock verifier and no mock database —
signature verification is exactly the thing that must not be faked.

**Adapter behaviour is tested through intake, not around it.** An adapter is reached by
delivering a request, because that is how it is reached in production, and its signature
verification is only meaningful against a real request.

**Scenarios.**

- A correctly signed delivery is accepted and produces a normalised Signal.
- An incorrectly signed delivery is rejected, and no Signal is produced.
- An unsigned delivery is rejected.
- A replayed delivery produces no second Signal.
- A delivery for one organization cannot produce a Signal for another.
- A malformed payload behind a valid signature is rejected without a partial write.
- An oversized payload is rejected without being buffered whole.
- The status returned distinguishes retryable from permanent, and matches what the source's
  retry policy needs.
- Related alerts group into one incident; unrelated alerts do not.
- A resolution updates its incident without erasing the firing record.
- A late-arriving alert attaches without rewriting existing records.
- A human-initiated request opens an investigation with the named scope and window and no
  incident.
- A burst produces bounded investigations while every delivery is still recorded.
- No log line or trace attribute contains the payload or the signing secret.

**Prior art.** The existing webhook receiver in the reference implementation establishes the
verification-then-dedup-then-correlate shape and the discipline of never fabricating an
outcome for an uncorrelated event. The foundation's composition-root suite provides the
harness. The reference incident store shows what to model differently rather than what to
copy.

## Out of Scope

- Any alerting engine: rules, evaluation, thresholds, silences, routing, notification.
  Building these is what the product deliberately does not do.
- Sources beyond the first adapter. The second is a separate, smaller piece of work.
- Chat-initiated investigation. It requires resolving a human-typed resource name to a
  canonical resource, and canonical resource identity does not exist yet. Shipping it first
  would confidently investigate the wrong thing.
- The investigator itself. This produces incidents for it to act on.
- Notification of investigation outcomes.
- Incident user interface. The frontend is a separate project.
- Migrating the existing incident data. The model is being redesigned, not carried over.
- Automatic severity or priority inference.

## Further Notes

This is the first piece of work in the migration with no reference implementation to compare
against, which changes how confidence is obtained: from differential comparison to
specification and adversarial tests. It is worth being explicit that this is a weaker form of
assurance, and worth compensating with more attention to the rejection paths than a happy-path
suite would suggest.

The dependency on canonical resource identity is worth stating plainly because it will
resurface. Chat-initiated investigation, topology, and cross-source correlation all need to
know that the thing one system calls one name and another system calls another name are the
same object. That problem has one line of design and is the largest unsolved question in the
product. Nothing here depends on it, and that is deliberate.
