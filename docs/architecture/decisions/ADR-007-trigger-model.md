# Incidents arrive from existing alerting; OpenCluster builds none

OpenCluster accepts alerts from the monitoring systems customers already run —
Alertmanager, Grafana, Datadog, PagerDuty and similar — verifies and deduplicates them,
normalizes them into Signals, groups Signals into Incidents, and starts Investigations by
policy. External intake is the primary production path. Human-initiated investigation is
supported alongside it, for suspicious behaviour with no alert, for historical
investigation, and eventually from a chat surface.

No general-purpose alerting engine is built. The product is an incident-investigation
platform, and owning detection would both contradict that positioning and rebuild what
every customer already has.

## Consequences

- A vendor payload shape exists only inside its own adapter. Each adapter owns its
  signature verification and its deduplication identity; nothing downstream of
  normalization knows which system sent the alert.
- Investigation quality is bounded by the customer's alert quality on the external path.
  The human-initiated path exists partly to escape that ceiling, which makes it a product
  requirement rather than a convenience.
- The incident model is redesigned rather than migrated. The existing incident is a
  rule-series episode produced by alert evaluation; the new one is an operational problem
  grouped from externally-sourced Signals.
- Deleting the alerting module removes 36 migrations and roughly 14,900 lines with no
  replacement obligation beyond intake and grouping.
- A chat trigger naming a resource in prose implies resolving that name to a canonical
  resource, so it is blocked on canonical resource identity. Shipping it earlier would
  silently investigate the wrong thing.

## Considered and rejected

**Human-initiated only.** Smallest possible intake and it would prove the investigator
soonest, but it delivers no ambient value: it works only when someone already knows to
ask, which is precisely the moment an AI SRE is least useful.

**Retaining a reduced internal alerting engine.** Would give a self-contained demo with no
external dependency, at the cost of reopening 36 migrations and making the product partly
the monitoring tool the pivot exists to stop being.
