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
Each Tool Run has a stable evidence ID. Retained citations keep their original ordinal links.
Web reports, Conversation turns, and event streams enforce source access for both citation forms.
Source limits and content clipped for the model remain distinct limitations.
Editors and Admins can inspect retained bounded observations on demand. Ordinary
Investigation detail remains lightweight.

> OpenCluster is pre-release software. APIs and storage may change before the first stable release.

## How it works

1. Start a web **Conversation** for an operational question. Its Messages queue individual replies that can use authorized **Tools**.
2. Open an **Investigation** for an evidence-backed report, or use the one started by a firing alert. The report records Findings, Action Proposals, and limitations.
3. Review the report's evidence. Resolved Incidents can produce reviewed **Postmortems**.

Web Chat restores its Conversation checkpoint across replies. AgentScope compacts model context
while visible Messages remain available. Explicit Investigations own independent checkpoints and
keep completed reports immutable. New reports cite Tool Run UUIDs. The ordinal observation route
serves retained records.

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

The Integration type catalog lists provider identities. Type detail describes configuration fields and connection requirements.
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
coordinates Conversations and Investigations, authorizes Tools, and records Tool Runs and
results. The Investigation API exposes structured reports, and Slack delivers summaries and
activity in the originating thread.

PostgreSQL is the durable source of truth. Customer-owned state is explicitly scoped to an
Organization, and operational mutations are audited.

Chat and Investigations run on AgentScope. It handles model calls, Tool scheduling, context
management, and reply recovery. PostgreSQL stores checkpoints and fences Tool authorization,
evidence recording, and result acceptance. Slack continues to use assigned Investigation turns during the transition.
After the investigative reply completes, AgentScope makes a separate structured report request.
OpenCluster validates the Conclusion and commits it with the execution outcome and any linked
Message for a legacy assigned turn. Explicit and autonomous Investigations record an assessment when
they conclude. Web Chat commits the assistant Message and Conversation checkpoint together.
A crash before commit can regenerate the report from the completed checkpoint without resuming
the investigative reply.
When upgrading from the previous Investigation engine, stop the old control-plane instances
before starting this version so each newly admitted Investigation gets a native execution.

Findings require citations to successful reads. Failed or skipped reads can explain
limitations. Action Proposals for obtaining missing evidence may omit citations. Startup,
Tool activity, and terminal events remain available on reconnect. Incremental narrative
progress is tracked in issue #234.
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

Run `go test -race ./internal/ai` to verify the AgentScope foundation against
local provider fixtures. The tests require no model credentials.
Run `go test -race ./test/postgres -run '^TestNativeToolkit'` to verify native
Tool dispatch, current authorization, and evidence persistence with PostgreSQL.

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
