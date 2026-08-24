# Working in this repository

This file is for coding agents and new contributors alike: what this service is, where
its edges are, and what must be true before a change ships.

## What this is

The OpenCluster control plane: durable truth for organizations, Integrations, Signals,
incidents and investigations, plus four listeners — health, operator API, alert intake,
and the Relay endpoint. Customer-side execution lives in the separate Relay repository;
this service speaks its protocol and never touches a customer cluster.

## Vocabulary

`CONTEXT.md` is the glossary. Use its words exactly; the banned synonyms it lists fail
review. The retired architecture (Connections, Environments, evidence chains) must not
reappear under any spelling.

## Build, test, verify

```
make tools     # pinned linters and scanners, once
make verify    # lint + build + test + vuln + licenses — the whole CI gate set
make test      # race detector; needs a reachable Docker daemon (Testcontainers)
make test-short # unit tests only, no Docker
```

Run `make verify` before calling any change done. The integration suite starts real
Postgres containers and real listeners; nothing mocks the database.

## Boundaries the build enforces

- Only `internal/storage` touches the database; placements are resolved, never ambient.
- The integrations core imports no provider; only `internal/app` assembles the
  catalog. No switch over integration types anywhere.
- `internal/investigation` never imports `internal/reasoning`; reasoning implements the
  domain's boundary, and vendors appear only in adapter subpackages.
- No Kubernetes library in this module; cluster access belongs to the Relay.
- Persisted enum values are frozen; extending one starts in `internal/gates`.
- Every operator route is declared `(method, pattern, permission)` in a `Routes()` table;
  a mux registration anywhere else fails the gates.
- Secrets: environment variables name FILES, never values; inbound secrets are digests;
  presentable credentials are sealed via `internal/seal`; audit details drop
  credential-shaped keys mechanically.

## Style

Comments state constraints, invariants, or non-obvious reasons, in plain words — never
what the next line does, never the history of a change. Doc comments follow Go
convention. Tests assert observable behavior at real seams (the composed process, the
wire, the database), not call sequences.

## Documents

| Where | What |
| --- | --- |
| `README.md` | Repository overview, status, shortest development path, and contributor pointers |
| `CONTEXT.md` | The glossary; every identifier uses it |
| `docs/` | User-facing product documentation: Mintlify MDX pages, `docs/docs.json` and the named site chrome. Mintlify publishes this directory directly |

`docs/` is product documentation only, written for an SRE who has never read this code:
task-oriented, outcome-first, no internal vocabulary (package names, table names, sealed
blobs, agent plumbing). Every supported integration type must have
`docs/integrations/<product-role>/<key>.mdx`, in role-based navigation, matching shipped
behavior — an integration is not done without its page, and the docs gate plus the
Mintlify build in CI enforce the structure. Plans, scratch notes, and working state stay out of the repository;
specifications for unbuilt work live on the issue tracker, and version control is the
archive.
