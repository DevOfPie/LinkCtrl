# render-verify

Re-checks the claims [M26.5](../../docs/build-notes/phase-details/m26.5.md) makes
about where the header's two popover panels land, in Blink, Gecko and WebKit.

```sh
make verify-render                                   # all three engines
make verify-render RENDER_ARGS="--engine webkit"     # one
```

M26.5 says the positioning "is therefore explicit and verified in a browser at
each engine, not assumed from the markup", and lists it under Risks as "the one
part of the milestone that has to be looked at rather than asserted". It was
looked at, by a harness that lived in a session scratchpad and is gone. This is
that harness, kept — so the check can be repeated, and so the next milestone
making a rendered-geometry claim does not start from nothing.

## What it asserts

| | Claim, as M26.5 or the template states it |
| --- | --- |
| Popover API | `HTMLElement.prototype.showPopover` exists and both panels carry `popover="auto"` — not `manual`, which "dismisses on neither Escape nor an outside click" |
| Top layer | "whose containing block is the viewport rather than an ancestor" — the header is given a `transform`, which would capture a merely-`fixed` descendant, and the open panel must not move |
| Vertical | `top-[3.75rem]`: "the bar is h-14 (3.5rem) plus a 1px border, so this clears it by 3px" |
| Fixed, not absolute | the panel does not scroll away with the document |
| Horizontal | `right-[max(1rem,calc(50%-35rem))]`: "one value, correct at every width, no media query" — measured against the header container's own content edge at 375, 640, 900, 1152, 1153, 1440 and 1920px, either side of the 72rem hinge |
| No overflow | `max-w-[calc(100vw-2rem)]`: the panel stays inside the window at the narrowest width |
| Invoker-independent | "the email address in the identity button has no fixed width" — a 425px-wider invoker must not move either panel's right edge |
| Escape | the reason D24 replaced `<details>`: "a disclosure cannot close on Escape" |
| Exclusivity | "Only one auto popover is open at a time — showing either closes the other" |
| Keyboard | both invokers open from a focused Enter |

Every measurement is taken against something the page renders — the header's
bottom edge, the nav container's content edge — rather than against a number
retyped from the template. Two constants are asserted directly, `60px` and the
`3px` clearance, because those are the claim rather than a restatement of it.

## Where the page under test comes from

**Neither a running instance nor a committed HTML fixture.** The page is
assembled at run time out of the product's own files:

- every class string that decides geometry — the `<header>`'s, its `<nav>`
  container's, both invokers', both panels' — is read out of
  `internal/ui/templates/layout.html` and
  `internal/ui/templates/partials/nav.html`;
- the stylesheet the page loads is `internal/ui/static/css/app.css`, the same
  build the server serves.

The panels' *contents* are invented — a few links, a few list items. No claim
here is about them: `w-56`, `w-80` and the top/right pair fix the panel box
whatever is inside it.

Why not the other two:

- **A committed static fixture** would be a second copy of the header, free to
  stay green while the template it claims to describe drifts away from it. A
  harness that verifies its own copy of the markup verifies nothing.
- **The running test instance on :8081** would be the real page, and would also
  make a geometry check depend on Docker, Postgres, Redis, a migrated schema, a
  claimed account and a session cookie. Every one of those is a way for this to
  fail red for a reason that has nothing to do with where a panel sits, which is
  how a gate stops being believed. It would also put the check out of reach of
  anyone who has only cloned the repository.

So this needs **no stack running**, and never talks to :8081 or :8080. The one
generated input it does need is `internal/ui/static/css/app.css`, which is
untracked and built by `make css`; when it is missing the harness says so and
names that command rather than rendering an unstyled page and failing on
geometry that was never applied.

The fixture is served over `http://127.0.0.1:<ephemeral>` by Node's own
`http` module rather than opened as `file://`, because that is one security
context in two of the three engines and a different one in the third.

When the template changes shape enough that a class string cannot be found, the
extractor fails by name — which file, what it looked for, and that M26.5's
claims want re-reading — instead of throwing somewhere further down.

## Setup

Node is required, here and in `tools/agent-browser/` — nowhere the product ships.
Plan.md **D25** is what permits that: *shipped code stays stdlib-only; tooling
that only verifies it may use Node.* Nothing here is imported by, built into or
executed by the product — `go build` never sees `tools/`, and the container image
has no Node in it.

```sh
npm install --prefix tools/render-verify
npx --prefix tools/render-verify playwright install chromium firefox webkit
```

On a machine missing the shared libraries the engines link against, the second
command needs `--with-deps` and root. Playwright keeps the engines in
`~/.cache/ms-playwright`, outside this repository, and `node_modules/` is
gitignored.

`package-lock.json` **is** tracked, unlike the rest. `package.json` pins the one
direct dependency exactly, which is how the Makefile pins every other vendored
version — but that leaves what Playwright itself depends on floating, and the
point of keeping this harness is that somebody else can run the same check and
get the same answer. 1.7KB is a cheap price for that.

`make verify-render` runs the `npm install` for you if `node_modules` is absent.
It deliberately does not download the engines for you — that is several hundred
megabytes, and a target that quietly spends it is a target nobody runs twice.

## Opt-in, on purpose

`verify-render` is reachable from nothing. Not `make check`, not `ci-build`,
`ci-lint`, `ci-test`, `ci-integration` or `ci-image-smoke`, not `release-check`,
and not from any of their prerequisites. The engines are a download no other
target wants and CI does not have; the claims it protects change when a header
milestone changes them, which is when somebody should be running this and reading
the numbers.

## Failure is the point

Break the thing and it says so. Removing `inset-auto` from the identity panel —
the class the template comment says stops the UA stylesheet's `[popover] {
inset: 0 }` from stretching the panel across the window — fails in all three
engines with the measured offsets side by side:

```
FAIL  right edge tracks the container's content edge
        at 1440px the identity menu panel sits 1216px from the window edge,
        the header container's content edge sits 160px — they must be the same edge
```

The process exits non-zero on any failed assertion, and on any precondition it
cannot satisfy.

## Files

| | |
| --- | --- |
| `verify.mjs` | entry point: preflight, fixture server, engine loop, report, exit code |
| `fixture.mjs` | reads the templates, resolves the handful of Go template actions the two invokers use, assembles the three page variants |
| `checks.mjs` | one function per claim |
| `package.json` | the single dependency, pinned exactly, the way the Makefile pins every other vendored version |
| `package-lock.json` | tracked, so the transitive tree is pinned too and the check reproduces |
