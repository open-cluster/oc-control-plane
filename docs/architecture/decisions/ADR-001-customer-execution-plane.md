# ADR-001: Customer execution plane — central reasoning, in-cluster typed Relay

Status: ACCEPTED (2026-07-20 — founder decision, with modifications to the revision-2
proposal; revision 3 records the accepted form)
Date: 2026-07-20 (revision 3 — founder modifications applied; review record in the plan,
section 7)
Deciders: founder (decided 2026-07-20); prepared by architecture sessions 2026-07-20
Details: `docs/architecture/customer-execution-plane.md`
Plan: `plans/stage-1b-customer-execution-architecture.md` in the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService)

This is the repository's first ADR; it establishes `docs/architecture/decisions/` with
sequential `ADR-<number>-<slug>` naming. Prior decisions remain recorded in plan documents and
the implementation-status decision log.

## Context

Stage 1B needs customer-environment execution: Kubernetes reads run today through central
readers using customer kubeconfigs resolved from control-plane disk (a deliberate dev-mode
seam, never wired into any production composition root); the master plan requires
DNS/TCP/HTTP diagnostics from a "pre-provisioned Relay probe"; container logs and the
revision ledger are next. The hosted product cannot ship on central kubeconfig custody: it
concentrates every customer's cluster credentials on vendor disk, requires
internet-reachable API servers, and cannot reach in-cluster Prometheus/Loki or cluster DNS
context. HolmesGPT (analyzed at commit `3bccb1d`, Apache-2.0) runs the entire AI agent
in-cluster — which fragments state, is structurally single-cluster, and cannot represent
coverage gaps or certified negatives. OpenCluster's moat is the central durable truth
layer; the architecture must keep it central while gaining cluster-local execution.

The revision-2 proposal (six independent reviews, 6/6 APPROVE) recommended a .NET Relay,
HTTPS long-poll v1, an OpenAPI-authored contract, and a permanently retained `direct`
Kubernetes execution mode. The founder accepted the target structural model and modified
those four points; this revision records the accepted decision.

## Decision

1. Reasoning, scheduling, and all investigation truth stay in the private .NET control
   plane. No model code, model credentials, or investigation state in customer
   environments.
2. One customer-side component, the OpenCluster Relay, executes typed, versioned, bounded,
   read-only jobs — a closed compiled capability registry with CI-enforced banned-API and
   schema-shape gates; no command strings, no shell, no dynamic code, no generic remote
   procedure mechanism — and returns bounded structured results.
3. The Relay is implemented in Go and is open-source by design under Apache-2.0. The
   control plane remains private .NET. The Relay lives in a separate sibling Git
   repository (`opencluster-relay`) created with clean history, under a provisional module
   and image identity until naming clearance completes; it is not pushed or made public
   without explicit founder authorization.
4. Transport is an outbound-only, bidirectional gRPC stream over TLS on port 443 from the
   first production protocol version: bootstrap-token registration, hashed per-relay
   credentials held in a Relay-owned pre-created Secret (the single narrow RBAC
   exception), SPKI endpoint pinning, one authenticated session stream carrying closed
   `oneof` envelopes, and durable server-side jobs with fenced leases and lease epochs
   under a single-clock contract. The stream is a delivery channel; PostgreSQL job state
   is the source of truth. A focused ingress/proxy feasibility gate runs before full
   commitment; a blocking incompatibility returns an evidence-backed design decision
   rather than a silently introduced second transport.
5. Protobuf is the single language-neutral protocol source of truth, owned by the Relay
   repository under `proto/opencluster/relay/v1/`, managed with a reviewed Buf toolchain
   (format, lint, breaking-change detection, deterministic generation). Go and C# types
   are generated or verified from the same contracts; no manually duplicated transport
   DTOs. `google.protobuf.Any`/`Struct`/`Value`, map-based argument payloads, raw JSON
   payloads, and command/script/path fields are banned; every capability has a versioned
   typed argument and result message. REST/OpenAPI remains only for unrelated public
   control-plane APIs — never as a competing authority for the Relay protocol.
6. The Go Relay becomes the single production implementation of customer-private
   Kubernetes execution, for hosted and self-hosted alike. Kubernetes is Relay-only after
   migration. The existing C# Kubernetes implementation (OpenCluster.Kubernetes readers,
   kubeconfig credential custody) is temporary migration and differential-test
   infrastructure — the parity oracle — and must not become a second permanent production
   implementation. After semantic parity on a live cluster, routing cutover, and
   independent verification, the obsolete C# readers, kubeconfig custody, packages,
   composition, configuration, tests, and documentation are removed. The Go
   implementation reimplements the verified S1B-3/S1B-4 semantics (including the selector
   fix) idiomatically with client-go — never a mechanical translation.
7. Execution locality vocabulary in public contracts: `control_plane` (native OpenCluster
   data and public SaaS APIs) and `relay` (customer-private or cluster-local sources).
   The ambiguous term `direct` is not used in newly designed public contracts; it may
   persist only inside migration-era internal code slated for removal.
