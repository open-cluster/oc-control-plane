# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in OpenCluster, please report it privately
through [GitHub Security Advisories](https://github.com/open-cluster/oc-control-plane/security/advisories) by selecting
**Report a vulnerability**.

Do not disclose vulnerabilities, credentials, customer data, or exploit details in public GitHub issues or discussions.

Please include the affected versions or commits, steps to reproduce, potential impact, and any suggested mitigation. Do
not include real credentials or sensitive customer information.

## Response and Disclosure

Reports will be assessed privately, prioritized by severity and impact, and addressed through coordinated remediation.
Security advisories are published when disclosure is appropriate.

## Supported Versions

OpenCluster is pre-release software with no stable release currently published. Security fixes target actively
maintained code. Support for previously distributed versions is evaluated based on their affected components and
deployments.

## Security Boundaries

- Tenant-owned database operations are scoped to their Organization.
- The control plane does not access customer Kubernetes clusters directly; the separately deployed Relay handles
  authorized cluster operations.
- Recoverable credentials are encrypted and excluded from logs and audit details.
- Operator requests require explicit permissions, and investigation evidence is shared only with the configured model
  provider.
- CI includes dependency vulnerability, license, and secret scanning.

For additional information, see the [security model](./docs/security/overview.mdx)
and [architecture](./ARCHITECTURE.md).