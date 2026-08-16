# Security

What a reviewer should be able to confirm quickly, and where each property is enforced.

## Tenant isolation

- `org_id` is the only durable tenant identity; display names participate in nothing.
- Every tenant-scoped table carries composite `(org_id, id)` keys, and every cross-table
  reference goes through them — a cross-tenant reference is structurally impossible, not
  filtered. The storage tests prove the boundary per table.
- Placements are resolved from the organization; an unresolvable one is an error, never
  a shared-pool fallback.
- A request naming a tenant the caller is no member of answers 404 exactly like a tenant
  that does not exist.

## Credentials

| Credential | At rest | Notes |
| --- | --- | --- |
| Webhook secrets (inbound) | SHA-256 digest only | minted, shown once, constant-time compare, rotate-not-recover; fingerprints are minted, never derived |
| Integration credentials (Slack bot token) | AES-256-GCM sealed (`internal/seal`) | probed live before storage; write-only after entry; submitting one with no sealing key is refused loudly, never dropped |
| Identity provider client secrets | sealed, same key | |
| Session identifiers, API tokens | digests | API tokens bound to one organization, one role, an expiry |
| Operator bootstrap token | digest | bound to one organization and one role; deliberately no expiry and no revocation row — it exists to bootstrap a deployment with no members, and revoking it means changing the file and restarting |
| GitHub App key, model API key, DSNs | files named by env vars | never environment values; errors never quote file contents |

The audit record drops credential-shaped detail keys mechanically before writing.
Reasoning's `Secret` type renders as a placeholder in every format a log could reach.

## Untrusted input

Everything a customer's systems produce — alert payloads, channel messages, commit
messages, tool results — is untrusted for its whole life: information, never
instruction. The investigator's prompt states it; more importantly, the structure limits
it: the model can only call declared tools of router-selected sources, arguments are
validated per declaration, window arguments are clamped into the investigation's window
in provider code, and findings must cite recorded runs. Intake bounds bodies (1 MiB),
rate-limits before authentication, and normalises with per-field caps.

## Authorization

Three human roles, compiled; admin is never grantable by JIT provisioning, group claims,
SCIM mappings, or API tokens. Every route declares its permission in the table the mux
is built from; gates refuse routes registered anywhere else, hold the public surface to
a named list, and print the role table. Refused authorizations by credential-holders are
recorded — probing is only visible if failures are.

## The record

Audit rows commit inside the transaction of the change they record; the table refuses
UPDATE/DELETE/TRUNCATE at the database; retention pruning must declare itself in its
transaction. Investigations persist operational provenance — never model chain-of-thought
and never bulk copies of what was read.

## Reporting

Proprietary code; report findings through the organization's private channels, not a
public tracker.
