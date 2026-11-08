<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./docs/logo/dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="./docs/logo/light.svg">
    <img alt="OpenCluster" src="./docs/logo/light.svg" width="344">
  </picture>
</p>

<p align="center">
  <a href="https://docs.open-cluster.io/"><img alt="Documentation" src="https://github.com/open-cluster/oc-control-plane/actions/workflows/docs.yml/badge.svg"></a>
  <a href="./LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/github/license/open-cluster/oc-control-plane"></a>
</p>

<p align="center">
  <a href="https://docs.open-cluster.io/getting-started/quickstart">Quickstart</a> ·
  <a href="https://docs.open-cluster.io/api-reference/overview">API</a> ·
  <a href="https://docs.open-cluster.io/security/overview">Security</a> ·
  <a href="https://docs.open-cluster.io/developers/contributing">Contributing</a> ·
  <a href="https://discord.com/invite/8HrEBhUagE">Discord</a>
</p>

OpenCluster is an open-source AI SRE for operational questions and production incident investigation.

Ask through Slack or the Conversation API using evidence from Kubernetes, GitHub,
metrics, logs, traces, and permitted discussions. Alerts can also start investigations.
OpenCluster reports cited findings, explicit limitations, and recommended next steps.

Every material Finding is backed by the Tool Runs that support it, so operators can inspect
how a conclusion was reached instead of treating the AI response as a black box.
Uncertainty, missing telemetry, and unresolved assumptions remain explicit.
Tool Runs preserve subsecond execution timing separately from source-query bounds.
Source limits and content clipped for the model remain distinct limitations.
Editors and Admins can inspect retained bounded observations on demand. Ordinary
Investigation detail remains lightweight.

> OpenCluster is pre-release software. APIs and storage may change before the first stable release.

## How it works

1. An alert opens or updates an **Incident**, or an operator starts a **Conversation**.
2. OpenCluster opens a bounded **Investigation**.
3. The investigator selects authorized **Tools** from connected **Integrations**.
4. Tool Runs gather operational context and are recorded as evidence.
5. The Investigation produces impact, Findings, hypotheses, Action Proposals, and limitations.
6. Follow-up questions continue through the same Conversation without changing earlier Investigations.
7. Resolved Incidents can produce reviewed **Postmortems**.

An Investigation is immutable once completed. Follow-ups create new Investigations while
retaining bounded Conversation context and cited Findings established before the new
Investigation opened. Earlier reply details and older Messages are loaded on demand.
Normal requests retain their complete context. Only an actual provider context rejection triggers
bounded recovery that omits optional prior context while preserving current Messages and completed
Tool exchanges. Unrecoverable capacity problems are technical failures. Reads can refresh changing
infrastructure within the execution limits.
Accepted summaries and Action Proposals are preserved without silent truncation.

## Integrations

OpenCluster currently supports:

- **Kubernetes** — workload state, events, and logs through the OpenCluster Relay
- **GitHub** — repository and change context
- **Slack** — operational context and threaded conversations
- **Prometheus Alertmanager** — alert and Incident intake
- **Generic Webhook** — authenticated JSON alert intake
- **Prometheus** — metric discovery and bounded PromQL queries against approved self-hosted sources
- **Loki** — bounded LogQL log excerpts from approved self-hosted or Grafana Cloud sources
- **Tempo** — trace search and bounded span relationships from approved sources

Integrations expose only the capabilities that were successfully verified for that connection.

The Integration type API lists Tools, setup capabilities, and configured counts.
Registered types are available in the running build.

