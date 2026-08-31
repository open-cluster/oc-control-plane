<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./docs/logo/dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="./docs/logo/light.svg">
    <img alt="OpenCluster" src="./docs/logo/light.svg" width="344">
  </picture>
</p>

<p align="center">
  <a href="https://docs.open-cluster.io/"><img alt="Documentation status" src="https://github.com/open-cluster/oc-control-plane/actions/workflows/docs.yml/badge.svg"></a>
  <a href="./LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/github/license/open-cluster/oc-control-plane"></a>
</p>

<p align="center">
  <a href="https://docs.open-cluster.io/getting-started/quickstart">Quickstart</a> ·
  <a href="https://docs.open-cluster.io/api-reference/overview">API reference</a> ·
  <a href="https://docs.open-cluster.io/security/overview">Security</a> ·
  <a href="https://docs.open-cluster.io/developers/contributing">Contributing</a> ·
  <a href="https://discord.gg/pdFEgFr66">Discord</a> ·
  <a href="https://x.com/openclusterio">X</a>
</p>

OpenCluster is an open-source AI SRE that investigates production incidents across the systems you already use. It
gathers bounded read-only evidence, keeps competing hypotheses visible, and produces a structured conclusion that
separates impact, causal findings, proposed actions, and limitations.

Every material claim links back to a numbered Tool Run. OpenCluster never executes a mitigation: an action proposal
states its risk, reversibility, approval requirement, and verification procedure so an on-call engineer can decide
safely.

> OpenCluster is experimental pre-release software. APIs and storage may change without
> upgrade compatibility until the first stable release; recreate pre-release databases.

Licensed under the [Apache License 2.0](./LICENSE).

## Product workflow

1. Alertmanager sends an Alert Event.
2. OpenCluster groups it into an Incident and opens a Conversation and Investigation.
3. The investigator reads only authorized connected sources, including Kubernetes through an outbound customer-side
   Relay.
4. Operators watch hypotheses and operational progress while numbered Tool Runs execute.
5. The conclusion reports impact, findings, hypotheses, action proposals, and limitations.
6. After resolution, an operator can generate, correct, and review a draft Postmortem.

## Quick start

You need Docker with Docker Compose. Create local files for the database password, DSN, administrator bootstrap token,
model API key, and 32-byte sealing key. Secrets are mounted from files and never placed directly in environment values.

```bash
export OPENCLUSTER_POSTGRES_PASSWORD_FILE=/absolute/path/postgres-password
export OPENCLUSTER_DATABASE_DSN_FILE=/absolute/path/postgres-dsn
export OPENCLUSTER_BOOTSTRAP_TOKEN_FILE=/absolute/path/admin-bootstrap-token
export OPENCLUSTER_ENCRYPTION_KEY_FILE=/absolute/path/32-byte-encryption-key
export OPENCLUSTER_AI_API_KEY_FILE=/absolute/path/model-api-key
export OPENCLUSTER_AI_PROVIDER=anthropic
export OPENCLUSTER_AI_MODEL=claude-sonnet-5
export OPENCLUSTER_RUNTIME_UID=$(id -u)
export OPENCLUSTER_RUNTIME_GID=$(id -g)
docker compose -f deploy/compose/compose.yaml up --build
```

The PostgreSQL DSN must use host `postgres`, database `opencluster`, user `opencluster`, and the password stored in the
password file. The bootstrap token must contain at least 32 characters. Open the configured HTTP address, create the
first administrator, and connect Alertmanager.

Send a test alert, then connect at least one evidence source:

- Kubernetes through an optional Relay for workload runtime, namespace events, and logs.
- GitHub for repository and change evidence.
- Slack for operational testimony and follow-up conversations.

See the [quickstart](./docs/getting-started/quickstart.mdx) for the full walkthrough.

To enable the optional Relay transport, provide `OPENCLUSTER_RELAY_SPKI_PINS`, start Compose with `--profile relay`, or
set `relay.enabled=true` in the Helm release. The Relay initiates the connection; the control plane never dials into a
customer cluster.

## Architecture

The control plane owns Organizations, Integrations, Alert Events, Incidents, Conversations, Investigations, Tool Runs,
conclusions, Postmortems, and audit events in PostgreSQL. The supported composition serves the frontend separately and
proxies its same-origin `/api/v1` and `/webhooks/v1` traffic to the control plane. A separate gRPC listener accepts
outbound Relay sessions.

Provider adapters offer native read tools behind one provider-independent investigation contract. Kubernetes libraries
and customer cluster credentials never enter this module; the Relay executes the released, versioned capability protocol
inside the customer boundary.

Read the complete [alert-to-action architecture walkthrough](./ARCHITECTURE.md).

## Read-only security model

- Every request, offered tool, Tool Run, and stored record is Organization-scoped.
- Organization-scoped API requests select one active Organization with
  `X-OpenCluster-Organization`; authorization verifies membership before handlers run.
- Connected content and Conversation messages remain untrusted data, never instructions.
- External tools are read-only and every call records an operator-visible purpose.
- Secrets are file-backed or sealed; credential-shaped fields are removed from logs, events, audit details, prompts, and
  API responses.
- OpenCluster proposes production changes but exposes no execution endpoint.
- State-changing proposals always require human approval.

See [SECURITY.md](./SECURITY.md) and the
[security model](./docs/security/overview.mdx).

## Develop

Development requires Go 1.26.6, Docker, Docker Compose, and Helm 3.

```bash
make tools
make test-short
make verify
```

`make verify` runs lint, build, unit and PostgreSQL integration tests, vulnerability and license checks, deployment
validation, and evaluation gates. The real Relay protocol E2E proof lives in the nested `test/e2e` module.

## Documentation

- [Product documentation](./docs/index.mdx)
- [API reference](./docs/api-reference/overview.mdx)
- [Domain glossary](./CONTEXT.md)
- [Architecture](./ARCHITECTURE.md)
- [Contributor requirements](./AGENTS.md)

The Mintlify site is authored in `docs/`. Every shipped Integration must have a navigable product page. Run `make docs`
for navigation, frontmatter, internal-link, publication, credential-literal, accessibility, Integration-page, OpenAPI
drift, API-surface, and configuration-key checks; `make verify` includes it.

## Contributing

Start with [CONTRIBUTING.md](./CONTRIBUTING.md). The project’s support, governance, maintainers, and roadmap are
documented in [SUPPORT.md](./SUPPORT.md),
[GOVERNANCE.md](./GOVERNANCE.md), [MAINTAINERS.md](./MAINTAINERS.md), and
[ROADMAP.md](./ROADMAP.md).
