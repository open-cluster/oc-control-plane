# Spec — The traced explanation

Status: READY FOR IMPLEMENTATION. Revision 1.
Date: 2026-08-02
Repository: the Go control plane
Glossary: `CONTEXT.md`

## Problem Statement

Across three live runs, the explanation a round stated traced to a hypothesis the investigator
actually tested exactly once. In the other two, every hypothesis was falsified or set aside and the
round still admitted an outcome of kind `supported`, naming a cause nobody had proposed.

`AdmitOutcome` requires a supporting Claim, and a Claim requires a citation. Neither requires the
explanation itself to correspond to a Hypothesis that was proposed and carried a falsification
condition. So the record can show four dead hypotheses beside a conclusion that arrived from
nowhere, and the reader has no way to tell that from a conclusion that survived a test.

That is the defect named in the brief. There are two more behind it, and fixing the first alone
would make the product worse rather than better.

**The interface makes the correct behaviour unexpressible.** `Reasoner.Hypotheses` is called once,
from the brief alone, before any evidence exists. Neither `Proposed` nor `Concluded` carries a new
hypothesis. When an adaptive read returns a line naming a missing Secret — a cause no one could
have guessed from a brief that predates the read — the reasoner has exactly two moves available:
state it in the conclusion untethered from any hypothesis, or abstain on evidence that names the
answer. It chose the first, and given what it was handed that was the better of the two. Adding a
validation rule and nothing else would convert correct answers into abstentions and would look like
the standard working when it is the interface failing.

**`HypothesisSupported` has no consumer.** A reasoner may settle a hypothesis to `supported`;
nothing in the domain then reads that state. It does not gate admission, it does not appear in the
outcome, and because `live()` excludes it, a hypothesis marked supported drops out of the only
hypothesis list `AdmitOutcome` is given. The one state that ought to be load-bearing is the one
state nothing loads.

## Solution

**A supported or caveated outcome names the one hypothesis it explains, and that hypothesis is in
the `supported` state.** This is the traced explanation, and it is checked where an uncited claim is
already checked: at admission, before storage. An outcome that cannot name one is refused, retried
once through the loop that already exists, and then abstains.

**A reasoner may propose a hypothesis at an adaptive pass and at the conclusion**, with a
falsification condition like every other one. Discovery mid-round is legitimate. Discovery smuggled
into a statement field is not, and widening the interface is what makes the difference expressible
rather than punishable.

**A hypothesis that no dispatched read was ever justified by was never put at risk, and an outcome
resting on one is `caveated` rather than `supported`.** It carries a coverage gap saying so. The
demotion is computed by the control plane from what it dispatched, never asked of the model, because
a model asked to grade its own rigour grades it generously.

That last rule is what stops the first from being satisfiable by ritual. Without it the reasoner
learns to emit one extra hypothesis object beside every conclusion and the requirement becomes a
formality that changes nothing. With it, proposing late buys nothing that proposing early and
testing would not have bought better.

## User Stories

1. As an on-call engineer, I want the explanation to be one of the alternatives the investigation
   named, so that the hypothesis list is the reasoning rather than decoration beside a conclusion.
2. As an on-call engineer, I want to see that the explanation was put at risk, so that "supported"
   means it survived a test rather than that it was stated confidently.
3. As an on-call engineer, I want an explanation nothing was read to disprove to say so on its face,
   so that I know which conclusions to re-run before acting on them.
4. As an SRE lead evaluating the product, I want the alternatives named and settled with reasons, so
   that I can check the investigation in a minute rather than reproduce it in an hour.
5. As the founder, I want a cause discovered from evidence to be admissible, so that the standard
   does not force abstention on runs that found the right answer.
6. As the founder, I want a round that discovered its cause late to be distinguishable from one that
   predicted it, so that three runs of harness scoring measure reasoning rather than luck.
7. As an engineer reading a case, I want every hypothesis the round held to stay visible with its
   final state, so that what was ruled out is as readable as what was concluded.

## Implementation Decisions

### The standard

**A traced explanation is an outcome that names exactly one Hypothesis, by ordinal among those the
reasoner was shown, whose state after this answer's settlings is `supported`.** One, not several: an
explanation resting on two hypotheses at once is two explanations, and choosing between them is the
work being asked for.

**`supported` and `caveated` both require it. `abstained` must not carry one.** An abstention says no
explanation was sufficiently supported; naming the hypothesis it explains would contradict its own
kind.

