# The add-on ABI, and the promise attached to it

An add-on reaches LinkCtrl through a fixed set of functions it imports from the
host, and through nothing else. **That set is the ABI.** There is no second
surface: no socket, no file, no shared table, no environment. Enumerating the
functions therefore enumerates the whole contract, which is what makes the
promise below something a publisher can rely on rather than a sentiment.

The host owns the definition and add-ons consume a **generated SDK**, so the
contract has one author. The definition lives in `internal/addon/abi` — the
`Functions`, `Records`, `Statuses` and `LogLevels` values in that package — and
everything else is produced from it by `make abi-sdk`:

| Generated | Is |
| --- | --- |
| `sdk/` | The Go package an add-on imports. Depends on the standard library and nothing else, which a test proves by compiling a consumer against it with the module cache turned off |
| The tables below | The function list, the statuses and the records, between this page's generated markers. The prose around them is written by hand |
| The host module | Not a file: `internal/addon/hostabi.go` builds wazero's parameter types from the same `Functions` slice, so host and guest cannot disagree about a signature |

`make generate` runs it beside sqlc and openapi, and `make check-generate` fails
on a diff — so a hand-edited SDK does not survive CI.

## The version, and what a manifest declares

The ABI has a version of its own, and it is not the product's. It follows
**semantic versioning with deprecation windows** — owner-set 2026-08-18, against
a recommendation of path versioning like `/api/v1`; the reasoning is in
[build-notes/decisions.md](build-notes/decisions.md).

An add-on's manifest declares one integer, `abi_version`. That integer is the
ABI's **generation**: the component a breaking change moves.

| While the ABI is | The breaking axis is | So `abi_version` is |
| --- | --- | --- |
| `0.x` | the minor — SemVer's own rule for major version zero | the minor: `1` means "built against `0.1.x`" |
| `1.0` and later | the major | the major |

The generation is what the host checks at load, before it reads a single byte of
wasm:

- **built against a newer generation → refused.** The module may import a
  function this host does not define, and finding that out at the first call is
  finding it out in production.
- **built against a retired generation → refused.** Retired means past the end of
  its deprecation window, below.
- **anything in between loads**, including a module built against an older patch
  or minor of the same generation. That is what the promise buys: an add-on
  published once keeps working across every additive release of its generation.

