# Security policy

Do not report vulnerabilities, credentials, customer data, or exploit details in a public
issue. Use the repository's private GitHub vulnerability-reporting channel when available;
otherwise contact an OpenCluster organization owner or repository maintainer privately.

Include affected versions or commits, reproduction details, impact, and any suggested
mitigation. Maintainers acknowledge reports, assess tenant-isolation and credential impact,
coordinate remediation privately, and publish advisories only when disclosure is safe.

## Security boundaries

- Every tenant-owned database operation is scoped to its Organization.
- The control plane never accesses customer clusters; the separately deployed Relay does.
- Credentials arrive through mounted files, are encrypted when recoverable, and are
  excluded from audit details and logs.
- Operator requests require explicit route permissions; only the configured model provider
  receives investigation evidence.
- Continuous integration runs dependency vulnerability, license, and secret scans using
  pinned tooling and least-privilege workflow permissions.

See [the public security model](./docs/security/overview.mdx) and
[architecture and trust boundaries](./ARCHITECTURE.md).
