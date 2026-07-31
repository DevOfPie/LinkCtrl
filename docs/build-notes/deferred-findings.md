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

| # | Finding | Where | Evidence | Severity | Reviewed |
| --- | --- | --- | --- | --- | --- |
| F2 | A stalled Redis stretches a link edit to about nine seconds | `internal/redirect/resolver.go`, `deleteFromRedis` | Measured while building M23, against a TCP proxy that accepts the connection and then never answers — which is what a stalled Redis looks like, as opposed to a refused connection, which fails in 264ms. `InvalidateAlias` took **9.07s** with the publish removed, so the time is the delete path alone: it makes three attempts, each given `RedisTimeout` (50ms in the test), and go-redis does not honour that context when it has to establish a connection to a server that never speaks. The measurement is in the transcript for M23; the same shape is reproducible with `redisProxy.blackholed(true)` in `test/integration/invalidation_test.go`. Predates M23 — the retry loop is M7's — and M23's own publish was made fire-and-forget so it adds nothing to this. | Moderate. Not data loss: the edit completes, the local tier is cleared, and correctness is unaffected. But `HTTP_REQUEST_TIMEOUT` is 15s, so an edit made while Redis is stalled comes within a few seconds of timing out, and a Redis stall is exactly when an operator is most likely to be changing things. | No |
| F1 | Release-notes extraction sweeps up the changelog's link-reference block | `.github/workflows/release.yml`, the "Extract this version's changelog section" step | The `awk` runs from `## [<version>]` to the next `## [` heading. The newest version is always the last section in `CHANGELOG.md`, so the trailing reference definitions are inside that range: the published v0.1.0 body carries `[Unreleased]: …` and `[0.1.0]: …` immediately before the `### Container image` section. Confirmed against the live release body, not inferred. | Cosmetic. Markdown renders link *definitions* as nothing, so a reader sees no artefact; the cruft is in the raw body only. It will recur on every release, because the newest section is last every time. | No |

## Closed

*(empty)*
