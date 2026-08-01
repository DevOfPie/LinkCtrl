# Deferred findings

Where out-of-spec issues go the moment they are found, instead of being fixed on
the spot or forgotten. Anything discovered while a milestone is in flight that is
not required by that milestone lands here as one row: what, where, the evidence
it is real, and a suspected severity.

**Nothing here is scheduled work.** The owner reviews each row and approves it
individually; approved rows become the phase's final milestone. An unreviewed row
is a report, not a commitment — which is what keeps "I noticed something" from
turning into unplanned scope.

Issues that make the *current* milestone's own claim false are in spec by
definition and get fixed immediately, whatever subsystem they turn up in. The
test is the claim, not the file.

The process around this — when to defer, what counts as work, what has to pass
before a phase PR — is in [workflow.md](workflow.md).

---

## Open

**Found in** names the milestone that was under way when the row was written —
not necessarily the milestone that caused the defect, which the evidence names
when it is known and different. It is written by whoever appends the row, at the
time, because it is unrecoverable afterwards.

| # | Found in | Finding | Where | Evidence | Severity | Reviewed |
| --- | --- | --- | --- | --- | --- | --- |
| F2 | M23 | A stalled Redis stretches a link edit to about nine seconds | `internal/redirect/resolver.go`, `deleteFromRedis` | Measured while building M23, against a TCP proxy that accepts the connection and then never answers — which is what a stalled Redis looks like, as opposed to a refused connection, which fails in 264ms. `InvalidateAlias` took **9.07s** with the publish removed, so the time is the delete path alone: it makes three attempts, each given `RedisTimeout` (50ms in the test), and go-redis does not honour that context when it has to establish a connection to a server that never speaks. The measurement is in the transcript for M23; the same shape is reproducible with `redisProxy.blackholed(true)` in `test/integration/invalidation_test.go`. Predates M23 — the retry loop is M7's — and M23's own publish was made fire-and-forget so it adds nothing to this. | Moderate. Not data loss: the edit completes, the local tier is cleared, and correctness is unaffected. But `HTTP_REQUEST_TIMEOUT` is 15s, so an edit made while Redis is stalled comes within a few seconds of timing out, and a Redis stall is exactly when an operator is most likely to be changing things. | No |
| F5 | M26 | Two shipped documents still say the audit log has no behaviour | `docs/SECURITY.md`, the "What is not defended" list — "**The audit log has no behaviour.** The table is partitioned and maintained; nothing writes to it. Do not rely on it for forensics yet."; and `README.md`, the "Not built yet" list — "**No audit log behaviour**, no folders, no custom domains…" | Noticed while making the same two lists true for M26's mailer. Both claims were true when written and are false now: [M21](phase-details/m21.md) landed a writer (`internal/audit`, wired in `cmd/linkctrl/main.go` as `auditSvc` and passed into the link service), a read API (`GET /api/v1/audit`), a per-table retention window and a growth metric, and `test/integration/audit_test.go` exercises all of it. SECURITY.md's line is the more consequential of the two: it explicitly tells a reader not to rely on the log for forensics, which is advice against a defence that now exists. | Minor, and documentation only — nothing in the product misbehaves. The consequence is an operator who reads SECURITY.md and does not turn on a retention window, or does not know a trail exists, because the file told them there is nothing there. It makes M21's own claim false rather than M26's, so it reopens M21 rather than arriving as new scope — which is the owner's call to schedule, not this milestone's to take. | No |
| F1 | 0.1.0 release | Release-notes extraction sweeps up the changelog's link-reference block | `.github/workflows/release.yml`, the "Extract this version's changelog section" step | The `awk` runs from `## [<version>]` to the next `## [` heading. The newest version is always the last section in `CHANGELOG.md`, so the trailing reference definitions are inside that range: the published v0.1.0 body carries `[Unreleased]: …` and `[0.1.0]: …` immediately before the `### Container image` section. Confirmed against the live release body, not inferred. | Cosmetic. Markdown renders link *definitions* as nothing, so a reader sees no artefact; the cruft is in the raw body only. It will recur on every release, because the newest section is last every time. | No |

