# The operator API

Everything an operator or a console does crosses `/operator/v1`, behind the permission
each route declares; directory provisioning crosses `/scim/v2` (RFC 7644's own version,
so a directory's configured base URL survives an operator API bump). The route table is
the API's index: each capability's `Routes()` table declares `(method, pattern,
permission)`, and nothing serves outside it.

`README.md` carries the full route tables. This page holds the contract rules a client
builds against.

## One listing contract

Every listing takes `search`, `sort`, `cursor`, `limit` where it declares them — an
undeclared sort or filter is refused, never silently dropped; a tampered cursor is
refused, never restarted; an oversized limit is clamped, never refused. Every listing
answers one envelope:

```json
{ "items": [], "next": null, "total": null, "partial": [] }
```

Every field is present, including the empty spellings. `total` is null when counting
would cost a second scan; `partial` names any field served without data and why.

## Refusals

- An organization the caller is no member of answers `404` exactly as one that does not
  exist — a `403` would enumerate customers.
- A member lacking the permission gets `403` naming what they lack.
- Malformed input is refused with the reason in the operator's language; a field nothing
  declares is refused, never dropped.
- `503` means the change was refused because it could not be recorded, or the deployment
  lacks a capability (a sealing key, a model provider) that the operation needs.

## Secrets on the wire

A webhook secret exists in exactly one response — its creation or rotation. An outbound
credential exists in requests only; reads render a minted fingerprint and timestamps.
Nothing this surface returns is cacheable (`Cache-Control: no-store`).

## Investigations

| Route | Permission | Behavior |
| --- | --- | --- |
| `POST /operator/v1/organizations/{org}/investigations` | `investigation.open` | Body `{episodeId}` or `{question}`. `202` with a running record; an ambiguous question answers `200` with one plain-language `clarification` and opens nothing; no model provider configured answers `503` with the reason |
| `GET …/investigations` | `investigation.read` | Newest first, standard envelope |
| `GET …/investigations/{id}` | `investigation.read` | The record plus full provenance: `sources` (rank, reason, selectedAt), `runs` (ordinal, tool, arguments, window, outcome, truncated, summary, sources, error), `findings` (statement + run ordinals), `spend` (tokens, integer micro-cents) |

A finding's `sources` are one-based ordinals among `runs` — every finding cites at least
one, enforced before anything persists.

## Versioning

`/operator/v1` is the contract the frontend builds against. With no external consumers
during the foundational simplification, removed operations were removed outright; from
here, a breaking change means `/operator/v2`.
