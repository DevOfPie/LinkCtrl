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

### M50.5 — where does an uploaded logo live?

**Needed by:** [M50.5](phase-details/m50.5.md), and it cannot be deferred into
the build: the answer decides whether [M57](phase-details/m57.md)'s conformance
test still passes, and that test is written a phase-half later.

This product has never stored a file. Three options, and each costs something it
has so far avoided.

| Option | Buys | Costs |
| --- | --- | --- |
| **A `bytea` column on `qr_codes`** *(recommended)* | Deletion comes free with the foreign-key cascades that already exist, backup and restore need no new procedure, and a single container stays a single container — which is the constraint M57 turns into a test | Binary in the row and in every `pg_dump`. A capped image is small, but the cap becomes a database sizing question rather than a disk one, and Postgres is the one dependency this product cannot degrade without |
| A filesystem path | Bytes stay out of the database, and serving is a file read | `docker-compose.yml` mounts only `pgdata`, so this needs a volume that does not exist and that `make demo-update` must not lose. It also makes deletion explicit everywhere a cascade would have handled it, and gives a multi-replica deployment a shared-storage problem it does not have today |
| An object store | The answer that scales, and the one an operator running many replicas would expect | A **new required dependency**, which [M57](phase-details/m57.md)'s single-container conformance test is written to forbid. Choosing this means amending that test's claim before it is written |

**Default if unanswered:** **A**. It is the only option that adds no
infrastructure and no new deletion path, and the caps are what keep its cost
bounded. Moving to B or C later is a migration of bytes, not of behaviour.

**Assumes:** that the caps M50.5 sets keep a stored image small enough for a
column to be uncontroversial — which is true only once those numbers exist, so
this answer is re-checked when they do; and that no milestone before M50.5
introduces file storage for its own reasons. None does.


### M55 — Does the update checker default on or off?

**Needed by:** [M55](phase-details/m55.md). The milestone builds either way and
deliberately does not pre-empt this; what changes is a sentence in
`docs/SECURITY.md` that is part of why somebody self-hosts this product.

`docs/SECURITY.md:73` currently reads *"No telemetry, no phone-home, no
third-party calls in the default configuration"*, and enumerates the four
connections that leave this product rather than counting them — because that row
said *two* until M45 and both of the missing ones were shipped features. An
update checker is the fifth, and it is the first that would be **on** without an
operator asking for it.

| Option | Buys | Costs |
| --- | --- | --- |
| **Off by default** *(recommended)* | The sentence above stays true, unedited. An operator who wants the check turns it on, which is the same shape `SMTP_HOST` and `FEED_URL` already have — the operator's connections are off until configured, and this joins that group without changing its rule | The people most likely to be running an outdated version are the least likely to find the setting. The feature exists and does nothing on almost every instance, which is close to not having built it. The recommendation states this against itself: the cheapest option to defend is not obviously the useful one |
| On by default, with an opt-out | The feature works for the operators it was requested for, without them knowing it exists | `SECURITY.md:73` has to be rewritten rather than extended, and *no phone-home in the default configuration* becomes *no phone-home except this*. That is a real change to a claim this product has made since Phase 1, and it is made on behalf of every operator who read it |
| On by default, prompted at first run | The operator decides knowingly, and the default is whatever they chose | There is no first-run prompt surface for instance-level settings; the setup form claims the instance and does not configure it. This invents one, inside a milestone that is otherwise a daily HTTP GET |

**Default if unanswered:** **off**. It is the only option that does not require
editing a security claim, and turning a default-off feature on later is a
configuration change where turning a default-on feature off later is an apology.

**Assumes:** that `docs/SECURITY.md:73` still enumerates rather than counts —
true and verified 2026-08-06 by reading it — and that no milestone between here
and M55 adds a sixth outbound connection. [M53](phase-details/m53.md) explicitly
adds none.

## Open — nothing forces this

No deadline, no milestone waiting. Read when convenient; an answer here is worth
exactly what an answer anywhere else is worth.

### An 'All Workspaces' dashboard scope — which phase, and whose milestone?

**Needed by:** nothing yet. It is the feature half of a queue row split on
2026-08-01; the other half — *the dashboard should show only the selected
workspace* — turned out to be already built, so only this remains.

The dashboard and links pages scope to the acting workspace and always have.
What does not exist anywhere is a way to see **across** workspaces at once: no
handler, no query and no UI takes an all-workspaces scope, and
`actor.WorkspaceID` is a single value threaded through the service layer rather
than a filter that could be widened.

| Option | Buys | Costs |
| --- | --- | --- |
| **Phase 3, beside *Moving links between workspaces*** *(recommended)* | The two cross-workspace capabilities land together, and they share the hard part — every scoped query in `internal/link` and `internal/analytics` assumes one workspace id. Phase 2 has no milestone this belongs inside | Somebody with several workspaces keeps switching to compare, for the whole of Phase 2 |
| A Phase 2 milestone of its own | The demo M33.5 is about to make multi-workspace instances the normal thing to look at, which is exactly when the gap gets noticed | It is a new scope row late in a phase whose remaining milestones are already substrate for each other, and it widens a query path M34–M37 are about to build on |
| Fold into [M37](phase-details/m37.md) | M37 is the dashboard milestone, so the surface is already being touched | M37 is about how a *dimension* is visualized, not which workspaces are in scope. Different question wearing the same page |

**Default if unanswered:** it stays unbuilt and unscheduled, which is the status
quo and costs nothing until somebody asks for it a second time.

**Assumes:** that the dashboard and links pages remain workspace-scoped — true
and verified on 2026-08-01 by reproduction, not by reading — and that no
milestone between here and M45 introduces a cross-workspace view for its own
reasons.

### <milestone> — <the question in one sentence>

**Needed by:** M31, after M25 and M29 land.

| Option | Buys | Costs |
| --- | --- | --- |
| A — *recommended* | … | … (the recommended option states its own cost) |
| B | … | … |

**Default if unanswered:** what the loop does if it arrives here with no answer.

**Assumes:** the specific, falsifiable things this rests on.
```
