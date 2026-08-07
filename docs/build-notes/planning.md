# Planning a requested feature

What to do when the owner asks for something the plan does not have. Written
after the first such request (dark mode, M24.5, 2026-07-31) so the second one
does not re-derive the path; that addition is the worked example throughout.

Defects are not features. A defect — anything making an existing claim false —
follows [workflow.md](workflow.md)'s issue trigger and
[deferred-findings.md](deferred-findings.md). This file is for capabilities the
product was never claimed to have.

The owner decides *whether*. This file decides *where and how*.

## 1. Establish absence

Search before writing anything: Plan.md and its scope tables,
[phase-details/](phase-details/), decisions.md, and the artifact itself — the
plan can omit what the code half-has, and the code can lack what a plan row
implies. (Dark mode was proven absent in both: no plan mention, and the
compiled stylesheet contained zero `dark:` variants.) Search synonyms, not just
the requested name.

Four outcomes: already built (point at it), already scheduled (point at the
row), partially scheduled (extend that milestone's file, not the table), or
absent — continue below.

## 2. Read before writing

The conventions decide most questions before taste gets a vote:

- [phase-details/_template.md](phase-details/_template.md) — the file format.
- [phase-details/README.md](phase-details/README.md) — the rules every
  milestone inherits, so the new file does not restate or contradict them.
- The phase's decisions table in Plan.md — the addition may touch a decision
  already taken; reference it by number rather than re-deciding it.
- The precedent: Phase 1 absorbed M18–M20 after its review, Phase 2 absorbed
  M24.5 after finalisation. Scope changes arrive as numbered milestones with
  their own definitions of done, never as riders on existing ones.

## 3. This phase, or a future one

**This phase** when all of:

- Its dependencies exist or are scheduled before it.
- It lands numerically before the pre-release review — work numbered after the
  last `X.9` ships unreviewed, so nothing is placed there.
- It does not re-open work an adversarial review has already passed. A review's
  value is its coverage claim; scheduling work that invalidates the claim makes
  it silently false.

And the strong signal *for* this phase: **it is substrate for scheduled work.**
If later milestones would build UI, schema or behaviour on top of it, landing
it early makes it a foundation those milestones build inside; landing it late
makes it a retrofit of everything they produced. This is the plan's own
ordering rule — substrates before consumers — applied to insertions. Dark mode
went in at M24.5, before seven UI-building milestones, for exactly this reason.

**A future phase** when any of:

- It needs capabilities this phase does not build.
- It would land after the pre-release review.
- It is listed in Plan.md's *Not in Phase N* with a reason. The request is then
  a request to reverse a recorded decision: surface that reason to the owner
  and let them reverse it knowingly, rather than scheduling around it.
- It is large enough to move the phase's success criteria or its release. That
  is a phase-boundary conversation, not an insertion.

A future-phase feature is parked, not remembered: a row in *Not in Phase N*
(or the next phase's candidate list) carrying its reason, and a decisions.md
entry for the why. The queue is the plan, never anybody's head.

### The size target: a phase stays under sixteen milestones

Fifteen at most, counting fractional insertions — they are milestones, they have
definitions of done, and a rule that counted only integers would be satisfied by
inserting. **Owner-set 2026-08-06, and explicitly revisitable.**

Phase 2 ran **33**: 25 integers, M21 through M45, and 8 insertions. It was the
phase that produced this target, and it is the number the target is set against.

An insertion that would take a phase past the target is not forbidden by
arithmetic — it is a **phase-boundary conversation**, the same as the last bullet
above. Either something moves to the next phase or the target moves knowingly,
and both are the owner's. What the target removes is the case where a phase grows
by one insertion at a time and nobody is ever the person who decided it was large.

**It has been tested twice in one day, and it worked as designed both times.**
Phase 3 was planned at fifteen with every slot spent.

On 2026-08-07 the owner asked for QR logos, was shown that they fitted no
milestone and that the phase had no room, was offered the alternatives with their
costs — park them for Phase 4, or trade [M50](phase-details/m50.md)'s slot — and
chose to have both. Sixteen.

Hours later an independent review of that milestone found it was two: an upload
surface and the compositing, with a clean seam and unpriced work hiding in a
sub-clause. The owner split it rather than keep one fat milestone. Seventeen.

**The second one is the more interesting test**, because it is the trap this
section warns about arriving from the other direction. A count invites fatter
milestones, and the way this one was caught was a review reading the milestone
against *reviewability* rather than against the number. The rule's own sentence
holds: if the target and a milestone's reviewability disagree, the target gives.

Phase 3 therefore runs at **seventeen**, and that is a recorded exception rather
than a new ceiling: **the rule here stays fifteen**. Moving the target for one
phase and moving it for every phase after are different decisions, and only the
first was taken — twice.

**The trap, stated because a target invites it.** A count is a number people plan
*to*, so the cheapest way to satisfy this rule is fatter milestones — the same
scope in fewer files, each one less reviewable, which is worse than the size it
was avoiding. Phase 2's own evidence is that a milestone is the unit of review:
its two adversarial reviews found defects *"no single milestone's definition of
done can catch, because each milestone was internally consistent"*. Merging two
milestones to make a count hides exactly that seam. If the target and a
milestone's reviewability disagree, the target is the thing that gives.

## 4. Numbering

- **Integers** are the work the phase planned. Never renumber them: the
  ordering table's dependency edges reference numbers, so renumbering rewrites
  the contract and every file that cites it.
- **`X.1`–`X.8`** are insertions, placed in dependency order. Prefer the
  mid-band (`X.5`) so later insertions can still fall on either side.
- **`X.9` is reserved for scheduled reviews.** A review covers everything
  numerically below it, so keeping reviews at the top of the band guarantees
  any insertion between `X` and `X+1` stays inside the nearest following
  review's range. (Phase 1's M0.5 predates this reservation and stays where
  history put it; Phase 2's reviews are M32.9 and M44.9.)

## 5. What a scope addition writes

Five artifacts. None optional — each keeps a different document true.

| # | Artifact | What it must say |
| --- | --- | --- |
| 1 | `phase-details/mN.md`, from the template | Falsifiable definitions of done. Deliberate exclusions stated, each with its reason. Enforcement named as a mechanism — a test that fails, not review vigilance. |
| 2 | Plan.md ordering-table row | Dependencies, with soft edges marked as ordering preferences. Discharges: the promise it closes, or "owner-added scope" with the date — never an invented one. The milestone-count sentence stays true. |
| 3 | `phase-details/README.md` status row | Plus an inherited-rules row **only** if the milestone constrains all later work (M24.5's template scan qualifies; most milestones do not). |
| 4 | decisions.md entry, with index row | The why, dated. Placement reasoning, design constraints that forced choices, what deliberately stays out. |
| 5 | The restraint list | Do **not** touch: a decisions table headed "taken before the plan was finalised"; README's *Not built yet* unless the absence is a production surprise; any committed decision-log entry — a later entry corrects, never an edit. |

## 6. Verify

- `make check-links` — and hand-check links in files not yet committed, because
  the gate walks `git ls-files` only.
- Re-read the nearest following review's range. The insertion must sit inside
  it; if it does not, the numbering is wrong, not the review.
- Any count, status line or table the addition made false is part of the
  addition, not cleanup for later.

## 7. Review it before anything is built against it

**A plan is reviewed by somebody who did not write it, before the first
milestone is built against it.** Owner-set 2026-08-07.

The phase already budgets two adversarial reviews for the *code*. Nothing
budgeted one for the plan, and the plan is what every one of those milestones is
validated against — a definition of done that is wrong is wrong in the tree
before anybody looks.

### When

- **A phase plan** — always, before its first milestone starts.
- **A scope addition that adds a milestone** — always, before it is built.
- **Not** an annotation, a re-homing row, a deferral, or a count correction.
  Reviewing a one-line row costs more than it can find.

### Who

An independent reviewer, and the two words carry weight:

- **A different model from the writer's, and a more capable one where the choice
  exists.** A reviewer sharing the writer's model shares its blind spots, and
  the failures this rule was written from were not carelessness — they were an
  author reading past rules they had themselves cited on the same page. Where
  only one model is available, use a fresh context that has never seen the
  writer's reasoning; the independence is the point and the model is how it is
  bought.
- **Given the plan, this repository's standards, and nothing else.** No
  conclusions, no summary of the author's intent, and above all no hint of which
  parts the author thinks are weak — that is the fastest way to get a review
  that confirms rather than checks. Name the standards as files:
  [_template.md](phase-details/_template.md), the inherited-rules table in
  [phase-details/README.md](phase-details/README.md), this file,
  [workflow.md](workflow.md), and [phase-loop.md](phase-loop.md).

### What it is pointed at

Dimensions, never conclusions. At least:

1. **Internal consistency** — counts, numbering, the ordering table against the
   status table against the files on disk, dependency edges.
2. **Falsifiability** — which *done means* bullets would pass review while being
   untestable, and where enforcement is named as a mechanism that cannot catch
   what it is cited for.
3. **Evidence accuracy** — plans here cite `file:line` heavily. **The reviewer
   checks them against the tree**, and proves absence claims with its own
   searches rather than trusting the wording.
4. **Conformance** — the five artifacts, the inherited rules one at a time, and
   whether anything contradicts a recorded decision without saying it reverses
   it.
5. **Ordering and hidden edges** — two milestones that would edit the same
   files, or one needing what a later one builds.
6. **Is any milestone two?** The size target's trap is fatter milestones, and a
   reviewer reading for reviewability is what catches it.
7. **What is missing**, given what the phase is for.

### What happens to what it finds

- **Verified before it is acted on.** A review is evidence, not a verdict. Both
  reviews that produced this rule contained one item that did not survive being
  checked, and acting on either would have made the plan worse.
- Documentation-level corrections are **made** — the standing approval in
  [workflow.md](workflow.md#standing-rules) already covers them.
- Anything that changes a definition of done, a scope row or a milestone count
  is a **prompt**, not a correction.
- Findings the owner declines are recorded, because *we decided this did not
  matter* is the decision this project keeps losing.

### What it costs

A review of a phase plan is a large agent run and it is not free. It is worth it
at a phase boundary and at a milestone insertion, and it is not worth it for
every edit to a planning file — which is what the *When* list above is for. If
the cost stops being worth paying, narrow the trigger deliberately and in
writing rather than skipping it quietly.
