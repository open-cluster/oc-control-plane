# Spec — Retire the pre-pivot planning record and move the governing documents to Go

Status: IMPLEMENTED 2026-07-27
Date: 2026-07-27
Repositories touched: the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService) and this one

## Problem Statement

The documents that govern the work live in the repository that is no longer doing the work.

Thirty-five documents sit here: twenty-six plans, seven decision records, a domain glossary,
and one architecture document. Seventeen of them describe work that has not happened yet, in
Go, in two other repositories. The rest describe work that already happened, in C#, in this
one. All of them are in the repository that has just been frozen and given retirement
criteria.

That is two separate problems wearing one appearance.

**The location problem.** A decision record that governs the Go control plane cannot live in
a repository scheduled for archive, because archiving it would take the decision with it.
Every edit to a specification currently means touching a frozen codebase, which either
erodes the freeze or makes the specifications go stale rather than be corrected. The five
relay and intake specifications already carry stale status lines — one says ready for
implementation when the work has been abandoned, another waits on a precondition that will
never land — and they are stale precisely because correcting them felt like editing a frozen
repository.

**The content problem, which is worse.** Most of this record describes a product that is no
longer being built. There are plans for an alerting engine that a decision record now says
is deliberately out of scope. There is an adoption roadmap written when the product was
identified as an observability platform, before the pivot to incident investigation. There
are eighteen implementation slices describing C# that is frozen. Someone reading this
directory cannot tell, from the directory, which documents describe the product and which
describe its history. Both are marked approved. Both read as instructions.

The distinguishing evidence is not in the documents but in how they were produced: the
documents that survive the pivot are the ones that came out of adversarial architecture
review against the current product identity. The rest predate that review, were never tested
against it, and mislead in proportion to how confident they sound.

And the two problems interact. Moving the governing documents without deciding what happens
to the rest leaves pointers from the new location into the old one. Four such pointers exist
today: two decision documents cite an implementation plan that would stay behind, and the
master plan cites the superseded plan it replaced, twice.

## Solution

One test decides where every document goes: **does it describe the product as it is now, and
govern work still to be done?**

Documents that pass move to the Go control plane, where the work happens and where they can
be corrected without touching a frozen repository. Documents that fail stay beside the
implementation they describe, marked as its record rather than as instructions. One document
that fails on identity rather than on age is deleted outright.

Every reference that would cross the new boundary is rewritten to name its repository, so a
pointer either resolves or says plainly that it points into the frozen record. A signpost is
left at the root of the frozen .NET reference repository (OCluster/Zyrenn.ConsumerService) saying what it now is and where everything went.

The result an engineer sees: one place holding documents that describe the product, none of
which contradict it, and a separate place holding the history, which is clearly labelled as
history.

## User Stories

1. As an engineer picking up the next slice, I want the specifications in the repository
   where I will write the code, so that I am not reading instructions out of a frozen
   codebase.
2. As an engineer, I want every document in the planning directory to describe the product as
   it is now, so that I do not have to date-check each one before trusting it.
3. As an engineer, I want a document describing work that already shipped to be visibly
   history, so that I do not implement it a second time.
4. As an engineer, I want the specification statuses to be true, so that "ready for
   implementation" means the work is ready rather than that nobody has updated the header.
5. As an engineer, I want an abandoned specification marked abandoned rather than deleted, so
   that the reasoning that produced the abandonment is not lost with it.
6. As an engineer, I want a reference from one document to another to resolve, so that
   following a citation is not an investigation.
7. As an engineer, I want a reference that crosses into the frozen record to say so, so that
   I know I am reading history before I read it rather than after.
8. As an engineer new to the project, I want the domain glossary in the active repository, so
   that the vocabulary I am asked to use is where I am working.
9. As an engineer, I want the decision records beside the code they constrain, so that a
   review can check the code against the decision without a second checkout.
10. As an engineer, I want to know which documents were produced by adversarial architecture
    review and which were not, so that I can calibrate how much a confident-sounding plan is
    worth.
11. As a reviewer, I want the plans that describe a deliberately unbuilt capability removed
    from the active set, so that a reviewer cannot cite them as a requirement.
12. As a reviewer, I want the classification rule stated once and applied uniformly, so that
    the placement of any individual document can be checked rather than argued.
13. As a reviewer, I want a document's move to be visible as a deletion here and an addition
    there, so that nothing is silently lost in transit.
14. As the founder, I want the observability-era adoption roadmap gone rather than archived,
    so that nothing in the repository still argues the product is an observability platform.
15. As the founder, I want the frozen repository to keep its own history intact, so that
    freezing costs nothing that was already paid for.
16. As the founder, I want the migration to be one reviewable change per repository, so that
    it can be judged in one sitting.
