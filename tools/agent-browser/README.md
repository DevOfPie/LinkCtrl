# agent-browser

Two tools, one pinned manifest, added by
[M46.5](../../docs/build-notes/phase-details/m46.5.md):

- **`@playwright/cli` 0.1.18** — a browser an agent drives from a terminal:
  open, snapshot, click, fill, eval. Sessions persist between commands.
- **`@playwright/test` 1.62.1** — the runner for the kept spec in
  [`specs/`](specs/), which asserts what no template scan can: what the
  browser *refused*. A CSP violation appears in no markup diff and no HTTP
  status; it appears in a console.

Before this directory, five browser harnesses had been written here and
discarded — three deliberately, on the argument that a check nobody can re-run
protects nothing between the two occasions somebody ran it; two more without
even a discard note. The kept spec is the deliverable; the exploration that
produces one never is.

```sh
make browse                          # open a session on the test instance
make browse ARGS="snapshot"          # any playwright-cli command
make verify-ui                       # the kept spec, failures only
```

After `make browse`, the session persists — drive it directly:

```sh
tools/agent-browser/node_modules/.bin/playwright-cli snapshot
tools/agent-browser/node_modules/.bin/playwright-cli click e11
tools/agent-browser/node_modules/.bin/playwright-cli close
```

## The config file

`cli-config.json` names the **bundled chromium**. Without it the CLI launches
branded Chrome at `/opt/google/chrome/chrome`, which is not on this machine and
is not among `--browser`'s values (chrome, firefox, webkit, msedge — chromium
is reachable only through a config file). It is passed once, to
`open --config=`; the session keeps it, and later commands take no flag.

## Two Playwright versions, on purpose

The CLI at 0.1.18 bundles `playwright-core` **1.63.0-alpha** (chromium-1237).
The spec runs on stable **1.62.1** — the same pin, and therefore the same
engines, as [`tools/render-verify`](../render-verify/README.md)
(chromium-1234, firefox-1538, webkit-2336). Measured on 2026-08-11: no stable
1.63 exists, and **every** published `@playwright/cli` version bundles an alpha
core, so aligning the two means pinning verification evidence to an alpha.
Declined; the cost — ~700MB of duplicate engines (the cache went 1.2G → 1.9G
when the CLI's chromium was installed) and two versions to reason about — is
recorded in
[decisions.md](../../docs/build-notes/decisions.md#2026-08-11--m465-a-browser-an-agent-can-drive),
with the instruction to revisit when a stable 1.63 ships.

Because of that split, `node_modules` holds both: the alpha `playwright` is
hoisted to the top and the runner resolves its own nested 1.62.1. The tracked
`package-lock.json` is what makes that layout — and the bin shims —
reproducible, which is the same argument render-verify's README makes for its
lockfile.

## Setup

```sh
npm install --prefix tools/agent-browser
npm run --prefix tools/agent-browser install-browsers        # spec engines (1.62.1)
node tools/agent-browser/node_modules/playwright/cli.js install chromium   # CLI engine (1.63-alpha)
```

Engines live in `~/.cache/ms-playwright`, outside the repository. Both `make`
targets run the npm install for you; **neither downloads an engine silently** —
that is several hundred megabytes, and a target that quietly spends it is a
target nobody runs twice. A missing engine fails red, naming the install
command (verified: an empty `PLAYWRIGHT_BROWSERS_PATH` fails with *"run npx
playwright install"*, downloading nothing).

## The traps, so nobody rediscovers them

Each was found by driving the running test instance on 2026-08-11:

| Trap | What happens | Handle it |
| --- | --- | --- |
| Origin CSRF | `http.NewCrossOriginProtection` refuses a form POST without a matching `Origin` — but a browser navigating same-origin satisfies it. Proved: the sign-in POST returned **401, not 403** — evaluated, not refused | Nothing. No exemption is needed and none is added |
| htmx swaps kill element refs | `e11` became `f1e12` after a swap, and a fill against the old ref went nowhere, silently | Re-snapshot after every swap, or target by role and name |
| The product classifies the driver as a bot | `internal/analytics/useragent.go` lists `playwright`, `headlesschrome`, `puppeteer`, `selenium` | Only the redirect path's analytics care. A spec touching that path must say how it handles this; the kept spec does not touch it |
| An unstyled page is the default failure | `app.css` is generated and gitignored | Both targets drive the Docker instance, whose image builds its own stylesheet — the trap cannot bite there. Driving a locally-run server instead needs `make css` first |
| A failed sign-in charges a real lockout counter | Credentials also do not belong in a committed spec | The kept spec stays on `/login`, where layout, stylesheet, CSP and htmx are all live without a session |

## Opt-in, with a cadence

`verify-ui` is reachable from no other target — not `check`, not `ci-*`, not
`release-check` — for render-verify's reason: engines CI does not have and a
download no other target wants. What is different is that this check now has a
schedule: **every `X.9` review must answer it**, per
[phase-loop.md](../../docs/build-notes/phase-loop.md#two-milestones-that-do-not-end-like-the-others)'s
review section. That cadence, not CI, was the owner's choice (2026-08-11) over
four CI shapes offered and declined.

## Failure is the point

The spec was born red: run before the F206 fix was deployed, it failed on the
config assertion, and — with that assertion inverted once, as sabotage — on the
console assertion, printing the exact CSP refusal (*"Applying inline style
violates the following Content Security Policy directive 'style-src 'self''"*).
Restored by counter-edit, fix deployed, green: one line.

Green costs one line because `make verify-ui` reads the runner through
`--reporter=json` and [`report-failures.mjs`](report-failures.mjs) prints only
failures; a run in which **no test ran at all is red**, because "nothing
failed" and "nothing was checked" must not share an exit code.

## Files

| | |
| --- | --- |
| `cli-config.json` | the bundled-chromium session config `make browse` passes to `open` |
| `playwright.config.mjs` | test dir, base URL (`LINKCTRL_BASE_URL` overrides :8081), chromium only |
| `specs/clean-console.spec.mjs` | the kept spec: `/login` renders, htmx runs configured, console clean |
| `report-failures.mjs` | JSON-reporter filter: green in one line, red at exactly the assertion |
| `package.json` | both pins, exact, the way every vendored version here is pinned |
| `package-lock.json` | tracked, so the two-version layout and bin shims reproduce |