The one thing the manifest cannot express is which *patch* a module was built
against, so a module using a function added in `0.1.2` and loaded on a `0.1.0`
host is not refused at load — its import does not resolve and instantiation
fails, naming the function. The manifest was not grown a second version field for
it because the failure is loud, immediate, and names its own cause. A publisher
who needs to support both hosts probes with
[the availability status](#every-function-answers-the-same-way) instead.

The version the ABI is at right now is in the generated table below, and
`sdk.ABIVersion` carries the same string into the module.

### The `migrations` field, and why every file is named

An add-on that declares `storage.own_schema` may ship DDL, in a `migrations/`
directory beside its module, in goose SQL. The **host** runs it — at load, before
the listener opens, inside the schema the add-on owns and as the add-on's own
database role. That is what keeps *DDL is additive within a minor version* a
promise somebody can keep rather than one every publisher re-implements.

The manifest names each file with its own digest:

```json
"permissions": ["storage.own_schema"],
"migrations": [
  { "file": "00001_initial.sql", "sha256": "…" },
  { "file": "00002_add_index.sql", "sha256": "…" }
]
```

Three rules, and each is a refusal rather than a warning:

- **a file listed here and not on disk** refuses the add-on;
- **a `.sql` file on disk and not listed here** refuses the add-on. The set is
  closed for the same reason the `permissions` vocabulary is: DDL an operator did
  not write and the manifest does not describe is DDL nobody agreed to;
- **a file whose bytes are not the digest** refuses the add-on, which is the
  `module` field's rule applied to the DDL beside it.

Filenames are goose's: a version number, an underscore, and `.sql`. A name goose
cannot read a version out of is refused here rather than silently ignored, because
a migration that never ran is the worst way to find out. `.go` migrations are not
available — the host cannot compile a publisher's Go.

Compute the digests with the same tool the module's uses:

```console
$ sha256sum addon.wasm migrations/*.sql
```

`down` migrations are never run by the host. Write them if you like; nothing calls
them, and rolling a release back does not roll its DDL back.

## What counts as breaking

The point of writing this down is that *is this minor or major* stops being a
mood. Work through the table; the first row that matches decides.

| Change | It is |
| --- | --- |
| Changing the parameters of a function **no released version of this product implements** — one still declared and refused everywhere — or a field of a record only such functions carry | **neither — no version moves.** The case is spelled out [below](#a-function-nothing-implements-has-no-signature-to-break), because it is the one this table could not decide |
| Putting a function **behind a permission it did not cost**, or behind a narrower one, while **no released version of this product publishes this generation of the ABI** | **neither — no version moves.** The other case this table could not decide, spelled out [below](#a-permission-added-before-the-abi-was-published). From the release that publishes the generation, the next row applies |
| Removing a function, or renaming one | **breaking** |
| Changing a function's parameters — count, order, kind, or what a value means | **breaking** |
| Narrowing what a function accepts, or what it will do with what it accepts | **breaking** |
| Changing which status a function returns for a case it already handled | **breaking** |
| Removing a field from a record, renaming one, or changing its type | **breaking** |
| Changing the calling convention, the host module name, or a status's number | **breaking** |
| Adding a function | additive |
| Adding a field to a record | additive — a record is a JSON object and a consumer ignores what it does not know |
| Adding a status for a case that previously answered `ErrInternal` | additive |
| **Implementing a function that was declared and refused** | additive. This is the one that would otherwise cost a generation per limb, and it is deliberately additive: a module that already handles `ErrNotAvailable` keeps working, and one that does not was already broken |
| Widening what a function accepts | additive |
| Loosening a status into a success | additive |
| Changing a doc comment, this page's prose, or an error's wording | neither — no version moves |

Two cases the table does not settle, answered here so nobody has to decide twice:

- **A bug fix that changes an observable answer** is breaking if an add-on could
  reasonably have relied on the old answer, and additive if the old answer was a
  crash, a panic or a status contradicting its own documentation. Write which one
  it was in the changelog entry either way.
- **A record field that stops being populated** without being removed is
  breaking. A field present and always empty is worse than a field that is gone,
  because nothing fails.

If a real case in front of you is not decidable by the above, **that is a defect
in this document**, and it is fixed here in the same change that raised it. This
sentence exists because the judgement call that cost this project nine days of red
CI was exactly this shape.

It has been used once already, before anything was published against this page:
the first row of the table and the subsection below were added when M61's own
review pointed out that this document gave two answers for the change every
milestone after it is certain to make.

### A function nothing implements has no signature to break

The first row of the table is that case. Three milestones of this phase finish a
record — M64 `HTTPRequest` and `HTTPResponse`, M65 `SessionClaim` and
`MintedSession`, M66 `RedirectEvent` — and under the ordinary rows each of those
costs a generation, which is the exact cost the declared-and-refused pattern
exists to avoid.

**The rule: while every released host answers `ErrNotAvailable` for a function,
that function's parameters, and the records only such functions carry, move no
version at all.** Not a minor, not a patch — the same *neither* a doc change
gets. What was promised when the function was declared is its **name**, the
**status** it answers, and that a module handling `ErrNotAvailable` keeps
working. Its parameters were explicitly [not promised](#what-is-not-promised),
and nothing can have depended on them, because every call was refused before it
reached one.

An earlier wording of that bullet said such a signature *"may change within
`0.x`"*, which is why this subsection exists: `0.x` is ambiguous between *no
version moves* and *the minor moves*, and the minor **is** the generation. The
answer is the first of those.

Two conditions bound it, and both are checkable rather than judged:

- **Released, not unimplemented in a working tree.** A function becomes live in a
  release, and that release is what fixes its signature. From then on the
  ordinary rows apply and changing it is breaking.
- **A record is inside the carve-out only while *every* function carrying it is
  refused by every released host**, and outside it the moment one of them goes
  live. `Function.Carries` in `internal/addon/abi` is the list to read; it is
  there so this is a question with an answer rather than an opinion.

It is not free, and the cost falls in one place. A module built against the older
SDK, run on the host that implements the function with different parameters,
**fails to instantiate** — the import does not resolve, and the failure names the
function, exactly as the patch-version case above does. So the change is
announced in [../CHANGELOG.md](../CHANGELOG.md) under `Changed` even though no
version moves, and a publisher who compiled against a refusal recompiles when it
stops being one. A module that branches on `ErrNotAvailable` and recompiles is
unaffected, which is the whole reason the pattern is worth its cost.

### A permission added before the ABI was published

The table's second *no version moves* row is this case, and it is narrower than the
first. A live function that starts costing a permission is **narrowing what it will
do with what it accepts**, which the row of that name calls breaking — and that row
is right from the moment anybody could be running the function.

**The rule: while no released version of this product publishes a generation of the
ABI, moving a function of that generation behind a permission moves no version.**
Not a minor, not a patch. There is no published host whose behaviour changed and no
add-on that could have compiled against the old cost, because no host has offered
the function to anybody yet.

The condition is one a reader checks rather than one this page asserts:

- **Published in a release, not present in a working tree.** A generation is
  published by the release that ships it, and [../CHANGELOG.md](../CHANGELOG.md) is
  where that is visible: while this ABI's own first appearance is still under
  `[Unreleased]`, no host has published it. From the release that does, every
  function's cost is fixed and this carve-out is spent.
- **The current cost is always in [the function table](#the-abi)**, generated
  from the host's own definition, and the SDK's doc comment names it at the call
  site. So a publisher
  never has to reason about which version changed what: they read what the function
  costs today, and their manifest either declares it or does not.

It has been used exactly once, and this subsection exists because that use was made
before the rule was written down. `config_get` cost nothing when the ABI was first
written and costs `config.read` from the release that publishes it — the same
release, so no host ever offered the free form. The reasoning is in
[build-notes/decisions.md](build-notes/decisions.md).

**Unlike the signature carve-out, this one announces nothing** — and the difference
is worth stating, because the two conditions look alike. A refused function's
signature can change while its generation is published, so there are publishers to
tell. A function's *cost* cannot: the release that publishes a generation is what
publishes every cost in it, so this carve-out only ever applies while there is
nobody to announce to. The one reader it can still cost anything is somebody
tracking an unreleased SDK, and what they owe is one line of manifest: the call
answers `ErrDenied`, naming the function, and the doc comment above it names the
permission.

## The deprecation window

A function or a generation is **deprecated** first and removed later, never
removed directly.

> **The minimum window is two of this product's minor releases and 90 days,
> whichever ends later**, counted from the release that announced the
> deprecation.

Two releases rather than one, because an operator who skips a release must still
see a deprecation before it becomes a removal. Ninety days rather than a release
count alone, because releases here have been as close together as one day and a
window measured only in releases can close inside a week.

`MinimumGeneration` in `internal/addon/abi` is where a closed window becomes
behaviour: raising it is what actually retires a generation, and it may not be
raised before the window ends.

A deprecation is announced in **four** places, and all four are required:

1. **The function's own definition**, whose `Deprecated` field the generator
   emits into the SDK as a Go `Deprecated:` marker. That is the one an add-on's
   author sees without reading anything — their editor and `staticcheck` say so
   at the call site.
2. **This page's generated table**, which marks the function and names the
   earliest version it may be removed in.
3. **[../CHANGELOG.md](../CHANGELOG.md)**, under `Deprecated`, in the release that
   announces it — that release is what starts the clock.
4. **The release that removes it**, under `Removed`, naming what replaced it.

## What a function costs

An add-on declares what it needs in its manifest's `permissions` array, and **the
host refuses everything else**. Each function in the table below names the
permission it costs, or names none; a call whose permission is not in the manifest
is `ErrDenied`, counted in
`linkctrl_addon_refusals_total{addon,permission}`, and logged with the function
beside it.

**The refusal comes before the availability status.** A module that declared
nothing gets `ErrDenied` from `template_render`, not `ErrNotAvailable` — so probing
for a capability, which this page invites you to do, only tells you about
capabilities you asked for. A module that *did* declare the grant gets
`ErrNotAvailable` from the same call until the release that implements it, which
is the whole of the difference between the two statuses. The example was
`storage_query` until the release that implemented it; which functions are live is
the **Status** column of the table below, and it is generated, so it is the one
place worth reading for that.

**The vocabulary is closed.** A `permissions` entry that is not one of the tokens
in [the permission table](#permissions) refuses the add-on at load, for the same
reason an unknown manifest *field* does: a declaration this host cannot interpret
is a manifest written for a host that is not this one, and there is no safe
direction to guess in. The one thing that is *not* a refusal is a token this
version publishes and no version grants yet — `redirect.inline` is that case
today. Such an add-on loads, does not hold the grant, and gets a warning at boot
naming what it asked for and did not get; `linkctrl_addon_info` carries what it
**holds**, so the difference is visible in a scrape.

**Two functions cost nothing**, and that is a decision rather than an oversight.
`abi_version` reports a constant. `log` is the capability that was granted on
purpose — a module's stdout and stderr are discarded precisely so that reaching an
operator's log is something it has to be given, and this is the giving. Requiring
a declaration for it would put a line in every manifest and buy nothing: a module
refused the log still runs, and now silently.

**What is ungated is not what is trusted.** Because every loaded module can reach
`log` — including one whose `permissions` array is empty — the host neutralizes the
message before the line is written, and bounds it at 4 KiB.

**The rule is stated as what survives, not as what is caught.** A **graphic**
character reaches the line as itself — every letter, mark, digit, punctuation mark,
symbol and space, in every script, with the one exception named below — and anything
else becomes its escape: a
newline, a control character, an ANSI escape, every format and bidirectional
control, every unassigned or private-use code point, and the few characters that
are graphic by category yet render as nothing. Two consequences are worth knowing
before you write a message. The zero-width joiner and non-joiner are format
characters, so an emoji sequence or an Indic conjunct built from them arrives
escaped; write the text, not the joiner. And a code point assigned by a Unicode
revision newer than the host's Go toolchain is not yet a graphic character to it, so
it arrives escaped until that host is rebuilt.

**One graphic character is escaped anyway**: a backslash is doubled. Without that,
the two characters `\` and `n` in your message reached the line as the same bytes an
escaped newline did, and a reader could not tell which you wrote — and it is what
makes the host's truncation mark, `…\(truncated)`, something a message cannot end
with by accident or on purpose. Expect a Windows path or a regular expression to
arrive with its backslashes doubled.

**The exceptions run the other way and are named**: Unicode's
`Prepended_Concatenation_Mark` property — today `U+0600`–`U+0605`, `U+06DD`, `U+070F`,
`U+0890`, `U+0891`, `U+08E2`, `U+110BD` and `U+110CD`, the Arabic, Syriac and Kaithi
signs that scope the digits that follow them — is left alone, because a boundary that
mangles Arabic is worse than the thing it prevents. It is read from the property and
not from a list, so a host built against a newer Unicode carries whatever that
revision added to it: the enumerated form of this list shipped two members short.

A module cannot forge a record that reads as the host's, cannot arrange bytes in
front of a reader to be overlooked, and cannot make a complete message read as a
truncated one. Nothing is refused for any of it and a message needing none of it
arrives as it was written, backslashes aside, so this costs a well-behaved add-on
nothing.

A grant is **held, never inferred**. Nothing about an add-on's code, its name, its
other declarations or the order it was installed in widens what it may call — and
holding one is the operator's act of installing a module whose manifest asks for
it, rather than anything a role or a credential confers. The comparison with this
product's own permission model, and why the two are parallel mechanisms rather
than one, is in
[build-notes/decisions.md](build-notes/decisions.md).

## What a storage statement may and may not do

`storage_query` and `storage_exec` are one grant, `storage.own_schema`, and the
schema boundary is the whole of it. There is no row-level or column-level form, and
nothing here reaches another add-on's schema or this product's tables — unless that
add-on granted you the reach itself, which it can, because it owns its schema and
the host reports the grant rather than preventing it.

What holds, and it is enforced by Postgres rather than by a parser here:

- **the schema is yours and nothing else is.** Your add-on's role owns
  `addon_<name>` and holds no privilege on any table outside it, so
  `SELECT * FROM public.links` is `ErrDenied` — not because a search path failed to
  resolve, which it never would, but because the role may not read it. The same
  answer for another add-on's schema. `pg_catalog` and `information_schema` stay
  readable, because Postgres does not make them revocable; you can see that the
  product's tables exist and not a row of one;
- **`ErrDenied` and `ErrInvalid` mean different things.** `ErrDenied` from a
  statement is the boundary — you reached outside your schema — and `ErrInvalid` is
  your statement: a syntax error, a column that is not there, an argument the ABI
  does not carry. The error text never crosses, deliberately: a Postgres message
  names tables and constraints, and a module that could read one could print this
  product's schema into somebody's page. The host logs the detail;
- **one statement per call.** The host parses through Postgres's extended protocol,
  so a payload carrying two commands is refused. Batch by calling twice;
- **a query cannot write.** `storage_query` runs in a `READ ONLY` transaction, so
  which of the two functions you called is a fact rather than a description;
- **arguments are a JSON array** of strings, numbers, booleans and nulls, bound as
  `$1`, `$2` and so on. An object or an array inside it is `ErrInvalid` — whether
  you meant `jsonb` or a record is not knowable from the value — so pass JSON as a
  string and cast it in the statement;
- **rows come back as a JSON array of objects**, keyed by column name. Two columns
  of one name are refused rather than collapsed, so alias them. A column type with
  no JSON form is `ErrInvalid`; cast it to text if you want a shape you chose;
- **there are bounds, and they are not configurable.** One statement gets five
  seconds, a result gets a megabyte, and one add-on gets four connections. A
  statement crossing into the host is bounded at 64 KiB like every other value;
- **your search path is your own schema**, pinned per statement, so unqualified
  names resolve to your tables. Re-pointing it inside a `DO` block is allowed and
  buys nothing, because the search path was never the boundary.

**Nothing caps how much you store.** An operator sees
`linkctrl_addon_schema_bytes{addon}` and decides what to do about it, which is the
same answer this product gives its own audit log. Growing without bound is
something you should be able to defend.

**Store it in your schema and nowhere else, and keep it to yourself; the host checks
rather than trusts.** At every load it asks Postgres what your role owns, what is in
your schema, and what you have granted on that schema to anybody but yourself — and
refuses your add-on if any of the three answers is wrong. So anything of yours
outside your schema works once and never boots again, and so does a `GRANT` on your
own schema to another role or to `PUBLIC`. Two cases are worth naming because
Postgres permits them and the check does not:

- a **large object** is in no schema at all, and your role may create one. The host
  also counts them and publishes the count. There is no ABI for large objects and
  there is not going to be one: a `bytea` column in your own table is the shape this
  product accounts for.
- a **temporary table** is in `pg_temp`, which is not your schema either. On most
  instances `CREATE TEMP TABLE` is refused outright, because installing a storage
  add-on revokes the privilege from `PUBLIC`; where the instance's database user does
  not own the database it is not refused, and then the load post-condition catches it
  instead. Either way it is not a place to put anything. A `SELECT` with a CTE does
  what a scratch table would, inside one statement, which is all you get anyway.

**Inside your own schema you may do as you like to your own data**, including
things the host's own *DDL is additive* rule forbids: drop a column, rewrite a
table, change a type. The rule protects readers, and you have none you did not
create yourself: the host gives no add-on a way to reach another's schema, and
nothing of this product's reads yours. What the host cannot stop is *you* handing
your data out — you own the schema, so `GRANT USAGE ON SCHEMA … TO PUBLIC` and a
`GRANT SELECT` beside it work, and another add-on can then read your tables. It
notices instead: the load post-condition reports every grant on your schema whose
grantee is not your own role, and refuses to load you until an operator revokes
it. So *no other reader* is yours to keep, and what stays additive is your
**host-visible** contract within one ABI generation.

## Every function answers the same way

One convention for all of them, because one convention is one thing to learn.

- Every function returns a single `i32`. Zero or a positive number is success; a
  negative number is a status.
- A value the guest passes crosses as a **(pointer, length)** pair. Zero length
  with a null pointer is legal and means empty.
- A value the host returns crosses into a buffer **the guest owns**, passed as a
  **(pointer, capacity)** pair, and the return value is the size the value
  occupies. If that exceeds the capacity offered, **nothing was written** and the
  caller retries with a buffer that size. So no call ever has to ask for a size
  first, and the generated SDK does the retry for you.
- At most one out parameter, and it is last.

The host never allocates inside a module. A guest that exports an allocator hands
the host a way to run guest code at a moment the guest did not choose, and the
first thing that reaches for is a module that traps inside its allocator while
the host holds a lock.

A function this ABI declares and this host has not implemented yet answers
**`ErrNotAvailable`** — a status a module can branch on, which is what lets one
module work against two hosts. Probing for a capability is the intended use.

## What a module exports

The functions above are what a module *imports*. There is one function it must
**export**, and it is how the host hands over a request:

```go
//go:wasmexport linkctrl_http_handle
func handle() int32 { … }
```

Required only of an add-on that declared `routes.own_prefix`; ignored for one
that did not. It takes no arguments and returns an `int32`, read the way a host
function's answer is read — a negative number is one of the statuses in the table
below, anything else is success.

**Nothing is passed in and nothing is returned out**, deliberately: the calling
convention already has a way to move a record across, and a second one for this
direction would double what there is to learn. Inside the handler, call
`http_request_read` to see what you were asked and `http_response_write` to
answer. Both are `ErrNotFound` outside a request — which includes package
initialization, since the host makes an instance per request and attaches the
request after the module has initialized.

Three things follow from that, and each of them is a real constraint rather than
a note:

- **A module cannot keep state between two requests.** Its memory is new every
  time. State a flow needs across requests belongs in the schema
  `storage.own_schema` grants, where it also survives a restart and is visible to
  every replica of the instance.
- **A response is one record, and writing twice is `ErrInvalid`.** The first
  write stands. A module that could keep writing could hold a connection open,
  and a module that can hold one open can hold every one open.
- **A handler that returns without writing is a failure**, answered as one. There
  is no implicit empty page.

What a response may carry is bounded by the host and checked at the moment you
write it, so a refusal reaches you as a status from your own call rather than as a
page that differs from what you asked for: `content_type` is `text/plain`,
`application/json`, or **empty**, which is the ordinary case and means the host
wraps your body in the dashboard's own page, escaped. `text/html` is refused —
the host owns the HTML, which is what makes "an add-on cannot inject markup" a
property of this boundary rather than of a filter. A `location` is answered `302`
and never a permanent redirect; a `set_cookie` name has to begin with one of your
declared `cookie_prefixes`, and the host applies its own `Path`, `Secure`,
`HttpOnly` and `SameSite`.

## What is not promised

- **Stability, while the ABI is `0.x`.** A generation may be retired with the
  window above and nothing longer. The ABI becomes `1.0` when the contract is
  stable, and that is a release's statement to make, not this page's.
- **The signature of a function that is declared and refused**, or the fields of
  a record only such functions carry. Until a released host implements it, its
  parameters move as the behaviour behind them is built, and **no version moves
  with them** — the rule, its two conditions and its one loud failure mode are
  [above](#a-function-nothing-implements-has-no-signature-to-break). Its *name*
  is fixed, and so is the status it answers.
- **That a function keeps costing the same permission.** The vocabulary is closed
  and each entry is enumerated, but which grant a given function is behind is the
  host's to decide, and moving a function to a *narrower* grant is a breaking
  change under the table above — *narrowing what a function will do with what it
  accepts* — while moving one to a grant an add-on already holds is additive. Before
  a generation has been published in a release there is nothing to break, and
  [that carve-out](#a-permission-added-before-the-abi-was-published) is bounded by
  the same *released* condition as the signature one.
  Adding a permission to the vocabulary is additive. Making one grantable that was
  not is additive, and is announced under `Added`.
- **Anything about a module's own contents.** An add-on's version, its language,
  its dependencies and its release numbering are its author's business. Only the
  boundary is versioned here.
- **A raw client address, ever.** Not a promise of restraint — a property of the
  surface. No function in the table below hands a module a client's address in
  any form, and the record that carries redirect data is bound to what
  `click_events` may carry, prefix-derived and country-level, asserted by a test
  that reads the column list out of the migration. An add-on cannot store what it
  is never handed. See [SECURITY.md](SECURITY.md).
- **A cookie of the host's, ever.** Also a property of the surface. LinkCtrl's
  sessions are server-side and opaque, so the `Cookie` header *is* the
  credential — a record carrying it verbatim would let an add-on act as whoever
  is signed in. Instead a manifest declares `cookie_prefixes`, each of which must
  begin with the add-on's own name and an underscore, and the request record
  carries the cookies matching one of them and nothing else. The same prefixes
  bound what `set_cookie` may name, because a cookie an add-on may not read is
  one it must not be able to overwrite. Two consequences worth stating: an
  add-on that needs some *other* cookie of the host's cannot have it, and two
  add-ons can neither read nor deny each other's cookies. The namespace comes
  from the name rather than from whoever installed first, and that alone was not
  enough: `oidc` may declare `oidc_x`, which is a prefix of every prefix add-on
  `oidc_x` is allowed to declare. So two names standing in a `name + "_"` prefix
  relation are **both refused at load** — neither is awarded the other's
  namespace, and the instance says which two directories to rename.

## The ABI

<!-- BEGIN GENERATED: the function table -->

The ABI is **0.1.0**, generation **1**, and this host loads generation 1 or newer.

| Function | Since | Requires | Status | What it is |
| --- | --- | --- | --- | --- |
| `abi_version`<br>`sdk.HostABIVersion()` | 0.1.0 | — | **live** | HostABIVersion is the ABI version of the host this module is running in. A module's manifest declares the generation it was built against and the host refuses a mismatch before instantiation, so this is not how a module checks compatibility — it is how one logs what it is talking to, and how it decides whether a function added in a later patch is worth probing for. |
| `log`<br>`sdk.Log(level string, message string)` | 0.1.0 | — | **live** | Log writes one line to the host's logger, attributed to this add-on. It is the only way out: a module's stdout and stderr are discarded, because routing them into an operator's log is a capability and the host grants none it was not asked for. The host adds the add-on's name; a message that repeats it is noise. An unknown level is ErrInvalid rather than a silent default, so a typo does not become a line nobody greps for. The message is neutralized before it is written and bounded at 4 KiB, and the rule is stated as what survives rather than as what is caught: a graphic character reaches the line as itself, in any script, and everything else becomes its escape — a newline, a control character, an ANSI escape, every format and bidirectional control, every unassigned or private-use code point, and the few characters that are graphic by category and render as nothing. So an invisible code point appears in the line rather than acting on whoever reads it, and a code point Unicode adds after the host was built is escaped rather than let through. One graphic character does not reach the line as itself: a backslash is doubled, so that the two characters \ and n cannot be mistaken for an escaped newline, and a module cannot spell the host's own truncation mark. The named exceptions run the other way: Unicode's prepended concatenation marks — the Arabic, Syriac and Kaithi signs that scope the digits after them — are left alone, read from Unicode's property rather than from a list, so a host built against a newer revision carries the marks it added. Nothing is refused for any of it, and a message that needed none arrives as it was written, backslashes aside. |
| `config_get`<br>`sdk.ConfigGet(key string)` | 0.1.0 | `config.read` | **live** | ConfigGet reads one of this add-on's own settings. The key must be one the add-on's manifest declares; anything else is ErrDenied, which is what scopes the function to the add-on rather than to the instance — there is no way to ask for another add-on's setting or for one of this product's own configuration values. A declared setting with no value yet answers with the default the manifest gave it, and ErrNotFound only when it declared none. Values are edited in the Add-on manager; until a host implements that, every answer is a declared default. |
| `storage_query`<br>`sdk.StorageQuery(sql string, args []byte)` | 0.1.0 | `storage.own_schema` | **live** | StorageQuery runs a read against the Postgres schema this add-on owns. The schema boundary is the whole of the permission: an add-on names no database, no connection and no search_path, and a statement that reaches outside its own schema is refused rather than executed — ErrDenied, which is distinguishable from ErrInvalid so that a module can tell confinement from its own mistake. One statement per call: the host parses through the extended protocol, so a payload carrying two is refused. The read is a read at the server, in a READ ONLY transaction, so this function cannot be used to write. Arguments are a JSON array of strings, numbers, booleans and nulls; pass JSON as a string and cast it. Rows come back as a JSON array of objects keyed by column name, and a result with two columns of one name is refused rather than collapsed. |
| `storage_exec`<br>`sdk.StorageExec(sql string, args []byte)` | 0.1.0 | `storage.own_schema` | **live** | StorageExec runs a write against the Postgres schema this add-on owns. Migrations are not this function: the host runs an add-on's migrations, which is what keeps *DDL is additive within a minor version* a promise somebody can keep — the add-on ships them in its own `migrations/` directory and names each with its digest in the manifest, and the host applies them at load inside the same schema this function writes to. Everything StorageQuery says about the boundary, the single statement and the arguments applies here too; what differs is that the transaction is not read-only. |
| `http_request_read`<br>`sdk.HTTPRequestRead()` | 0.1.0 | `routes.own_prefix` | **live** | HTTPRequestRead reads the request that reached one of this add-on's routes. It answers ErrNotFound outside a request, which is what a module calling it from package initialization gets — an instance is made per request and its initialization runs before the request is attached, so this is the ordinary answer during init rather than an edge case. Read twice in one request it answers the same record twice: the host holds it, the guest does not consume it. |
| `http_response_write`<br>`sdk.HTTPResponseWrite(response []byte)` | 0.1.0 | `routes.own_prefix` | **live** | HTTPResponseWrite answers the request that reached one of this add-on's routes. Called twice for one request it is ErrInvalid: a response is one record, not a stream, because a module that can hold a connection open is a module that can hold every connection open. What the record may carry is bounded by the host and not by the module: `content_type` is a closed vocabulary that does not include text/html, because the host wraps a page and an add-on that could choose the type could choose markup; `location` is answered 302 and never a permanent redirect; and `set_cookie` is bounded by the prefixes the manifest declares, with the host's own Secure, HttpOnly and SameSite attributes applied. Each of those is ErrInvalid rather than a silently corrected response. |
| `template_render`<br>`sdk.TemplateRender(name string, data []byte)` | 0.1.0 | `routes.own_prefix` | declared, refused | TemplateRender renders one of this add-on's own templates through the host's renderer, so a page an add-on draws inherits the product's escaping, its theme tokens and its Content-Security-Policy. It is also how an add-on reaches the page without bringing a front-end toolchain: it renders nothing itself. A host that does not implement it yet answers ErrNotAvailable. |
| `session_context`<br>`sdk.SessionContextRead()` | 0.1.0 | `session.context` | **live** | SessionContextRead asks the host who is signed in on the request this add-on is answering. It is the *read* half of the session boundary and the whole of it: what comes back is an identity and where it is working, never a cookie, a token or a session row, so an add-on can draw a page for the person in front of it and cannot act as them anywhere else. Nobody signed in is not an error — add-on routes are reachable without a session, because a sign-in flow could not otherwise begin — so the record's `signed_in` is false and every other field is empty. Outside a request it is ErrNotFound, which is what a module calling it from package initialization gets. Minting a session is session_mint and is a different grant. |
| `session_mint`<br>`sdk.SessionMint(claim []byte)` | 0.1.0 | `session.mint` | declared, refused | SessionMint tells the host that this add-on authenticated somebody, and asks for a session. The add-on does not make a session and never sees a token: it makes an assertion, the host decides whether an account exists for it and what the session may do, and the cookie is written by the host. That split is what keeps the host, and not an add-on, the authority over who is signed in. What comes back is a MintedSession, and it is enumerated for the same reason the claim is: an answer described only as "a JSON object" is an answer the credential assertion over this surface cannot read. A host that does not implement it yet answers ErrNotAvailable. |
| `redirect_event_read`<br>`sdk.RedirectEventRead()` | 0.1.0 | `redirect.observe` | declared, refused | RedirectEventRead reads the redirect this add-on is observing. What it carries is at most what click_events may carry — prefix-derived and country-level, and no client address in any form. The grant it costs is redirect.observe, which is out-of-band observation and nothing more: running inside the redirect path itself is redirect.inline, a separate declaration no host grants yet, so a module cannot reach the path by holding this. A host that does not implement it yet answers ErrNotAvailable. |

### Permissions

An add-on declares what it needs in its manifest's `permissions` array, and the host refuses a call whose permission is not there — before it refuses one it has not implemented, so a module that declared nothing cannot use the availability status to find out what a host can do. The vocabulary is closed: a token outside it refuses the add-on at load.

| Permission | Grantable | Is |
| --- | --- | --- |
| `config.read` | yes | Read this add-on's own declared settings. It is the narrowest grant here and it is still a grant: the manifest's `settings` list says which keys exist, and this says whether the module may read any of them at all. |
| `storage.own_schema` | yes | Read and write the Postgres schema this add-on owns, whole. The schema boundary is the whole of the grant — there is no row-level or column-level form of it, and nothing here names another add-on's schema or this product's own tables. It does not stop you giving your own schema away: a `GRANT` on what you own works, and the host reports it at your next load and refuses you until it is revoked. Migrations are the host's and are not this grant. |
| `routes.own_prefix` | yes | Serve requests under the path prefix this add-on owns, and render its own templates through the host's renderer. One grant rather than two: a module renders a fragment in order to answer a request, and a template rendered for nobody is not a capability. |
| `session.context` | yes | Ask the host who is signed in: identity, workspace and organization, and nothing else. Its own token rather than a thing a page-serving add-on gets for free, because `routes.own_prefix` is read as *this add-on draws a page* and identity is a second answer — a manifest declaring one grant should not turn out to have declared two. It is the read half and the whole of it: there is no cookie, no token and no session row behind it, and minting or destroying a session is session.mint. |
| `session.mint` | yes | Tell the host that somebody authenticated, and ask for a session. The highest-value grant in this vocabulary: a module holding it decides who is signed in, subject to the host's own judgement about whether an account exists and what the session may do. |
| `redirect.observe` | yes | Observe redirects this instance served, out of band. What crosses is at most what click_events may carry — prefix-derived and country-level, and no client address in any form — so this grant cannot be widened into one by the host implementing it. |
| `redirect.inline` | **no host grants it yet** | Run inside the redirect path itself, where a module's own latency is added to the response. Distinct from redirect.observe so that a module cannot acquire it by accident, and **no host grants it yet**: it is declared here so the milestone that admits an add-on onto that path enforces behaviour against a permission that is already enforced. |

### Statuses

Every function returns one `i32`: a length or zero on success, one of these on failure.

| Status | SDK | Means |
| --- | --- | --- |
| `-1` | `sdk.ErrInternal` | The host failed at something that is not the add-on's fault; it has logged the detail |
| `-2` | `sdk.ErrNotAvailable` | This ABI declares the function and this host does not implement it yet |
| `-3` | `sdk.ErrDenied` | The add-on did not declare this capability, or declared it and may not have it |
| `-4` | `sdk.ErrNotFound` | A well-formed request for something that is not there |
| `-5` | `sdk.ErrInvalid` | The arguments were the add-on's fault: a length outside its memory, text that is not UTF-8, or a value outside the vocabulary |

### Records

A record crosses the boundary as a JSON object.

#### `RedirectEvent`

One redirect this instance served, handed to an observing add-on. Every field is one click_events may carry, which is asserted rather than promised: the test reads the column list out of the migration.

**Bound by `click_events`.** Every field below is one that table may carry, and a test reads the column list out of the migration to prove it.

| Field | Type | Notes |
| --- | --- | --- |
| `link_id` | string | The link, as a UUID |
| `workspace_id` | string | The workspace the link belongs to, as a UUID |
| `occurred_at` | string | RFC 3339, from the host's clock and not the guest's fake one |
| `visitor_hash` | string | The daily-salted visitor hash, hex — irreversible once the day's salt is purged, and not joinable across workspaces |
| `is_first_visit` | boolean | As stored: dormant, and therefore always false |
| `country` | string | ISO 3166-1 alpha-2, and the finest location this ABI carries |
| `device` | string | Device class |
| `browser` | string | Browser family |
| `os` | string | Operating-system family |
| `language` | string | The primary Accept-Language tag |
| `referrer_host` | string | The referrer's host only; the full URL is discarded at the edge |
| `is_bot` | boolean | Whether the request was classified as a bot |

#### `HTTPRequest`

A request that reached one of an add-on's routes. The header set is an allowlist and not a map: every address-bearing header — Forwarded, X-Forwarded-For, X-Real-IP and the CDN spellings beside them — is absent, because handing them over would put a client address across this boundary through a field nobody called an address. Cookies reach an add-on because an authentication flow cannot work without them, and only the ones it declared a prefix for: this product's sessions are server-side and opaque, so the Cookie header is the credential rather than a description of one.

**Cookies are prefix-filtered.** An add-on sees the cookies whose names begin with one of the `cookie_prefixes` its manifest declares, and a declared prefix has to begin with the add-on's own name — so no prefix an add-on may declare reaches a cookie of the host's, and this instance's session cookie is not among the ones it can ask for.

| Field | Type | Notes |
| --- | --- | --- |
| `method` | string | The HTTP method |
| `path` | string | The path within the add-on's own route prefix |
| `query` | string | The raw query string |
| `cookies` | object | The cookies whose names begin with one of the prefixes this add-on's manifest declares, by name — and nothing else, so no prefix an add-on may declare reaches a cookie of the host's |
| `content_type` | string | The request's Content-Type |
| `accept_language` | string | The request's Accept-Language |
| `body` | string | The body, base64 when body_base64 says so |
| `body_base64` | boolean | Whether body is base64 rather than text; it is true exactly when the request's own body was not valid UTF-8, and it exists because a guest cannot otherwise tell an encoded body from one that happens to look encoded |

#### `HTTPResponse`

What an add-on answers a request with, and what the host will let it. `content_type` is a closed vocabulary — text/plain and application/json — and **text/html is deliberately absent**: leaving it empty is the ordinary case and means the host wraps the body in the dashboard's own page, escaped, which is what makes "an add-on cannot inject markup" a property of this record rather than of a filter somewhere. Every bound here is checked when the record is written, so a module learns it was refused from the call it made rather than from a page that differs from what it asked for.

| Field | Type | Notes |
| --- | --- | --- |
| `status` | number | The HTTP status code |
| `content_type` | string | The response's Content-Type |
| `location` | string | For a redirect; never a permanent one, which the host enforces rather than trusts |
| `set_cookie` | array | Cookies to set, bounded by the same prefixes the manifest declares — a namespace an add-on owns is one it owns in both directions, or it could overwrite a cookie it is not allowed to read; the host applies its own Secure, HttpOnly and SameSite attributes |
| `body` | string | The body, as UTF-8 text — this direction carries no encoded form, because the content types an add-on may name are text and a flag saying otherwise would be a flag with nothing behind it |

#### `SessionContext`

Who is signed in on the request an add-on is answering, and where they are working. It is deliberately not the session: no cookie, no token and no session identifier crosses, because this product's sessions are opaque server-side rows and the cookie *is* the credential (D232) — an add-on handed one could act as whoever is signed in, without escaping the sandbox. What is here is what a page needs in order to be drawn for somebody: who they are, and which tenant and workspace their request landed in. Every field is empty and `signed_in` is false when nobody is, which is the ordinary state of a route that begins an authentication flow.

| Field | Type | Notes |
| --- | --- | --- |
| `signed_in` | boolean | Whether anybody is signed in at all; false makes every field below empty |
| `user_id` | string | The account, as a UUID — stable, and the only identifier of a person this record carries |
| `email` | string | The account's email address |
| `display_name` | string | The person's name, for display |
| `workspace_id` | string | The workspace this request landed in, as a UUID |
| `organization_id` | string | The organization that workspace belongs to, as a UUID — the tenant, which a workspace is not |
| `role` | string | The role held in that organization: owner, admin, editor or viewer |

#### `SessionClaim`

An add-on's assertion that somebody authenticated. It is a claim and not a session: the host decides whether an account exists for this subject, what role it holds and how long the session lives.

| Field | Type | Notes |
| --- | --- | --- |
| `subject` | string | The identity provider's stable identifier for the person |
| `issuer` | string | Which provider asserted it |
| `email` | string | The person's email address, as the provider gave it |
| `email_verified` | boolean | Whether the provider says it verified that address |
| `display_name` | string | The person's name, for display |
| `groups` | array | Provider groups, for whatever mapping M65 decides on |

#### `MintedSession`

What the host hands back when it accepted a claim and minted a session. It is deliberately not the session: no token, no cookie and no row of the sessions table crosses, because the host writes the cookie itself and an add-on able to read one would be able to replay it. What is here is what an add-on's own response depends on, and every field traces to a decision m65.md already states is the host's; a field M65 finds it needs is additive.

| Field | Type | Notes |
| --- | --- | --- |
| `expires_at` | string | RFC 3339, when this session stops being one — how long a session lives is the host's decision and not the claim's |
| `second_factor_required` | boolean | Whether the person still owes a second factor: an account with TOTP enrolled meets it after an add-on's assertion rather than instead of it, so this is what an add-on has to read before it decides the page it sends them to |

<!-- END GENERATED -->