See [Connect your tools](https://docs.open-cluster.io/getting-started/connect-your-tools)
for API setup and provider authorization.

## Capabilities

OpenCluster currently ships observation and read capabilities.

The capability model is not permanently limited to reads. State-changing capabilities must
add explicit authorization, human approval, idempotency, auditability, and verification
before they are enabled.

Today, OpenCluster recommends actions but does not execute production changes.

## Architecture

OpenCluster is split into a control plane and an optional customer-side Relay.

### Control plane

The control plane receives API requests and webhooks, groups alerts into Incidents,
coordinates Conversations and Investigations, selects eligible Integration Tools, runs the
reasoning loop, records Tool Runs and results, and exposes progress and conclusions to the UI
or Slack.

PostgreSQL is the durable source of truth. Customer-owned state is explicitly scoped to an
Organization, and operational mutations are audited.

Investigations use Anthropic or OpenRouter through native Tool calling. OpenCluster owns
Tool authorization, execution budgets, evidence recording, and conclusion validation.

Findings require citations to successful reads. Failed or skipped reads can explain
limitations but cannot establish a Finding. Startup activity describes the initial phase,
and recorded activity remains available when a client reconnects.
Tool completion activity is published only after the Tool Run's provenance is recorded.
Operational JSON logs correlate Investigations and Tool executions with trace identifiers.
Routine requests and successful health probes log at DEBUG, with durations in milliseconds.

### Relay

The Relay runs inside the customer environment and opens an outbound gRPC session to the
control plane.

It executes approved Kubernetes capabilities close to the cluster, while Kubernetes
credentials remain inside the customer boundary. The control plane never receives those
cluster credentials.

### Investigation flow

```text
Alert / operator question
          │
          ▼
   Incident / Conversation
          │
          ▼
     Investigation
          │
          ▼
   Eligible Integrations
          │
          ▼
       Tool Runs
          │
          ▼
      Conclusion
   ┌──────┼─────────┐
   │      │         │
Findings Actions Limitations
```

See [ARCHITECTURE.md](./ARCHITECTURE.md) for the complete design.

## Security

- Tenant-owned data is explicitly scoped to an Organization.
- Connected content and Conversation Messages are treated as untrusted data.
- Tools are available only through verified Integration capabilities.
- Credentials are sealed or digested and are not exposed through product APIs.
- GitHub connections require user authorization and proof of installation ownership.
- Changing credential-use settings requires a verified replacement credential; renaming does not.
- Material Findings cite supporting Tool Runs.
- Current Action Proposals do not execute production changes.

See [SECURITY.md](./SECURITY.md) and the
[security documentation](https://docs.open-cluster.io/security/overview).

## Quick start

The fastest way to run OpenCluster locally is with Docker Compose:

```bash
docker compose -f deploy/compose/compose.yaml up --build
```

A local deployment requires PostgreSQL, an encryption key, and a supported model provider.

Follow [Docker Compose setup](https://docs.open-cluster.io/self-hosting/docker-compose) for installation and private initial signup.
With a compatible frontend, open `/setup` over a trusted private connection to create
the first account. Signup issues the existing session cookie. Keep the deployment private
until the first Admin is created. A fresh or reset database allows the first caller to claim Admin.
The standalone backend uses port `8081`. Its default local public origin is
`http://localhost:8081`. A separately deployed frontend must provide its own verified routing.
Helm requires `server.publicURL` to be set to the deployment's actual public origin.
Model requests use provider context-limit responses for bounded recovery. The output allowance
remains configurable through `OC_AI_MAX_OUTPUT_SIZE`.

## Development

```bash
make tools
make test-short
make verify
```

The full container-backed root-module suite runs packages sequentially to preserve
shared test-container lifetimes. Race detection remains enabled.

See [CONTRIBUTING.md](./CONTRIBUTING.md) before submitting changes.

## Documentation

- [Product documentation](https://docs.open-cluster.io/)
- [Architecture](./ARCHITECTURE.md)
- [Domain glossary](./CONTEXT.md)
- [Roadmap](./ROADMAP.md)

## Contributing

Start with [CONTRIBUTING.md](./CONTRIBUTING.md). The project’s support, governance, maintainers, and roadmap are
documented in [SUPPORT.md](./SUPPORT.md),
[GOVERNANCE.md](./GOVERNANCE.md), [MAINTAINERS.md](./MAINTAINERS.md), and
[ROADMAP.md](./ROADMAP.md).
