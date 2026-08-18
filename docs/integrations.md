# Integrations

An **Integration Type** is a kind of tool this build supports; an **Integration** is one
configured installation belonging to an organization. The catalog
(`GET /integration-types`) says what each type can do, what configuring it takes, and
what reading it offers; nothing asks an operator to classify anything.

## The types this build serves

| Type | Direction | Configured with | Verified by | Reads |
| --- | --- | --- | --- | --- |
| `alertmanager` | inbound webhook | nothing; the platform mints the webhook URL and secret | the last accepted delivery — honesty about what an inbound type can prove | — |
| `kubernetes` | relay-dispatched | optional namespace allow-list; requires a Relay | the bound Relay's live session and advertised capabilities | workloads, events, container logs (through Relay jobs) |
| `slack` | outbound | a pasted bot token (sealed at rest, verified live before saving) | `auth.test` against the vendor: workspace, bot identity, granted scopes — recorded, because tool availability derives from them | channels, channel history, threads; message search only with a user token granted `search:read` |
| `github` | outbound | an installation id; the App credential is the deployment's | the installation itself: account, suspension, granted repositories | repositories, commits, one commit's diff, pull requests with files and checks, workflow runs, job logs, file contents, releases — by stable ids |

## The lifecycle every Integration shares

Create → verify (live, on demand) → operate → disable (keeps the record) → delete (only
while nothing depends on it). Status is observed, never declared: `configured`, `active`,
`degraded` (the far end answered and part of the offer is unavailable), `failed`. The
last verification's note says what was established, in the operator's language.

## Credentials

Two paths, both explicit:

- **Inbound webhook secrets** — minted, shown once, stored as SHA-256 digests, compared
  in constant time, rotated rather than recovered.
- **Outbound credentials** (a Slack bot token) — probed against the vendor before
  anything is stored, sealed with AES-256-GCM under the deployment's sealing key,
  write-only after entry. Reads render a minted fingerprint, never the credential. A
  deployment without a sealing key refuses to serve a credential-bearing catalog at all.

The GitHub App credential is deployment-level configuration, not a per-integration
secret; an Integration stores only its installation id, and access follows what that
installation granted, live.

## Tools

Outbound types declare **tools**: bounded, read-only operations an investigation may run.
Every tool declares its purpose, when to use it, when NOT to use it, its arguments,
required permissions, and output — enforced at catalog assembly, rendered in the catalog,
and used by the investigator to route. Every read is bounded by named limits, flags
truncation from the vendor's own answer, and clamps any window argument inside the
investigation's window. A Slack thread longer than its bound answers its newest tail with
the truncation flagged — the end of a war-room thread is where the conclusion lives.

**Availability derives from verified reality.** Verification records what the credential
was actually granted — Slack: the OAuth scopes, plus whether the token is a user token —
and a tool whose requirements those grants cannot support is absent from an
investigation's set rather than offered and failing at call time. Classic Slack message
search is user-token-only, so a pasted bot token is never offered `slack.search_messages`.
The Slack tools want `channels:read`, `channels:history` and `users:read` (author names in
transcripts); a missing scope degrades verification with the consequence named.

## Adding a provider

One self-contained package under `internal/integrations/`, one seeded row in a new
migration, one line in the composition root's catalog. The drift test fails until the
row and the compiled definition agree. No registry, no synchronisation, no switch.
