# Phase 1 — the milestones added after the review

Released as 0.1.0. History, kept because these are the definitions of done the
implementations are still held to.

Only M18, M19 and M20 have detail. Phase 1's first eighteen milestones were
summarized by area in [Plan.md](../../../Plan.md#build-status) and never carried
per-milestone specifications — these three were written after the phase was
already running, when the completeness review grew its scope.

| Milestone | State |
| --- | --- |
| **M18 — separate management and link hostnames** | **Done.** `APP_BASE_URL` and `LINK_BASE_URL` both default to `BASE_URL`, so an existing single-host deployment is unaffected; set to different hosts, the router dispatches on `Host` and each tree answers only its own paths. A wrong-host request is `404`, never a cross-host redirect. `short_url` is built from the link origin, the CSRF trusted origin follows the dashboard host, and `/healthz` and `/readyz` answer on every hostname including ones never configured, because probes do not know the operator's names. Reserved aliases stay enforced on both hosts. |
| **M19 — post-release defect fixes, and a demo seeder** | **Done.** Effective status is derived rather than stored, so an expired link reports as expired everywhere and `?status=expired` matches it. `visitors` and `is_first_visit` are documented as dormant instead of described as working, and stay under partition maintenance and retention so the day something writes to them the guarantees already apply. The deletion notice says what recovery is. `lctl demo` / `make demo` fills an instance with a workspace worth looking at. |
| **M20 — root redirect on the link domain** | **Done.** Every requirement below holds, verified live and under test. |

## M20 in detail

M18 gave the instance a second public hostname and left its root a bare `404`:
`/{alias}` does not match `/`, and the dashboard routes that used to answer there
moved to the other host. Anyone who trimmed a short link back to the domain, or
typed it out of curiosity, got nothing. Every commercial shortener points that
page somewhere, and the operator is the only party who knows where.

An operator can now set a destination for `https://lnk.example.com/`, and every
row below holds:

| Requirement | Why it is stated |
| --- | --- |
| Stored on the domain row, as an additive column | It is a property of the hostname, not of a workspace or a link. The `domains` table already exists and already has the row this describes. |
| Guarded by a new `domains.write` permission, granted to owner and admin | Phase 1 has no per-domain owner to check: the default domain is instance-wide with a null organization, so "the domain owner" resolves to whoever administers the instance. The permission is the honest form of that, and it becomes a real ownership check in Phase 2 when a workspace can bring its own hostname — [M39](m39.md). |
| Only in effect when the hosts are actually separate | On a single-host deployment `/` is the dashboard, and a root redirect there would take the dashboard away from the person who set it. Refused rather than silently ignored. |
| Destination validated by `ValidateDestination`, unchanged | Same scheme allowlist and same private, loopback and metadata refusals as any other destination. A root redirect that skipped them would be a cleaner SSRF than the one the validator exists to stop, because it needs no link and no alias. |
| `302`, like every other redirect here | Same reason: a `301` cached in browsers and intermediaries cannot be recalled, and this is the destination most likely to be changed later. |
| Unset stays `404` | The current behaviour, and the one that reveals nothing. No default marketing page, no "powered by" — an instance that says nothing is a valid choice. |
| Resolved from cache, never a query per request | It sits on the redirect tree under the same 20ms budget as an alias, and the root of a link domain is a page crawlers and scanners hit often. Cached with invalidation on change, the way a link snapshot already is. |
| Not counted as a click | There is no link, so there is no `link_id` to attribute it to. Root visits stay out of `click_events` rather than acquiring a synthetic link to hang off. |
| Changing it is an audit-log event once the audit log has behavior | It is a setting that redirects every stray visitor to the whole domain, which is exactly the class of change worth being able to ask about afterwards. Discharged by [M21](m21.md). |

## M19 in detail

The three defects, each with what "fixed" means:

| Defect | Fix |
| --- | --- |
| `links.status` is never set to `expired` | Only `active` and `archived` are ever written. The redirect path is correct — it reads `expires_at` and answers `410` — but the management surface reports an expired link as `"status":"active"`, and the UI's *Expired* filter is an option that can never match a row. [operations.md](../../operations.md#troubleshooting) tells an operator diagnosing an unexpected `410` to check the link's status, which will tell them the link is fine. Fixed by deriving effective status in one place that both the list and the resolver agree with, rather than by adding a job to write a column that is stale the moment it is written. `disabled` is in the same enum and the same position; it is out of scope only because nothing offers to set it either — and stays that way by decision D10, see [M43](m43.md). |
| The `visitors` table is dead | Nothing writes it and nothing reads it, and `click_events.is_first_visit` is the same one column down: always `false`, under a comment claiming the rollup computes it, which no rollup does. The milestone forced a choice; the choice taken was neither of the two it offered. Both stay dormant and both stay under partition maintenance and retention, because the alternative fails in the direction that matters — a table dropped from those lists that later acquires a writer puts its rows in the default partition, which retention never drops, making the dormant table the one place raw visitor data is kept forever. What was actually wrong was the description, so the comments now say dormant instead of describing work that does not happen. They stay dormant through Phase 2 as well (decision D12); [M45](m45.md) trues up the remaining comments. |
| The deletion notice promises a button that does not exist | The UI says "Link deleted. It stays restorable for 30 days." [usage.md](../../usage.md) is straight about the truth — "recovery inside the 30 days is a database operation, not a button" — and `RestoreLink` is guarded by `deleted_at IS NULL`, so it refuses soft-deleted rows by design. Fixed by making the notice say what recovery is. Adding a trash view instead would be a scope change, and Phase 1 already decided against it. |

## The seeder

`lctl seed` exists and is for load testing: a hundred thousand links called
`ld0`…`ld99999`, no destination rows, random visitor hashes. It is the wrong shape
for looking at the product. `make demo` fills an empty instance with something
worth a screenshot — a workspace of plausible links, thirty days of history with
weekday seasonality and a launch spike, and every status the UI can render,
including an archived link, an expired campaign and one in the trash.

Two requirements make it worth committing rather than keeping as a snippet: links
are created through the same service call the REST API makes, so seeding runs the
same validation and alias policy a client does and cannot invent states the
product cannot reach; and backfilled click rows match what the ingester would have
written column for column — no IP anywhere, referrers already reduced to a host,
`is_first_visit` false, and the exact device and browser strings `Classify` emits.
A seeder that writes rows the application could not have produced tests nothing
and teaches the reader something false.
