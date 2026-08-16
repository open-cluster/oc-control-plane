# Troubleshooting

Symptoms, what each actually means, and where the answer is written down.

## The process

| Symptom | Meaning |
| --- | --- |
| Exits at startup naming an `OC_` variable | The configuration cannot be served. The message names the variable and the rule; nothing starts degraded |
| `OC_SEALING_KEY_FILE is required: the catalog serves slack…` | The build serves a credential-bearing type and the deployment cannot seal a credential. Configure the key or bind no operator surface |
| `readyz` 503, `healthz` 200 | The required placement is unreachable. Liveness ignores dependencies on purpose — do not restart your way out of a database outage |

## Integrations

| Symptom | Meaning |
| --- | --- |
| Create refused 400 with a vendor reason (`invalid_auth`, `does not know installation…`) | The live probe failed: the pasted credential or installation id is wrong AT THE VENDOR. Nothing was saved |
| Create refused 503 "no sealing key" | The deployment cannot hold the credential; nothing was saved or dropped |
| Verify turns an integration `failed`/`degraded` | The far end's answer changed: revoked token, suspended installation, disconnected relay, missing scopes. The note names it in the operator's language |
| "the stored credential could not be opened; paste it again" | The sealing key changed since the credential was stored. Re-paste the credential |
| Webhook deliveries 401 | Wrong or rotated secret, or the integration is disabled — a disabled one refuses rather than records |
| Deliveries 200 instead of 202 | A replay of a body already accepted; the source should stop retrying. Nothing was written twice |
| DELETE refused 409 | Records depend on the integration. Disable instead; the record of what a source produced must survive |

## Investigations

| Symptom | Meaning |
| --- | --- |
| POST refused 503 "no model provider" | The deployment has no `OC_MODEL_*` configuration; everything else still works |
| 200 with a `clarification` instead of 202 | The question tied to zero or several open incidents. Answer the one question it asked |
| Ends `failed`: "the reasoning step could not run: …" | The model provider refused, timed out, or is down; the named outcome says which — `refused` pages whoever owns prompts, `outage` pages whoever watches the vendor |
| Ends `failed`: "cited a read that never ran" | The model produced an untraceable finding and it was refused rather than stored |
| Ends concluded with no findings | Honest: what was read established nothing. Read the runs — the provenance shows what was tried |
| Runs show `truncated` | The source held more than the bound; the content shown is real but not the whole. Narrow the window or the query |
| A run failed "not one of the tools the selected sources offer" | The model reached outside the router's selection; the read did not happen and the record says so |

## Relays

| Symptom | Meaning |
| --- | --- |
| Kubernetes verify: "relay is not connected" | The relay holds no live session — its side of the wire, its logs |
| Verify degraded: "does not advertise: …" | The cluster's own RBAC or relay build lacks the capability; fix in the cluster |
| Session conflicts on the roster | Two relays presented one identity — a possible credential theft. Clearing the mark destroys the finding, so only an Admin can, and it is audited |
