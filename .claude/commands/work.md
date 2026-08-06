---
description: Resolve a route from the arguments and enter that route's loop
---

Read `docs/build-notes/work-loop.md` and follow it exactly. It is the grammar,
the route table, and what to do when a target or a kind is unknown.

`$ARGUMENTS` is `[target …] <kind> [flags]`. **The kind is the last token;
everything before it is a target.** No arguments at all → report the backlog for
every kind and recommend one, and **enter nothing**.

You resolve the route. You do not build. Having resolved it:

| Kind | Then |
| --- | --- |
| `phase` | You are the **orchestrator**. Read `docs/build-notes/phase-loop.md` and follow it exactly, from step 0. You hold steps 0, 1, 3.4–3.9 and 4, and every prompt to the owner. Step 2 and steps 3.1–3.3 go to a fresh worker, one per attempt, and you accept or reject its work against the tree before committing it. |
| `workflow` | Run the workflow loop in `work-loop.md`, from step 0. It is **not** delegated — one actor throughout, and that file says why. |

A target may name a **milestone** — `/work M45 phase`. It **bounds** the run: the
loop builds what it would have built, in the order it would have built it, and
stops when that milestone lands. It never skips ahead to it, and it never
weakens another stop condition. Already `done` → enter nothing and say so;
another phase → prompt.

An unknown target or an unknown kind is a **prompt**, before anything is spawned
and before any note is rewritten. Never route to the nearest match because it was
close. Then note the answer where `work-loop.md` says to note it, and nowhere
else.

`--revalidate` (also `-Revalidate`, `-reval`, `-r`) re-derives the route against
the tree and prompts with the result before the loop is entered. It changes
routing only — the loop it hands off to still resumes the way that loop resumes.

Do not skip validation, do not commit work you have not checked against its
definition of done, do not bundle two units of work into one commit, and do not
cross a phase boundary.