## Closed

A row leaves *Open* when the work that makes it untrue has landed, and the
milestone that closed it is named. The finding itself is not edited — the
evidence is what it was, and rewriting it would hide the shape of the defect
from anyone reading back.

| # | Found in | Finding | Where | Evidence | Severity | Reviewed | Closed by |
| --- | --- | --- | --- | --- | --- | --- | --- |
| F3 | M24.5 | Dark mode has no effect on the UI: the light tokens are unlayered and beat every dark rule | `internal/ui/static/css/input.css` — light `:root` block at line 88, dark rules at lines 139–178 (`@media (prefers-color-scheme: dark)`) and 182 (`:root[data-theme="dark"]`) | Owner report, 2026-07-31, and confirmed by construction rather than inferred from the symptom. The light `:root` block sits at the top level of the file; both dark blocks sit inside `@layer base`. CSS cascade layers give **unlayered** normal declarations priority over layered ones regardless of specificity, so `:root { --t-surface: #f8fafc }` unconditionally beats `:root[data-theme="dark"] { --t-surface: #0f172a }`. Walking the built `app.css` top level confirms it survives the build: the dark override is inside the `@layer base { … }` construct, and `:root { color-scheme: light; … }` is emitted after `@layer utilities` closes, as its own unlayered top-level rule. Both dark paths are dead, the explicit override and the `prefers-color-scheme` one, which is why `color-scheme: dark` never takes either — matching "changing to Dark Mode does nothing". The server side is *correct* and is not implicated: the demo serves `data-theme="dark"`, `data-theme="light"` and no attribute across the three cookie states. **The milestone's own enforcement cannot catch this**: `TestTemplatesUseThemeTokensOnly` asserts templates name tokens, and the AA contrast figures are recorded in comments, so nothing tests that a token's *value* ever changes. A fix wants a test that fails on this specific cascade, not more contrast arithmetic. | **Critical.** [M24.5](phase-details/m24.5.md)'s headline claim — every dashboard page follows `prefers-color-scheme`, and an explicit override renders in the chosen theme — is false in the shipped build. The milestone was reopened rather than corrected by a successor, so the status row now reads `in progress (reopened)` and no longer asserts something untrue. The AA contrast claim is false by consequence: the dark figures describe values no browser applies. M24.5 was placed before M25, M31, M37, M38, M41, M43 and M44 precisely so those build inside a working theme; every UI milestone landing before this is fixed is written and reviewed against a dark theme that cannot be seen, which is the retrofit the ordering existed to avoid. | Yes — owner-reported; folded into reopened M24.5, 2026-07-31 | M24.5, 2026-07-31. Every `--t-*` block is now unlayered, so the dark selectors win on specificity and source order; `TestThemeTokensShareOneCascadeLayer` was shown failing against the stylesheet built from `b289a39` before it counted, and the live check now resolves the cascade against the composed stack rather than confirming the attribute. |
| F4 | M24.5 | The theme control sits in the page footer, where nobody looks for it | `internal/ui/templates/layout.html`, the `theme_switch` template near the footer | Owner report, 2026-07-31. Not a false claim: [m24.5.md](phase-details/m24.5.md) says the override is "settable from the dashboard" and never names a location, so the footer satisfies the milestone as written. It is a placement judgement the milestone did not make — appearance preferences are conventionally in account settings or the top-level chrome, and the footer is the one region of the page a person scanning for a setting will not read. Related to F3 only by file: the control works, it is merely hard to find, and moving it fixes nothing about F3. | Minor. No claim is false and nothing is broken. It is discoverability, and it costs a person one hunt through the page rather than any data or correctness. | Yes — owner-reported; folded into reopened M24.5, 2026-07-31 | M24.5, 2026-07-31. The control moved to `/account`, with the sign-in page rendering the same partial; `TestExactlyOneAppearanceControlPerPage` asserts the count on every page, so the layout's site cannot come back. |