17. As the founder, I want a signpost left behind, so that anyone arriving at the old
    location is redirected rather than misled.
18. As a maintainer of the frozen repository, I want its remaining documents to be clearly the
    record of an implementation, so that nobody mistakes an approved slice plan for approved
    future work.
19. As a maintainer, I want the freeze to survive this change, so that moving documents does
    not become a precedent for editing the frozen implementation.
20. As an agent working on this project, I want a single directory whose every document is
    current, so that context loading does not pull in contradictory instructions.
21. As an agent, I want no document in the active set to describe an alerting engine, so that
    a plausible-looking plan cannot lead to building what the trigger-model decision record
    forbids.
22. As an agent, I want the specification statuses to encode their real blockers, so that
    sequencing can be read rather than reconstructed from conversation.
23. As a security reviewer, I want to know that no credential, endpoint, or customer
    identifier travels with a moved document, so that the move does not widen exposure.
24. As a security reviewer, I want the moved documents to land in a repository whose
    visibility matches their sensitivity, so that competitive planning material does not
    reach a repository destined to be public.
25. As a contributor to the Relay repository, I want the Relay's own documentation left where
    it is, so that an Apache-2.0 repository does not acquire proprietary planning material.
26. As an engineer resuming after a break, I want a moved document to be findable by the name
    it always had, so that a remembered filename still works.
27. As an engineer, I want the migration to state what it does not preserve, so that I do not
    later discover that a document's history is gone and assume it was an accident.
28. As an engineer, I want a mechanical check that references resolve, so that the next move
    or rename fails a build rather than quietly producing the problem this migration exists
    to fix.
29. As a reviewer, I want that check to fail when a document points into the frozen record
    without saying so, so that the distinction between current and historical does not decay.
30. As the founder, I want the count of documents in each category stated before the move, so
    that the outcome can be verified against the intent rather than accepted.
31. As an engineer, I want the frontend project to inherit the same vocabulary, so that a
    third repository does not start its own glossary.
32. As the founder, I want this migration to be small and finished, rather than an ongoing
    documentation programme.

## Implementation Decisions

**One classification rule, applied to every document.** A document moves if it describes the
product as it is now and governs work still to be done. Everything else stays or is deleted.
The rule is stated because thirty-four individual judgements are unreviewable, while one rule
plus a list of exceptions can be checked.

**Provenance is the evidence, not the date.** The documents that survive the pivot are the
ones produced by adversarial architecture review against the current product identity —
incident investigation, not observability. Documents that predate that review were never
tested against it. A confident status line on an untested document is what makes it
misleading, so status confidence is not evidence of currency.

**Seventeen documents move**: the domain glossary; all seven decision records; the
customer-execution architecture document; and eight plans — the Go migration plan, the Go
foundation specification, the four relay specifications, the intake specification, and the
autonomous investigation master plan. This specification moves with them.

**Seventeen plans stay** as the frozen implementation's own record: the two alerting plans,
the superseded platform master plan, the C# structural refactor, the implementation status
tracker, the six Stage 1A slices, and the six Stage 1B documents including the relay walking
skeleton. Each describes work performed in C#, and each is more useful beside that code than
away from it. They are not instructions and are marked accordingly.

**One document is deleted rather than moved or kept.** The Monoscope adoption roadmap was
written while the product was identified as an observability platform. It fails on identity
rather than on age, so archiving it would leave an argument in the repository for a position
the product has abandoned. Deletion is the point; the commit message carries what it was and
why it went, which is the part worth keeping.

**The abandoned protocol-synchronisation specification moves, marked abandoned.** Its
conclusion — that a consistency check whose reference is derived from the thing being checked
can only prove the artefact agrees with itself — is the most transferable thing in the set
and is not recorded anywhere else at that length. It moves with an explicit abandoned status
and a one-line reason, so it can be read as a lesson and never as a work item.

**Statuses are corrected on arrival, not before.** Each moved specification's status line is
rewritten to its true state as part of the move: the protocol synchronisation work is
abandoned, the end-to-end proof is unblocked because its stated precondition was removed
rather than met, and the downstream specifications keep their real dependencies. Correcting
them in the frozen repository first would be an edit to a frozen repository for no benefit.

**Every boundary-crossing reference is rewritten to name its repository.** Four exist today:
the customer-execution decision record and its architecture document both cite the Stage 1B
implementation plan, and the master plan cites the superseded platform plan twice. Each
becomes a reference that names the frozen repository explicitly. A reader then knows they are
being sent into the historical record before they follow the link, which is the difference
between a citation and a trap.

**The frozen repository gains one document at its root** stating what it is, what may change
in it, where the governing documents now live, and where the retirement criteria are recorded.
It is a signpost, not a summary; anyone arriving from a bookmark or a search result lands on
it.

