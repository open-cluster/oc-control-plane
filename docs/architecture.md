# Architecture

The control plane is one binary with four listeners and one database schema per
placement. Everything an operator or a customer's system can observe crosses one of the
four; everything durable crosses `internal/storage`.

## The shape

```
customer alerting ──► intake listener ──► signal/episode pipeline ─┐
customer cluster ──► relay endpoint (gRPC, outbound from the relay)│
operator console ──► operator listener (route table + permissions) ├─► storage (placements)
k8s / prometheus ──► health listener (liveness, readiness, metrics)┘
```

- **Composition root** (`cmd/controlplane`): explicit construction, no DI container. It is
  the only place that knows every integration provider and the only place that builds the
  model boundary.
- **Domain packages** own their vocabulary and declare the store interface they need;
  `internal/storage` implements those interfaces against the placement pools.
- **Provider packages** (`internal/integrations/{alertmanager,kubernetes,slack,github}`)
  are self-contained and hold exactly what their type needs: every one a definition and
  its verification; alertmanager its webhook adapter; slack and github a vendor client
  and tools. Kubernetes reads through Relay capabilities rather than tools. The core
  imports none of them.
- **The model boundary** is declared in `internal/investigation` and implemented by
  `internal/reasoning` over vendor adapters (`anthropic/`, `zai/`). The domain never
  learns a vendor exists.

## Decisions that still hold

Each of these was made deliberately; a change to one is a design decision, not a cleanup.

| Decision | Why |
| --- | --- |
| `org_id` is the only tenant identity; composite `(org_id, id)` foreign keys everywhere | A cross-tenant reference must be structurally impossible, not filtered |
| Placements resolved from the organization, never ambient | Serving an unassigned tenant from a shared pool is the failure the design exists to prevent |
| One behavioural test seam: the composed process against real Postgres | A mock database proves the mock; provider transports are the one added seam |
| The catalog is compiled; `integration_type` rows are migration-seeded; a test holds them identical | Reference data must not need runtime synchronisation machinery |
| Providers assembled only at the composition root; no switch over types | Provider behavior must not leak into shared modules |
| Sessions are opaque server-side rows, not JWTs | Sign-out and revocation must take effect immediately |
| Audit rows commit in the change's own transaction | An unattributable operation must never happen |
| Persisted enums are frozen integers with a build gate | A reordered constant silently relabels every stored row |
| Investigations persist provenance, never chain-of-thought | What an operator audits is what was done and found, not how a model deliberated |
| The tool universe derives from verified grants; the model chooses only among offered tools | Availability must reflect verified reality, and tool choice is what tool metadata is for |
| One investigation loop: an autonomous conversation over every offered source | The architecture was settled by a scored evaluation; the losers were deleted, never kept behind a switch |

## Data model, briefly

`integration_type` (reference) ◄─ `integration` (per-organization) ◄─ `signal`,
`incident_episode`, `relay_job`, `change_ledger*`, `investigation_source`,
`investigation_tool_run`. `investigation` points at the episode that triggered it; the
episode does not point back. Identity/RBAC and relay tables stand beside them. Migrations
are embedded, forward-only, and applied under an advisory lock at startup.
