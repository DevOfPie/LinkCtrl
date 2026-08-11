# M<N> — <title>

**Depends on:** `[M30](m30.md)`-style links, or "nothing". Say when an edge is an
*ordering preference* rather than a hard dependency — a soft edge presented as a
hard one hides a scheduling choice.
**Discharges:** the Plan.md scope row(s), known limitation(s), or repo promise
this closes. If it closes none, say so rather than inventing one.

One or two sentences on what this is and why it sits here. Not a restatement of
the title.

## Done means

Verifiable claims, in the Phase 1 idiom: things a skeptical reviewer can check,
not intentions. Each bullet should be falsifiable.

- Prefer "asserted by test" over "is correct".
- Name the test or file where one already exists.
- A `file:line` citation claims the tree at this milestone's own commit and
  nothing later (W39). One written after the milestone landed carries the
  commit — `gates.go:167` at `abc1234` — and a symbol name beats a line number
  where one exists, because line numbers rot on every insertion above them.
- If a number is claimed, say where it was measured.
- If something is deliberately *not* done, say that here rather than leaving its
  absence to be discovered.

Anything touching the redirect path must state how the <20ms cached SLO is
re-verified. Anything adding a destination-writing surface must state that it
passes [M30](m30.md)'s tier check. Anything emitting audit events depends on
[M21](m21.md); anything notifying depends on [M22](m22.md).

## Risks

What could go wrong, or what is genuinely unknown until the work starts. "Low" is
an acceptable answer; an empty section is not, because it reads as "not
considered" rather than "considered and small".

---

Rules every milestone inherits — SLO re-verification, privacy stance, permission
seeding, sabotage discipline, OpenAPI and contract parity — are in
[README.md](README.md) and are **not** repeated per file.