**Git history is not preserved across the move.** A cross-repository copy loses it, and
reconstructing it would be a filter-repository exercise for seventeen documents whose history
remains fully intact in a repository that is not being deleted. The decision is recorded here
so that its discovery later is not mistaken for an accident.

**The destination mirrors the source layout**: the glossary at the repository root, decision
records and the architecture document under the same architecture path, plans under a plans
directory. A remembered filename still resolves, and no document acquires a new name in
transit.

**The implementation status tracker stays, and the Go work starts a new one.** Everything in
the existing tracker is a C# slice; carrying it forward would mean a tracker whose first two
thirds describe a frozen codebase. The Go repository's tracker begins at the foundation
slice, which is the first entry that would be true of it.

**Sensitivity determines destination.** The moved documents are competitive planning
material, so they land in the proprietary control-plane repository and not in the
Apache-2.0 Relay repository, which is destined to become public. The Relay's own protocol and
naming documentation stays where it is; nothing moves into it.

## Testing Decisions

**What makes a good test here.** It asserts something a reader could verify by following a
link: that every relative reference in the active documentation set resolves to a file that
exists, and that a reference into the frozen record is identifiable as such. It does not
assert which documents exist or what they say, because the set will change; it asserts that
the set is internally consistent, which must hold no matter how it changes.

**One new seam, and the reason it is worth adding.** The control-plane repository gains a
documentation-integrity test alongside its existing architecture gates. This is a new seam,
which the preference for reusing seams would normally argue against — but the failure it
prevents is exactly the failure this migration exists to fix, and that failure is silent. A
broken reference does not crash anything; it misleads whoever follows it, months later,
with no signal that anything is wrong. Review does not catch it because a reviewer sees the
document being edited, not the document that pointed at it.

**It lives with the existing gates** because it is the same kind of property: build-failing,
structural, and invisible to a reviewer looking at one file. The gates package already
establishes the pattern — a test over the repository's real structure rather than a lint rule
that can be suppressed inline.

**It must not be able to pass vacuously.** A test that walks a documentation tree and finds no
files reports success just as loudly as one that finds every reference sound. It fails if it
examined no documents, on the same reasoning that the storage gates fail when they parse no
production files.

**Scenarios.**

- A documentation set whose references all resolve passes.
- A reference to a file that does not exist fails, naming both the citing document and the
  missing target.
- A reference into the frozen repository that names that repository passes.
- A reference to a path that only existed before the migration fails.
- A run that examined no documents fails rather than passing.
- A document moved or renamed later, without its citations updated, fails.

**Prior art.** The control plane's architecture gates establish the shape: tests over the
repository's real structure, failing the build, with an explicit guard against passing
vacuously. The Relay's generated-code drift gate establishes the discipline of comparing what
is committed against what should be there. Neither needs a test double, and neither does this.

## Out of Scope

- Rewriting the content of any moved document beyond its status line and its
  boundary-crossing references. Several of the moved documents have known staleness in their
  bodies; correcting that is separate work, done per document, when the document is next used.
- The Relay repository's documentation. Nothing moves into it or out of it.
- The frontend project's documentation, which does not exist yet. It inherits the glossary
  from the control plane when it starts.
- Deleting or archiving the frozen repository. Its retirement criteria are recorded and
  unmet.
- Preserving git history across the repository boundary.
- Any change to the frozen implementation's code, tests, or project files. This migration
  moves and deletes documents and adds one signpost.
- Publishing the documentation as a site, or any rendering concern.
- A tracker or issue-tracker integration. Specifications stay file-based until a tracker is
  configured.
- Consolidating the moved documents with each other. Seventeen documents move as seventeen
  documents.

## Further Notes

The deletion is the only irreversible act here, and it is deliberate. Everything else can be
undone by moving a file back. The adoption roadmap is deleted rather than archived because an
archived document in a repository is still a document someone can find and cite, and the
specific harm it does is arguing that the product is something it is not. That harm does not
diminish with an "archived" header.

The integrity test is worth more than the migration it verifies. This move creates the
conditions for exactly one broken-reference incident; the test prevents every future one, and
future ones are likelier, because a rename is cheap and updating its citations is easy to
forget. The migration is the occasion for adding it rather than the reason.

There is a question this specification deliberately does not answer: whether the planning
substrate should eventually live in a repository of its own, separate from the control plane,
once the frontend project exists and three repositories share a glossary. Moving it twice
would be worse than moving it once, but the control plane is the right destination today —
it is where the work is, and a documents-only repository with one consumer is
infrastructure without a purpose. Revisit when the frontend repository exists and has
diverged enough to need its own vocabulary, which may never happen.
