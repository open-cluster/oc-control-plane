# The frontend is organised around the case file, not around infrastructure

Status: ACCEPTED (2026-07-31 — founder decision in the frontend architecture grilling session)
Supersedes: the frontend's current direction. `opencluster-web` is built against the frozen .NET
API and carries observability-era pages; a new application is written against the Go control
plane. This also closes the open question recorded as assumption 3 in the implementation status.

The **Investigation case file** is the product. It is durable, permalinked, shareable, exportable,
and understandable without navigating the rest of the application. Everything else in the interface
is setup.

The alternative — a management console with an investigations section — was drawn, reviewed and
rejected. Its navigation gave five of eight slots to things a platform lead touches once during
onboarding, and it required a continuously synchronised resource inventory to populate a Resources
page, which is the monitoring platform ADR-010 exists to prevent, arriving through the front door.

## Navigation

```
WORK            Home · Investigations
OPERATIONS      Sources · Investigation readiness
ADMINISTRATION  Settings
```

**Sources** unifies alert intake (trigger inputs), evidence connections (capability providers) and
Relay health. The distinction between a system that tells you something happened and one that lets
you find out why is real, and the two have different failure modes. **Investigation readiness**
stays separate despite overlapping: *is this source healthy* is operational, *can we investigate
production right now* is what a platform lead actually asks, and merging them buries the second.

Not present in v1, and the exclusions are part of the decision: **no Overview dashboard, no
Resources browser, no Services catalog, no Topology page, no Alerts section, no Incidents section,
no Investigation Policies item.** Resources and topology appear contextually inside investigation
creation and inside case files. Home is minimal — start an investigation, the queue, readiness
problems, an optional example case — and any future Home widget must prove it improves the
investigation workflow rather than resembling monitoring.

## What the case file may claim

- **Coverage is modelled by typed evidence capability, not by vendor.** Kubernetes, Proxmox,
  Zabbix and Prometheus are providers of capabilities — runtime state, metrics, logs, changes,
  topology, network. Per investigation each relevant capability is checked-with-evidence,
  checked-and-empty, checked-but-incomplete, relevant-but-unavailable, or not-applicable. Only
  relevant unavailable capabilities become coverage gaps; a product the customer does not run never
  appears as a missing source.
- **Gaps render beside the hypothesis or timeline event they limit**, and aggregate in the coverage
  view. No fixed empty section per possible integration.
- **No confidence meters, no numeric coverage percentages, no calibrated probability.** The stated
  basis carries it: supporting findings, contradictions, missing checks, independent sources,
  evidence quality and freshness, and alternatives considered with reasons. Internal factors may
  rank hypotheses; the ranking surfaces as an ordinal, never as a score. Revisit only when real
  calibration data exists.
- **Impact appears only when supported by cited EvidenceItems.** No user counts or request counts
  from telemetry that was never checked. Severity is inherited from the trigger with its source
  named, or set by the human who started the run, or absent — never inferred.
- **Every claim links to the evidence that produced it.** The output schema already makes an
  uncited claim impossible; the interface makes that visible.
- **Abstention renders with equal visual weight to a finding.** If the honest outcome looks like a
  failure state, the product gets tuned to stop being honest.
- Never the words "root cause". A **VerifiedCause** is visually distinct from a best-supported
  explanation, and is rare.

## Consequences

- **One renderer, one permission-aware read model.** In-app view, chrome-free shared route and both
  exports differ in chrome and in nothing else. The scenario harness's scored artifact is the same
  artifact, so the reader is built once for the product, the evaluation and the postmortem.
- **Share is internal in v1** — an authenticated deep link requiring Organization membership and
  Environment authorization, labelled as such. Externally readable signed links are the target
  model and are blocked on ADR-006 identity, along with control delegation and any audit trail
  worth having.
- Exports are self-contained HTML preserving claim-to-evidence navigation, plus Markdown, each
  stamped with investigation identity, citations, coverage gaps, source timestamps, planner and
  model versions, and export time. An artifact that travels carries its own caveats.
- **Reads are a lightweight summary plus lazily loaded sections**, every response carrying the
  whole-case version. The browser polls the summary — 2 to 3 seconds while investigating, 15 to 30
  while waiting, stopping when complete — and invalidates only the visible tab. Streaming is a
  later swap behind the same read model. Payload boundaries are set by measurement against small,
  medium and long-running cases, not by estimate.
- Pause means "stop updating the view" and is never adjacent to Cancel, which stops the
  Investigation.
