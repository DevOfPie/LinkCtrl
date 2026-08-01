# Upcoming decisions

Questions the phase loop will reach but has not reached yet, so they can be
answered at leisure instead of while the loop stands still waiting. Written by
`/preview-decisions`, which reads ahead of the build; see the trigger in
[workflow.md](workflow.md).

**This file holds questions, never answers.** An entry leaves it when it is
answered, and the answer is appended to [decisions.md](decisions.md) with its
`D` number when the milestone that uses it lands — carrying the date it was
*given* as well as the date it was used, so the trail shows the decision
predated the work. One direction, always: a decision recorded here and nowhere
else is a decision that will be re-taken by whoever builds the milestone.

An answer given here binds exactly as much as one given inside the loop. The
early timing is a scheduling convenience and not a lower standard, which is why
entries carry options and a recommendation like any other prompt.

**Assumptions are what make an early answer safe.** Each entry names what it
takes for granted about a tree that is not built yet. Validation re-checks those
when it reaches the milestone; a false assumption re-opens the question rather
than letting the milestone inherit a stale answer.

**Two sections, by what forces an answer.** *Open — a milestone needs this*
holds questions the loop will stand still on, and is read before building the
milestone named. *Open — nothing forces this* holds questions with no deadline:
a convention taken by judgement that nobody ratified, a thing built that could be
struck. Nothing stalls on that section, and it is meant to be read at leisure —
but a question is in it rather than in somebody's memory, which is the whole
point.

A question answered in conversation and nowhere else is a question that will be
re-asked, and answered differently. Write it here first, then answer it.

An entry whose milestone shipped without the question ever being asked leaves
this file with a one-line note in [decisions.md](decisions.md) saying it was
dropped and why — usually that the build answered it implicitly. It is not
silently deleted: "the question was not real" is a conclusion, and unrecorded
conclusions are what this file exists to stop.

---

## Open — a milestone needs this

### Every milestone from M26.5 on — does README describe the released product, or this branch?

**Needed by:** every commit, since `b068b73` made README.md and
docs/SECURITY.md part of the per-commit Docs gate. Forced at
[M45](phase-details/m45.md), where the README status line moves to 0.2.0.

**Currently behaving as B**, chosen by the orchestrator on 2026-07-31 without
being put to the owner. That is the reason this entry exists: the convention was
settled in prose, and the gate now enforces whatever it is.

| Option | Buys | Costs |
| --- | --- | --- |
| **B — describes this branch** *(recommended, and what the tree does)* | The gate is meaningful from the day it was added; a reader of the phase branch sees what the phase branch does. Matches what M23, M24 and M24.5 already did to README | Anyone reading README on `phase-2` sees features no released version has. The status line says "released as 0.1.0" while the feature table describes more than 0.1.0, which is a contradiction a careful reader will find |
| A — describes the released product | README is always true for somebody who installed a tag, which is who reads it | M23, M24 and M24.5's README edits become wrong and need reverting; the Docs gate becomes almost always a no-op, so it stops catching the drift it was added for; every phase's features land in README in one M45 lump |

**Default if unanswered:** B continues. It is what the tree does and what the
gate assumes.

**Assumes:** that CHANGELOG's `[Unreleased]` section remains how released and
unreleased work are told apart — true as of 2026-07-31, and the thing that makes
B survivable.

### M27 — is an invite a bearer link, or bound to the address it was issued to?

**Needed by:** [M27](phase-details/m27.md), the milestone after
[M26.6](phase-details/m26.6.md) — so the next validation but one. The milestone
requires both a mailed invite and "a copyable invite link either way", and those
two words are the whole question: a link that can be copied is a link that can be
forwarded.