**An outcome that cannot make the link is refused at admission.** It is not demoted to an abstention
silently and it is not stored. The concluding loop already retries a refused draft once and abstains
on the second refusal, and this refusal joins that set rather than getting a path of its own.

**The abstention the round falls back to says which standard was missed.** "The reasoner stated an
explanation that no hypothesis it proposed and tested corresponds to" is a different sentence from
"the reasoner could not cite its claims", and an operator reading the case must be able to tell them
apart without reading the code.

### Discovery is legitimate, and it is recorded as discovery

**The reasoner may propose hypotheses at an adaptive pass and at the conclusion.** Each carries a
statement and a falsification condition, exactly as an opening hypothesis does. They are appended to
the round's hypothesis list and take the next ordinals.

**One rule for ordinals at every call, with no exceptions.** An ordinal names what the reasoner was
shown PLUS what the same answer proposed, and it means that in every field of every document: the
read a proposal justifies, the hypothesis a weighing names, the hypothesis a settling moves, and the
hypothesis an outcome explains.

The exception this specification first carried — that a hypothesis proposed in a document is not
citable by that same document — was wrong twice over. It would have refused a planner that did what
the prompt asks (discover a cause, then ask for the read that would disprove it) by rejecting the
whole document as malformed. And it would have forbidden the reasoner from recording how the
evidence it discovered the hypothesis FROM stands towards it, which is most of what shows the
discovery was reasoning rather than assertion. Two rules where one will do is also how the two ends
of a boundary drift apart: the runner appends proposals before it validates reads and before it
applies settlings, so the domain already held the single rule while the decoder held the other.

**A hypothesis records the pass that proposed it.** Zero is the opening, from the brief alone. One
and two are the adaptive passes. Three — one past the last pass — is the conclusion. It is a number
rather than a flag because the useful question later is not "was it late" but "how late", and the
harness will want to compare that across scenarios.

### Untested explanations are caveated, and the control plane decides it

**A hypothesis is TESTED when at least one read this round DISPATCHED named it as its
justification.** Dispatched, not proposed: a read refused before dispatch tested nothing, and a
read that failed still put the hypothesis at risk because the failure is itself recorded.

**An outcome the reasoner drafted as `supported` whose hypothesis is untested is admitted as
`caveated`, and a coverage gap is recorded naming the consequence.** The reasoner's own drafted kind
is never promoted, only demoted: a reasoner that drafted `caveated` gets `caveated`, and one that
drafted `abstained` is unaffected.

**The gap's cause is a new one: the explanation was never put at risk in this round.** None of the
ten existing causes fits. It is not a capability that was unavailable, not a limit that was reached,
not a source that could not answer. It is a check the round had no remaining opportunity to make,
because the hypothesis arrived at the last call the round makes. Its consequence names the action:
a further round can test it, and reinvestigation already adds a round to the same case.

**The demotion is not something the model can opt out of, and not something it can invoke.** It is
computed from `Request.Justification` over the round's dispatched reads, which is a fact about what
this control plane sent to a customer's cluster.

### What the domain exposes

**`Draft` gains `Explains`, an ordinal.** `Outcome` gains `Explains`, the identifier the ordinal
resolved to. Zero and nil are legal on an abstention and refused on the other two kinds.

**Newly proposed hypotheses ride on the answer, not on the draft.** `Proposed` and `Concluded` each
gain a `Hypotheses` field, matching `Hypothesized.Hypotheses`, which the opening call already used.
They do not belong on the `Draft`: the round has to record them before the draft is admitted, so
that the ordinals the draft answers in resolve to rows that exist.

**`AdmitOutcome` takes the round's whole hypothesis list rather than only the live ones**, with each
one's state as this answer's settlings left it, plus the set of hypotheses a dispatched read named.
Passing only the live ones is what made `HypothesisSupported` invisible to admission, and it also
made the `Unresolved` ordinals mean a different list from the one the reasoner answered in — which
`internal/reasoning` compensates for with a translation step that this change deletes.

**`Unresolved` ordinals now name positions in the whole list.** A hypothesis this same answer
settled is dropped rather than refused, as it is today.

### The prompt and the schemas

**One version bump covers this and the distractor wording.** `PromptVersion` and `SchemaVersion`
both move to 3, the golden file is regenerated, and every recording made against 2 is refused by the
transcript key that already exists.

**The conclusion schema gains `explains` and `proposes`. The proposals schema gains `proposes`.**
Every property stays required, as every property in these schemas already is; an answer that has
nothing to propose sends an empty list and an abstention sends `explains` as zero.

