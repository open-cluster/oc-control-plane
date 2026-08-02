# OpenCluster — continue the live-model work

Status: HANDOFF PROMPT. Paste the whole of this file into a fresh session.
Date: 2026-08-02
Repository: the Go control plane

> This file exists so that starting a new session costs one paste rather than an
> archaeology session. It deliberately does NOT restate the architecture: the
> documents it points at are the authority and are kept current, and a second copy
> here would be a second thing to drift. If the work moves on, update those and
> this file stays correct for free.

---

You are working in `D:\Development\oc-control-plane` (Go control plane, private).
Related repos: `D:\Development\opencluster-relay` (the Relay + protocol module).

## Read these first, in this order

1. `CONTEXT.md` — the ubiquitous language. It is ENFORCED. Terms have prescribed
   names and explicit "avoid" lists (e.g. "budget" is banned for execution limits;
   the sanctioned term is "cost ceiling"). Use its words exactly.
2. `plans/implementation-status.md` — the living source of truth for what is
   built, what is proved, and what is not. Section 3 is current as of 2026-08-02.
3. `plans/spec-live-model-provider.md` — revision 2, the spec for the work just
   completed.
4. `docs/architecture/decisions/` — ADRs. Note ADR-016: files inside a capability
   package are named after domain nouns, never `service.go`/`repository.go`.
5. `internal/gates/gates_test.go` — architectural rules enforced mechanically.

## House style, non-negotiable

- Comments explain WHY a decision was made and what breaks otherwise — never what
  the code does. Match the density and voice of `internal/investigation/`.
- **Do not reference ADR files by number in code comments.** State the reason directly.
- No emojis in code. Small files grouped by domain, not by layer.
- Tests assert behaviour with names that read as sentences.

## What exists now (commits 02a33d3, 3820074, 44b7926)

- `internal/reasoning` — the model boundary, provider-neutral. Owns the prompt
  (versioned), three JSON output schemas, decoding, four-rate pricing in integer
  micro-cents, the recorder, the explicit fallback chain, breaker/limiter/ceiling.
- `internal/reasoning/anthropic` and `internal/reasoning/zai` — the two adapters.
  A gate fails the build if the domain imports either, or if the orchestration
  imports an adapter.
- `cmd/redherring` — one live investigation against pre-baked evidence; the
  instrument for judging prompt/schema quality cheaply.
- `test/e2e/cmd/scenario` — the real harness: provisions broken k3s clusters, runs
  the whole product, files artifacts for blind scoring.
- `scripts/live-model.sh` (`scenario` | `controlplane` | `config`), `.env.example`.

## What is genuinely proved

One end-to-end run on GLM-5 (real k3s + Relay + control plane + Postgres) solved
the `red-herring` scenario: correctly named a missing Secret, did not blame a loud
innocent deployment change beside it. ~$0.021, 13,118 tokens, 2m36s.
Evidence: `docs/evidence/live-glm5-red-herring-summary.json`.

## What is NOT proved — say so, don't assume otherwise

- **Anthropic has never been called.** Its adapter is exercised only against a
  canned HTTP transport.
- Nine of ten harness scenarios have never run live.
- Three live runs total. That is data points, not a measurement.

## The work, in priority order

**1. The falsification hole (domain, highest value).**
Across three live runs, the explanation traced to a hypothesis the investigator
actually tested only ONCE. `investigation.AdmitOutcome` requires a supporting
*claim* but never requires the explanation to correspond to a hypothesis that was
proposed and tested — so in two runs every hypothesis was falsified/set aside and
the outcome was still admitted as `supported`, stating a cause nobody proposed.
Decide whether that link should be required, and what an outcome that cannot make
it should be. This changes the abstention standard, so treat it as a spec question
first, not a patch.

**2. The distractor is avoided but not discriminated against (prompt).**
The `red-herring` scenario's ground truth says the correct behaviour is naming the
distractor as considered and SET ASIDE with a reason. The model never hypothesised
it at all. Avoiding a trap is not the same as ruling it out, and the case record
cannot show a reader the alternative was examined. Prompt-side; cheap to test with
`cmd/redherring`. Any prompt change MUST bump `reasoning.PromptVersion` and
regenerate `internal/reasoning/testdata/prompt.golden`
(`go test ./internal/reasoning/ -update`).

**3. Prove the contract is really provider-neutral: run Anthropic live.**
The two adapters deliberately disagree (Anthropic enforces output schemas and
reports cache-write tokens; Z.AI has a JSON mode and reports neither). Until
`claude-opus-5` answers a real scenario, "provider-neutral" is an assertion.
Put a key in `.secrets/anthropic.key`, then:
`scripts/live-model.sh scenario anthropic claude-opus-5`.

**4. Sweep the remaining scenarios.** `go run ./cmd/scenario list` shows all ten.
Roughly $0.20 for the set on GLM-5. Score them BLIND — give only `artifacts/` to
someone who did not build the system; `scenario score` joins ground truth after.

**5. Per-tenant consent (stated limitation, needs a decision).**
Consent is deployment-wide. Per-tenant needs the organization at the model
boundary, and `internal/tenancy` deliberately refuses ambient tenancy while the
`Reasoner` interface passes evidence rather than tenants. A shared deployment
serving tenants with different subprocessor agreements MUST NOT be pointed at a
live provider until this is settled. Documented in `internal/reasoning/deployment.go`.

**6. Integration-rich investigation (the direction).**
The intent is incident investigation across many integrations, not just Kubernetes.
`internal/reasoning` is already vendor- and domain-neutral: the schemas name
capability IDs from the registry rather than K8s concepts. The one real leak is
`internal/reasoning/decode.go`, where a read resolves a POD by ordinal against the
brief's topology; that wants generalising to "the entities the brief resolved"
before a second integration lands. New integrations = new capabilities + prompt
vocabulary, not a new model layer.

## Operational notes

- Credentials are FILE PATHS, never environment values. `.secrets/` and `.env` are
  gitignored. `.env.example` is the committed template and must never hold a secret.
- **Rotate the Z.AI key in `.secrets/zai.key`** — it was pasted into a chat transcript.
- The harness needs Docker (k3s + Postgres via testcontainers) and the Relay repo
  at `../opencluster-relay`.
- Priced models are listed by `scripts/live-model.sh config`. An unpriced model is
  refused at STARTUP by design — never cost it at zero.
- Verify with: `go build ./... && go vet ./... && go test ./... -short`, and
  `cd test/e2e && go vet ./...`.

## How I want you to work

Plan before building. Tests first at real seams. Do not call something proved
because unit tests pass — this codebase's whole thesis is the difference between
"the machinery behaves as specified" and "the explanations are worth reading."
When you find a defect in my instructions or my design, say so in a sentence and
keep going; don't silently narrow the work. Report honestly what was proved versus
what remains unverified.

Start by reading the files above and telling me your plan for item 1.