8. Relay-executed completeness is a distinct trust class: results carry a structured
   completeness proof; certified negatives resting solely on relay attestation are
   confidence-capped; cross-cluster negatives require every in-scope Relay complete. A
   customer-authored, server-immutable local policy (destination allowlist, local hard
   rate/volume caps, masking rules) is the independent second enforcement layer.
9. Three declared master-plan deviations stand as accepted (details: architecture
   document 3.5): Relay scope widened from diagnostics-only to the general customer
   execution plane; HTTP-path diagnostics deferred; revision-ledger capture is periodic
   list-and-diff in v1 with the continuous watch deferred to a streaming protocol mode
   (now a natural extension of the gRPC session rather than a second protocol).
10. Name: "OpenCluster Relay" (qualified two-word form publicly), subject to name
    clearance before any public tag (Sentry Relay is a close in-domain collision;
    "OpenCluster" vs CNCF Open Cluster Management is routed to the founder with OSS
    Phase 0). Provisional module/image identity until clearance.
11. Build scope: full-protocol walking skeleton (founder decision 1b resolved). The first
    Relay behavior is the `kubernetes.workload.runtime.v1` capability with differential parity and
    latency gates against the C# oracle on the same live cluster. DNS-first remains
    rejected as the opening slice; DNS/TCP/TLS follow as dedicated slices.

## Consequences

Positive: cluster credentials never leave customer environments; no inbound customer
connectivity; private API servers and in-cluster sources become reachable; multi-cluster
investigations become structural; the truth layer, replay, and evaluation are untouched;
one Kubernetes implementation in the end state — no permanent dual maintenance, no
drift-prone second reader; the Go/OSS positioning matches the cloud-native audience the
Relay must convince; gRPC gives immediate cancellation, server push (jobs, rotation,
drain), and a streaming path for the future watch mode without a protocol migration;
reproducible Go builds strengthen the supply-chain story a .NET Relay could not offer.

Negative / accepted costs: a second language in the program — the founder reviews and
maintains Go with weaker mutation tooling than Stryker.NET, and the verified C# reader
code is reused only as an oracle, not as production code (the revision-2 matrix priced
these two criteria highest; the founder consciously paid them); gRPC-first carries
HTTP/2 edge risks (proxies, load balancers, keepalive, reconnect storms) that long-poll
avoided — mitigated by a named risk register with per-risk tests and a feasibility gate,
not by hope; a second deployable customers install and upgrade (time-based support
window; advertised capability versions; execution-start re-validation); protocol surface
to secure and maintain; a compromised Relay can fabricate well-typed within-tenant
results — bounded and attributed via the trust class, not prevented; the hosted product
has no Kubernetes read path until R1 lands; until routing cutover and removal gates
actually complete, Kubernetes is NOT Relay-only and must not be reported as such.

## Alternatives considered

Full in-cluster investigator (HolmesGPT-style) — rejected: fragments truth/state,
single-cluster, per-cluster reasoning upgrades; the honest core objection is state and
replay, not LLM locality. Central-only direct connectors — rejected: credential custody,
reachability, no cluster-local context; survives only as migration-era oracle
infrastructure, then removed. Permanent dual K8s implementations (C# direct for
self-hosted + Go Relay for hosted) — rejected by founder decision: two implementations of
verified semantics drift; self-hosted runs the Relay beside the control plane or
in-cluster instead. .NET Relay in this repository (revision-2 recommendation) — modified
by founder: Go, separate clean-history repo; the revision-2 matrix itself recorded that
Go wins on pure engineering merits. HTTPS long-poll v1 with gRPC later (revision-2
recommendation) — modified by founder: gRPC from v1, with the feasibility gate carrying
the compatibility burden long-poll would have dodged. OpenAPI-authored contract —
superseded: a two-authority contract (OpenAPI plus the inevitable streaming schema) is
exactly the duplication the Protobuf single source of truth exists to prevent. Minimal
fixed-identity probe — closed: founder chose the full-protocol walking skeleton. Two
customer components (probe + separate K8s agent) — rejected: double
install/identity/skew for no isolation gain. Rich agent with local investigation
awareness — rejected: moves state and policy into the skew domain and dilutes the audit
model.

## Reversal triggers

- Transport: if the feasibility gate proves the intended edge stack cannot carry reliable
  outbound gRPC (HTTP/2 not preserved, unfixable idle/keepalive behavior), STOP and
  return an evidence-backed transport decision to the founder. No long-poll fallback is
  built without its own review.
- Language: none active. The Go decision is the founder's accepted end state; the
  Protobuf contract keeps any future port honest, but no .NET-Relay reversal is on the
  table.
- mTLS: pull earlier on enterprise demand or security-review escalation; the
  stolen-credential threat row is the standing justification.
- Relay scope: if design partners prove a needed on-prem source the typed model cannot
  express, re-open the capability contract — never with generic command execution.
- Naming: if clearance fails on "Relay", rename the component before any public artifact
  exists; the architecture is name-independent.
- C# retirement pacing: if differential parity or the feasibility gate stalls, the C#
  oracle stays alive longer — but it never re-enters a production composition root, and
  the removal gates remain the exit condition.
