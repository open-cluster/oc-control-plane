# Operations

Day-two OpenCluster: what runs on its own, what an operator drives, and what each
record means.

## What runs on its own

| Worker | Cadence | Does |
| --- | --- | --- |
| Audit pruner | hourly | applies each tenant's declared audit retention |
| Change-ledger pruner | hourly | ages ledger entries past `OC_CHANGE_LEDGER_RETENTION_DAYS` |
| Relay inventory sync | `OC_INVENTORY_INTERVAL` (relay-floored) | feeds the change ledger; opens scopes for kubernetes Integrations |
| Investigation runner | per opened investigation | routes, reads, records, concludes in the background |

## Incidents

Signals group into IncidentEpisodes on the source's own grouping identity; an episode
resolves when nothing in it still fires, and a wrong grouping is corrected by a merge
that rewrites nothing. Splits do not exist because the grouping errs toward splitting.

## Investigations

Open one from an episode, or ask a question in plain words — ambiguity gets one
clarifying question back. The runner:

1. selects up to two readable sources deterministically (term overlap with the subject
   and the alert's own labels), recording each reason;
2. lets the model choose among those sources' declared tools, bounded in rounds and
   calls, recording every run — scope, window, outcome, truncation, summary, source
   references — as it finishes;
3. expands once, to the next-ranked source, only if everything so far failed or came
   back empty;
4. ends concluded with findings citing run ordinals, or failed with the reason. Spend is
   summed in tokens and integer micro-cents over every call, including refused ones. A
   ceiling — the spend cap, the read budget, the turn budget — forces a final concluding
   turn and labels the outcome `stoppedBy` (`spend`, `tool_runs`, `reasoner_turns`), so
   resource exhaustion is never rendered as a free diagnosis.

Reading `GET /investigations/{id}` is the audit: what was queried, why, what came back,
what was established. A failed reasoning step is a failed investigation — never a
conclusion.

Every model call emits one span and one structured log line (provider, the model that
answered, request id, stop reason, latency, token decomposition, integer micro-cents,
and the derived `agent_revision` — a hash over the prompt preamble, conclusion schema
and tool definitions, so any change to what the model sees is attributable without a
version anybody bumps), plus Prometheus series derived from the `oc.reasoning.tokens`,
`oc.reasoning.spend` and `oc.reasoning.call_duration` instruments (the exporter adds
its own `_total`/unit suffixes), labeled by provider and model.

## Health signals worth alerting on

- `readyz` unready: the required placement is unreachable.
- Integration status `failed`/`degraded` after verification — a revoked token, a
  suspended installation, a disconnected relay, missing scopes. The note says which.
- Relay fleet summary `outdated`/`disconnected` counts.
- Investigations ending `failed` with reasons naming the model provider: check the
  deployment's model configuration and the vendor's status.

## Retention

Audit retention is each tenant's declaration, enforced hourly. The change ledger is the
deployment's schedule. Signals, episodes, and investigation provenance are kept: they
are the operational record.
