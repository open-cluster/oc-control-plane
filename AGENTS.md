# Working in this repository

This file is for coding agents and new contributors alike: what this service is, where its edges are, and what must be
true before a change ships.

## What this is

The OpenCluster control plane: durable truth for organizations, Integrations, Alert Events, incidents and
investigations, plus one HTTP listener and one Relay gRPC listener. Customer-side execution lives in the separate Relay
repository; this service speaks its protocol and never touches a customer cluster.

## Vocabulary

`CONTEXT.md` is the glossary. Use its words exactly; the banned synonyms it lists fail review. The retired architecture
(Placements, Connections, Environments, evidence chains) must not reappear under any spelling.

Use `operator` only for a human actor. Use `API`, the resource name, or the transport for software surfaces.

## Build, test, verify

```
make tools     # pinned linters and scanners, once
make verify    # lint + build + test + vuln + licenses — the whole CI gate set
make test      # race detector; needs a reachable Docker daemon (Testcontainers)
make test-short # unit tests only, no Docker
```

Run `make verify` before calling any change done. The integration suite starts real Postgres containers and real
listeners; nothing mocks the database.

## Boundaries the build enforces

- Only `internal/store/postgres` touches the deployment database; every tenant-owned query keeps Organization explicit
  and predicates on `org_id`.
- The integrations core imports no provider; only `internal/app` assembles the catalog. No switch over integration types
  anywhere.
- `internal/investigation` never imports `internal/investigation/agent`; reasoning implements the domain's boundary, and
  vendors appear only in adapter subpackages.
- No Kubernetes library in this module; cluster access belongs to the Relay.
- Persisted enum values are frozen; extending one starts in `test/architecture`.
- Every application API route is declared `(method, pattern, permission)` in a `Routes()` table; a mux registration
  anywhere else fails the gates.
- Secrets: accept direct environment values or optional _FILE inputs, reject conflicts, and redact errors; inbound
  secrets are digests; presentable credentials are sealed via `internal/seal`; audit details drop credential-shaped keys
  mechanically.

## Style

- Do not overcomplicate the code, do not create too much nested code (objects classes inside a class file).
- Do not use too much destructive comments.
- Prefer simplicity over overcomplication (Use best practices like DRY, KISS, etc)
  Meaning that the code is simple and easy to understand (namings, everything) to human developers. Keep the code clean
  and concise. Comments state constraints, invariants, or non-obvious reasons, in plain words — never what the next line
  does, never the history of a change. Doc comments follow Go convention. Tests assert observable behavior at real seams
  (the composed process, the wire, the database), not call sequences.
- Do not remove user written comments that explain the part of the code.

## Engineering and performance

- Prefer idiomatic Go and the smallest implementation that satisfies the requirement.
- Organize code by product responsibility, not by abstract layers or patterns.
- Introduce interfaces only where a consumer needs one.
- Avoid wrappers, helpers, configuration, and new dependencies that do not remove meaningful complexity.
- Keep HTTP, domain behavior, agent execution, and persistence responsibilities separate.
- Do not move code solely to shorten a file.
- Prefer standard library functionality when sufficient.

## PostgreSQL

- Use parameterized queries and explicit tenant scoping.
- Preserve transaction, audit, and authorization invariants.
- Prefer named SQL queries for substantial static statements.
- Keep dynamic SQL only when justified, with allowlisted identifiers and parameterized values.
- Keep database row models private to persistence.
- Avoid N+1 queries, unbounded reads, and unnecessary joins.
- Do not introduce indexes without considering query plans, existing indexes, and write overhead.
- Do not replace pgx or introduce an ORM without evidence.

## Performance

- Optimize measured bottlenecks, not theoretical ones.
- Establish a baseline before changing a hot path.
- Use Go benchmarks, profiles, or representative query plans.
- Consider CPU, memory, allocations, I/O, concurrency, latency, and operational costs.
- Prefer simpler algorithms and fewer database round trips.
- Do not add caches, pools, or goroutines speculatively.
- Re-measure after optimization and report the difference.

## Security and correctness

- Preserve Organization isolation across reads and writes.
- Treat external tool results and provider data as untrusted.
- Never expose secrets or restricted evidence through APIs, streams, logs, Slack, or agent context.
- Preserve idempotency, lease fencing, and cancellation.
- Prefer focused tests at observable boundaries.
- Never silently change externally visible behavior during an architecture-only refactor.

## Documents

| Where        | What                                                                                                                                          |
|--------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| `README.md`  | Repository overview, status, shortest development path, and contributor pointers                                                              |
| `CONTEXT.md` | The glossary; every identifier uses it                                                                                                        |
| `docs/`      | User-facing product documentation: Mintlify MDX pages, `docs/docs.json` and the named site chrome. Mintlify publishes this directory directly |

`docs/` is product documentation only, written for an SRE who has never read this code:
task-oriented, outcome-first, no internal vocabulary (package names, table names, sealed blobs, agent plumbing). Every
supported integration type must have
`docs/integrations/<product-role>/<key>.mdx`, in role-based navigation, matching shipped behavior — an integration is
not done without its page, and the docs gate plus the Mintlify build in CI enforce the structure. Plans, scratch notes,
and working state stay out of the repository; specifications for unbuilt work live on the issue tracker, and version
control is the archive.