**The conclusion task says what the kinds now mean.** That the explanation must be one of the
hypotheses held or one proposed in the same document; that a hypothesis proposed here needs its
falsification condition like any other; that alternatives are settled with reasons rather than left
silent.

**The hypotheses task asks for the change explanation when the brief lists changes.** The
`red-herring` scenario's ground truth says the correct behaviour is naming the loud innocent change
as considered and SET ASIDE with a reason. Across three runs the reasoner never hypothesised it at
all. Avoiding a trap is not discriminating against it, and a case record that never named the
alternative cannot show a reader it was examined. The wording asks for it as a hypothesis when the
brief reports a change in the window, and the conclusion task asks for every alternative to end in a
state with a reason.

## Production Requirements

**The gap cause is a persisted value and joins the frozen table.** Adding it means a migration
widening the `coverage_gap.cause` CHECK and a line in the gate that freezes these values. Both are
required; the gate is what makes the storage contract visible.

**The pass that proposed a hypothesis is persisted.** A column on `investigation_hypothesis`,
defaulting to zero so existing rows keep the meaning they have — every hypothesis written before
this change was proposed at the opening.

**`Outcome.Explains` is persisted and nullable.** Null on an abstention, and a foreign key to the
hypothesis so a citation is a lookup rather than a search, scoped by organization like every other
reference in this schema.

**The read model shows it.** An outcome that explains a hypothesis says which one; an outcome
carrying the untested gap shows that gap beside it, through the machinery gaps already travel
through. No new surface.

**Nothing about this changes what a round costs.** No further model call is made. The rules are
checks over what a round already produced.

## Testing Decisions

**The seam is `AdmitOutcome`.** It is a pure function over what the reasoner was shown, and it is
where an uncited claim already dies, so it is where these tests go. They assert refusals, because the
refusals are the standard.

- A supported outcome naming no hypothesis is refused.
- A supported outcome naming a hypothesis that was falsified is refused, and one naming a set-aside
  hypothesis is refused. Both name the state in the message.
- A supported outcome naming a hypothesis outside what was shown is refused, as every out-of-range
  ordinal already is.
- A supported outcome naming a supported, tested hypothesis is admitted, and resolves to that
  hypothesis's identifier.
- The same outcome naming a supported hypothesis that no dispatched read was justified by is
  admitted as `caveated`, and reports that it was demoted so the caller can record the gap.
- A drafted `caveated` outcome naming an untested hypothesis stays `caveated` and is not promoted.
- An abstention naming a hypothesis is refused: the kind and the link contradict each other.
- An abstention naming nothing at all is still refused, as it is today.

**At the decoding seam, in `internal/reasoning`.**

- A conclusion document proposing a hypothesis with no falsification condition is malformed.
- A conclusion may explain a hypothesis it proposed in the same document, by the ordinal that
  hypothesis will hold.
- A conclusion explaining an ordinal past everything shown and proposed is malformed.
- The rendered prompt for a fixed brief matches the regenerated golden, and the version is part of
  what the golden covers.

**At the round, in `internal/investigation`.**

- A round whose reasoner discovers its cause at the conclusion records the new hypothesis, admits
  the outcome against it, and records the untested gap.
- A round whose reasoner explains a hypothesis a dispatched read was justified by records no such
  gap.
- A round whose reasoner cannot make the link twice ends in an abstention naming the standard it
  missed.

**What this is not tested for, and cannot be.** Whether the reasoner actually proposes the
distractor now. Whether demoting to caveated makes the output more useful to a reader. Both are
empirical, both need a live provider, and `cmd/redherring` plus the scenario harness are the
instruments that answer them. A green suite here means the machinery behaves as specified and
nothing more.

## Out of Scope

- Requiring the explaining hypothesis to have predated the evidence. That would forbid discovery
  outright, and discovery is most of what an investigation is.
- A confidence score on the link. There is no score anywhere in an outcome, deliberately, and a
  number counting the reads that tested a hypothesis would be read as one.
- Automatically opening a further round to test a late hypothesis. Reinvestigation is a decision
  with its own trigger, and starting one from inside a round that just ended would spend a
  customer's limits on the platform's judgement.
- Requiring more than one hypothesis to be settled before a conclusion. It sounds like rigour and
  is a quota; a round that genuinely had one explanation would be forced to invent alternatives to
  discard.
