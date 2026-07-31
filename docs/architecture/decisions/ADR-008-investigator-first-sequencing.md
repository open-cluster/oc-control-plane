# The investigator is built before the model around it

Status: ACCEPTED (2026-07-30 — founder decision in the architecture grilling session)
Amended 2026-07-31 by ADR-013: the sequencing below stands, but the *model* of Investigation and
IncidentEpisode is designed now rather than later. See "Amendment" at the end.

The declared order was Environments and Connections, then Incidents and grouping, then
Investigation. That order is reversed. The next work is the smallest investigation that runs
against a real cluster and produces an evidence-cited explanation a human judges. Incident
grouping, environment management as a feature, and cross-provider canonical resource identity
are deferred until that first investigation has been evaluated.

The reason is one verified fact. Across the frozen .NET reference and both Go repositories,
roughly twenty-one thousand lines exist and no investigation has ever run. Stage 1A shipped six
verified slices of investigation bookkeeping — hypothesis stores, conclusion stores with
citations and confidence factors, an audited tool runtime — and the only implementation of
`IInvestigationExecutor` in the entire solution is `NotConfiguredInvestigationExecutor`, whose
body throws. The product's single load-bearing assumption, that bounded typed reads plus
reasoning can produce an explanation a senior engineer finds useful, has never been tested once.

Everything in the truth chain is a prediction about how that reasoning will fail. Some of those
predictions are certainly right and some are certainly wrong, and no amount of further design
distinguishes them. Only a run does.

## The path

Default Environment → Relay registration → minimal Kubernetes Connection → manually scoped
investigation → real Relay evidence → timeline → evidence-cited supported explanation.

Alongside it, a scenario harness: clusters broken in known ways, run end to end, output scored
by engineers who did not build it. The harness is not a test suite. It is the instrument that
decides whether the product works, and it is built in the same slice as the investigator.

## Consequences

- One capability is not enough. `kubernetes.workload.runtime` reports that a pod is failing and
  cannot report why. Kubernetes events and bounded container logs are on the critical path.
- The first investigation is triggered by a human naming a scope, not by an alert. Signal intake
  already exists and is not the bottleneck; nothing consuming a Signal is.
- An Incident entity is not built in this slice. An Investigation attaches to a Connection-scoped
  request. Grouping arrives when redundant investigations are an observed problem rather than an
  anticipated one.
- Environment survives as a mandatory scope carried by every row from the first migration, so
  there is no nullable retrofit later. What is deferred is environment *management* as a
  product feature; a Default environment is created automatically.
- Scope resolution for the first slice is deliberately narrow and hard-coded to one class of
  Kubernetes workload failure. That code is expected to be discarded. Writing down what is
  hard-coded is the obligation; generalising it early is not.

## Considered and rejected

**Keeping the declared order.** ADR-003's inheritance rule is the stated justification: an
Incident that cannot inherit an Environment must be assigned one, creating a second grouping
authority. The rule is right and the sequencing conclusion does not follow from it. There are no
incidents, no evidence, no deployments and no customers, so the retrofit being avoided is a
migration of rows that do not exist. The cost of the delay is three more weeks of truth-model
decisions made without a single observation of the reasoning loop failing — which is the specific
way this program already spent Stage 1A.

**Proving the reasoning offline on fixtures alone.** Necessary and not sufficient. Fixtures skip
the part the Relay exists to do, and the harder problem may well be obtaining the right evidence
rather than reasoning over it. The harness runs against real clusters through the real Relay.

## Amendment, 2026-07-31

The consequence above stating that "an Incident entity is not built in this slice" is narrowed
rather than reversed. ADR-013 establishes that an Investigation is a durable case holding many
bounded rounds, and that repeated notifications about one failure must not fragment into many
Investigations.

**The sequencing is unchanged.** The first slice is manually triggered and consumes no Signals, so
nothing can fragment yet, and no incident grouping, merging, splitting or routing is built.

**The model is not deferred.** The one-to-many between Investigation and InvestigationRound must be
structurally correct from the first migration, and `IncidentEpisode` must exist as a concept the
schema can accommodate — because retrofitting that relationship after rows exist is exactly the
retrofit this ADR argued about in the opposite direction. In v1 one IncidentEpisode has one primary
Investigation, and a manually started Investigation may use an implicit episode without surfacing
the distinction.

The distinction worth carrying: **deferring a feature is not the same as deferring a shape.** This
ADR was right to defer incident grouping and would have been wrong to defer the shape that grouping
eventually attaches to.
