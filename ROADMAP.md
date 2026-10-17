# Roadmap

OpenCluster is an open-source AI SRE for investigating production incidents. Built to help engineers investigate
production incidents, understand what went wrong, and minimize the operational toil and MTTR.

We started the project as read-only incident investigation, but our longer-term goal is to make OpenCluster more
proactive that will help with anomaly detection and safely automating the labour-intensive parts of a SRE/Platform
Engineers when dealing with incidents or potential ones.

This roadmap outlines our direction, not fixed release commitments. Priorities and architectural decisions may change as
we learn and develop from real-world usage and community feedback.

## Current priorities

- **Reliable investigations:** Strengthen the journey from alerts and operational questions to traceable findings, with
  accurate evidence, clear uncertainty, and reliable execution.
- **Production readiness:** Improve deployments, releases, recovery, documentation, and the operational experience of
  running OpenCluster.

## What's next

- **More integrations:** Expand beyond Kubernetes to support more infrastructure and observability systems, including
  HashiCorp Nomad, Sentry, Datadog, alert managers, etc.
- **Network observability:** Explore Cilium and Hubble for investigating network failures, dropped connections, DNS
  issues, and service communication.
- **Proactive investigation:** Move beyond responding to alerts toward detecting anomalies, correlating signals, and
  investigating emerging problems.
- **Safe automation:** Gradually introduce controlled actions and remediation workflows with explicit permissions.

## Longer-term direction

We want OpenCluster to grow as a cloud-native project that works with existing infrastructure and tools being already
used to cut the operational toil.

We also aim to adopt OpenSSF security best practices and pursue being a part of CNCF projects.

## Community and contributions

OpenCluster's direction should be shaped by the people building and using it.

We welcome contributions, ideas, and feedback. Join our [Discord community](https://discord.com/invite/8HrEBhUagE) to connect
with maintainers, discuss ideas, and collaborate with other engineers.

For feature proposals, architectural decisions, and development discussions,
use [GitHub Issues](https://github.com/open-cluster/oc-control-plane/issues).

Significant changes should be discussed openly before implementation. Maintainers will weigh user value, technical
evidence, security, and long-term maintainability when making decisions.

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines and [ARCHITECTURE.md](ARCHITECTURE.md) for the
current system design.