| Option | Buys | Costs |
| --- | --- | --- |
| **A — bound to the invited address** *(recommended)* | A forwarded or leaked link cannot add a stranger to the organization. The audit trail stays honest: *invited alice@…, alice@… joined* is one fact rather than two hopes. Under `invite` mode the account it can create is the one that was named | The copyable link stops being self-service — the owner who pastes it into a group chat for "whoever picks this up" is doing something the product now refuses. Redemption gains an address comparison, and that comparison is one more place that must not answer *does this address have an account* (the milestone's own no-enumeration bullet). Someone who wants to join under a different address must be re-invited |
| B — bearer: whoever holds the link joins | The copyable link does exactly what its name says, with no matching logic and no new enumeration surface. Onboarding works when mail does not, which on a default mail-free instance is every time | The link is a membership credential sitting in a chat log or a mailbox forever-ish. A forwarded invite admits the wrong person silently, and the audit record shows the organization gained a member nobody chose. Under `invite` mode it is an account-creation grant for an arbitrary address |
| C — both, chosen per invite | Honest about the two real uses | Two redemption paths and two test matrices, and the weaker one becomes the default because it is the one that always works |

**Default if unanswered:** the loop stalls at M27's validation — this is not a
choice it may take. If the answer is *you decide*, A.

**Assumes:** the mailer stays optional and off by default (M26, D1), so on a
default instance the copyable link is the *only* delivery path and this question
decides the normal case rather than the fallback. Also that no `invitations`
table exists yet — verified, migrations run to `01100_mail_outbox.sql` and none
of them creates one.

### M27 — what role may an invite carry, and may it be ranked at or above the inviter's own?

**Needed by:** [M27](phase-details/m27.md). Redemption creates a membership
(D6), a membership carries a `role_id`, and M27 is the first code in the product
that writes one for somebody other than the registrant. The rank table that
governs this is [M28](phase-details/m28.md)'s, one milestone later.

| Option | Buys | Costs |
| --- | --- | --- |
| **A — at or below the inviter's own rank** *(recommended)* | The ceiling ships with the first writer instead of being retrofitted onto live invites. M28's rank table then formalises what M27 already enforces, which is the cheap direction | It settles a piece of M28's rank semantics inside M27 — the scope leak the milestone split exists to prevent. If M28's table lands different semantics, M27 is *reopened* rather than corrected by a successor, per the workflow rule |
| B — a fixed role, member management waits for M28 | Smallest possible surface: no rank comparison before the rank table exists | An owner cannot invite a co-owner or an admin until M28, which is the first thing a real organization tries. M28 then adds role choice to an invite form that already shipped, so the form is built twice |
| C — any built-in role, unbounded until M28 | The invite form is complete on the day it ships | An admin can invite an owner and be promoted by them an hour later. That is a privilege-escalation path shipped knowingly and live until M28 closes it |

This answer also decides the delegability M27 must record under D18: a key
holding `members.write` that can invite above its own reach is a key that widens
its own reach — D18's second limb — so B and C point at non-delegable, and A
lets the key inherit its creator's ceiling.

**Default if unanswered:** the loop stalls. If the answer is *you decide*, A.

**Assumes:** the four built-in roles and their ranks are unchanged —
`owner` 10, `admin` 20, `editor` 30, `viewer` 40, seeded in `00700_seed.sql` —
and every seeded role has `organization_id IS NULL`, so rank comparison is total
and no per-org custom role can tie. Also that M28 still owns the rank table and
still follows M27.

### M27 — how long does an invite stay redeemable, and is that a knob or a constant?

**Needed by:** [M27](phase-details/m27.md). The milestone requires a TTL and does
not name one.

| Option | Buys | Costs |
| --- | --- | --- |
| **A — `LINKCTRL_INVITE_TTL`, default 168h** *(recommended)* | Matches how every other duration in this configuration is expressed — `SESSION_ABSOLUTE_TTL`, `SESSION_IDLE_TTL`, `REDIRECT_TTL` — so it documents itself in the file a reader is already in. An operator whose relay is slow, or whose colleagues read mail weekly, is not stuck | One more setting on a surface already large enough that M26 called it an invitation to scope creep, for something most instances will never touch. The knob is also not the interesting part: the default is what nearly every instance gets, and A still has to choose it |
| B — a constant, seven days | Nothing to document, nothing to validate, nothing to get wrong | The one thing an operator cannot work around without a rebuild is time. It is the shape D5 rejected for audit retention — a constant quietly disposing of something nobody configured — and the same argument is already open in this file for M26's thirty-day outbox purge |
| C — no expiry; revocation only | An invite never mysteriously stops working, which is the support ticket this avoids | An unrevoked invite in an old mailbox is a permanent grant. Single-use bounds the blast radius to one account; nothing bounds it in time |

**Default if unanswered:** the loop stalls. If the answer is *you decide*, A at
168h.

**Assumes:** mail is delivered asynchronously through M26's outbox (D23), so the
clock starts before anything is actually sent and a short TTL is shorter than it
looks. Also that a copyable link exists on every path, so a lapsed invite is
re-issuable without waiting for mail to work.

### M28 — may an admin manage another admin, or only ranks strictly below their own?

**Needed by:** [M28](phase-details/m28.md), which requires the rank table be
written into its own file *before* code. This is the row that table cannot leave
blank; the milestone's own risk note calls it the security surface.

| Option | Buys | Costs |
| --- | --- | --- |
| **A — strictly below: equal rank is unmanageable** *(recommended)* | No lateral removal. Two admins who disagree cannot delete each other, and the whole escalation surface is one strict inequality, which is a thing a test can cover exhaustively | An organization with one owner and several admins cannot re-role or remove an admin while the owner is unavailable — and a single owner who is on holiday is the *normal* shape of a self-hosted instance, not an edge case. The peer case for owners then needs its own answer; the last-owner rule sets a floor, not a rule about equals |
| B — equals may manage equals, owners excepted | Admins are operationally useful without the owner, which is what most people mean by "admin" | An admin can remove the admin who invited them. One compromised admin session can strip every other admin and leave a single owner to clean up, and the only record that it happened is the audit log |
| C — symmetric at every rank, guarded only by the last-owner rule | One rule, stated once, applies everywhere | Any owner can remove any other owner. On a two-owner organization that is a coin flip in a dispute, and it is irreversible without database access |

**Default if unanswered:** the loop stalls at M28's validation. If the answer is
*you decide*, A.

**Assumes:** the seeded ranks are unchanged (see the M27 entry above), and that
`memberships` still has no writer other than `Register` — so no instance holds a
row this rule would retroactively invalidate.

### M28 — does a workspace-scoped membership narrow access, or only add to it?

**Needed by:** [M28](phase-details/m28.md), the first milestone that can write a
membership with `workspace_id` set. Until it does, the behaviour below has never
been observable on any instance.

**What the tree does now:** `GetUserPermissions`
([`auth.sql`](../../internal/store/query/auth.sql), `GetUserPermissions`)
selects `DISTINCT p.slug` across every membership matching
`m.workspace_id IS NULL OR m.workspace_id = w.id`, and `GetUserRoleInWorkspace`
returns the lowest `rank` of them. The evaluator therefore already **unions**,
and always has — nothing chose that, it is what the query does when a second row
finally exists.

| Option | Buys | Costs |
| --- | --- | --- |
| **A — union, as the evaluator already computes it** *(recommended)* | The RBAC evaluator is not touched in the same milestone that lands member management, which is the worst possible place to change how permissions resolve. "Roles add up" is the model the schema comment already describes | *Make this org admin a viewer in the finance workspace* becomes unexpressible, and that is the natural reading of a per-workspace role. The feature surprises in the direction of granting too much, which is the worse direction |
| B — a workspace-scoped membership overrides the org-wide one inside that workspace | The restriction people expect. Workspace-scoped roles become useful for limiting, not only for widening | Changes both evaluator queries. "Revoke by inserting a row" has to be re-tested against every permission check in the product, in the milestone that also lands members, workspaces and org creation |
| C — refuse to hold both at once; the COALESCE index becomes the rule | The question stops existing, enforced by a constraint rather than by query semantics | It also removes the additive case — editor across the org, admin in one workspace — which is the case that motivated workspace-scoped roles in the first place (D15) |

**Default if unanswered:** the loop stalls. If the answer is *you decide*, A —
it is what the code already does, and B is a change to the authorization path.

**Assumes:** no membership row anywhere has `workspace_id` set — verified,
`Register` is the only writer and passes `WorkspaceID: nil`
([`service.go`](../../internal/auth/service.go), `CreateMembership`). If M27
ships an invite path that sets it, this question is answered by whatever M27 did
and must be re-checked rather than inherited.

### M28 — deleting a workspace that still holds links: refuse, or confirm?

**Needed by:** [M28](phase-details/m28.md), which says in as many words that this
decision is recorded rather than left to the implementation.

**What the tree does now:** `links`, `tags` and `folders` all carry
`workspace_id ... REFERENCES workspaces(id) ON DELETE CASCADE`
(`00300_links.sql`). A hard delete takes the links with it; a soft delete
(`workspaces.deleted_at`) leaves them present but unreachable through a
workspace that no longer lists.

| Option | Buys | Costs |
| --- | --- | --- |
| **A — refused while any live link remains** *(recommended)* | No path by which one click takes redirects offline. Consistent with Phase 1 deciding against trash/restore: there is nowhere to undo this from, so the guard has to be in front | A throwaway workspace with one test link cannot be deleted until it is cleaned out, and moving links between workspaces is not built — so the only exit is deleting them one at a time. Friction lands hardest on exactly the disposable case that motivated workspace creation |
| B — allowed with typed confirmation, links deleted with it | A workspace is disposable, which is what a create button implies | Every alias in it stops resolving at once, with no restore UI. The confirmation is the only guard, and confirmations get typed |
| C — refuse on live links, allow when only archived ones remain | Archiving is already the retire-this-link verb, so the sequence is discoverable without documentation | A third rule to state, and it still deletes archived links' click history with nothing warning that analytics goes too |

**Default if unanswered:** the loop stalls. If the answer is *you decide*, A.

**Assumes:** no cross-workspace link move exists or is planned in Phase 2 — true
as of 2026-07-31, Plan.md has no such row — which is what makes A's cost real
rather than theoretical. Also that alias uniqueness stays instance-wide, so a
deleted workspace's aliases do not silently free up (D14).

## Open — nothing forces this

No deadline, no milestone waiting. Read when convenient; an answer here is worth
exactly what an answer anywhere else is worth.

### M26 — keep or strike the outbox's thirty-day purge?

Finished `mail_outbox` rows are deleted after thirty days by the existing
housekeeping reaper. **No bullet in [m26.md](phase-details/m26.md) asked for
it.** The worker flagged it, the orchestrator accepted it and named it in
`13df367`'s commit message as strikeable, and the owner has not said either way.

| Option | Buys | Costs |
| --- | --- | --- |
| **Keep** *(recommended)* | The outbox does not become the one table in the schema growing forever with nothing watching it. Matches the thirty-day link purge the same reaper already runs | Thirty days is a constant, not a setting, and it deletes delivery history nobody configured — which is the shape D5 rejected for the audit log. The recommendation is also the cheap one, since keeping it means doing nothing |
| Strike | The milestone ships exactly its bullets, and retention becomes its own decision with its own reasoning | The table grows unbounded until somebody schedules that decision |
| Keep, but make it a setting | Both, honestly | A configuration variable, its documentation and its test, for a table nobody has yet complained about |

**Default if unanswered:** it stays as built. Which is itself the thing worth
noticing — unasked-for work defaults to shipped unless somebody objects.

**Assumes:** `mail_outbox` stays the only table M26 added, and that no consumer
begins depending on old rows being readable. Both true as of 2026-07-31.

---

Entry format:

```markdown
### <milestone> — <the question in one sentence>

**Needed by:** M31, after M25 and M29 land.

| Option | Buys | Costs |
| --- | --- | --- |
| A — *recommended* | … | … (the recommended option states its own cost) |
| B | … | … |

**Default if unanswered:** what the loop does if it arrives here with no answer.

**Assumes:** the specific, falsifiable things this rests on.
```
