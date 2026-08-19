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

## What counts as breaking

The point of writing this down is that *is this minor or major* stops being a
mood. Work through the table; the first row that matches decides.

| Change | It is |
| --- | --- |
| Changing the parameters of a function **no released version of this product implements** — one still declared and refused everywhere — or a field of a record only such functions carry | **neither — no version moves.** The case is spelled out [below](#a-function-nothing-implements-has-no-signature-to-break), because it is the one this table could not decide |
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
  add-ons can neither read nor deny each other's cookies, since the namespace
  comes from the name rather than from whoever installed first.

## The ABI

<!-- BEGIN GENERATED: the function table -->

The ABI is **0.1.0**, generation **1**, and this host loads generation 1 or newer.

| Function | Since | Status | What it is |
| --- | --- | --- | --- |
| `abi_version`<br>`sdk.HostABIVersion()` | 0.1.0 | **live** | HostABIVersion is the ABI version of the host this module is running in. A module's manifest declares the generation it was built against and the host refuses a mismatch before instantiation, so this is not how a module checks compatibility — it is how one logs what it is talking to, and how it decides whether a function added in a later patch is worth probing for. |
| `log`<br>`sdk.Log(level string, message string)` | 0.1.0 | **live** | Log writes one line to the host's logger, attributed to this add-on. It is the only way out: a module's stdout and stderr are discarded, because routing them into an operator's log is a capability and the host grants none it was not asked for. The host adds the add-on's name; a message that repeats it is noise. An unknown level is ErrInvalid rather than a silent default, so a typo does not become a line nobody greps for. |
| `config_get`<br>`sdk.ConfigGet(key string)` | 0.1.0 | **live** | ConfigGet reads one of this add-on's own settings. The key must be one the add-on's manifest declares; anything else is ErrDenied, which is what scopes the function to the add-on rather than to the instance — there is no way to ask for another add-on's setting or for one of this product's own configuration values. A declared setting with no value yet answers with the default the manifest gave it, and ErrNotFound only when it declared none. Values are edited in the Add-on manager; until a host implements that, every answer is a declared default. |
| `storage_query`<br>`sdk.StorageQuery(sql string, args []byte)` | 0.1.0 | declared, refused | StorageQuery runs a read against the Postgres schema this add-on owns. The schema boundary is the whole of the permission: an add-on names no database, no connection and no search_path, and a statement that reaches outside its own schema is refused rather than executed. A host that does not implement it yet answers ErrNotAvailable. |
| `storage_exec`<br>`sdk.StorageExec(sql string, args []byte)` | 0.1.0 | declared, refused | StorageExec runs a write against the Postgres schema this add-on owns. Migrations are not this function: the host runs an add-on's migrations, which is what keeps *DDL is additive within a minor version* a promise somebody can keep. A host that does not implement it yet answers ErrNotAvailable. |
| `http_request_read`<br>`sdk.HTTPRequestRead()` | 0.1.0 | declared, refused | HTTPRequestRead reads the request that reached one of this add-on's routes. It answers ErrNotFound outside a request, which is what a module calling it from package initialization gets. A host that does not implement it yet answers ErrNotAvailable. |
| `http_response_write`<br>`sdk.HTTPResponseWrite(response []byte)` | 0.1.0 | declared, refused | HTTPResponseWrite answers the request that reached one of this add-on's routes. Called twice for one request it is ErrInvalid: a response is one record, not a stream, because a module that can hold a connection open is a module that can hold every connection open. A host that does not implement it yet answers ErrNotAvailable. |
| `template_render`<br>`sdk.TemplateRender(name string, data []byte)` | 0.1.0 | declared, refused | TemplateRender renders one of this add-on's own templates through the host's renderer, so a page an add-on draws inherits the product's escaping, its theme tokens and its Content-Security-Policy. It is also how an add-on reaches the page without bringing a front-end toolchain: it renders nothing itself. A host that does not implement it yet answers ErrNotAvailable. |
| `session_mint`<br>`sdk.SessionMint(claim []byte)` | 0.1.0 | declared, refused | SessionMint tells the host that this add-on authenticated somebody, and asks for a session. The add-on does not make a session and never sees a token: it makes an assertion, the host decides whether an account exists for it and what the session may do, and the cookie is written by the host. That split is what keeps the host, and not an add-on, the authority over who is signed in. What comes back is a MintedSession, and it is enumerated for the same reason the claim is: an answer described only as "a JSON object" is an answer the credential assertion over this surface cannot read. A host that does not implement it yet answers ErrNotAvailable. |
| `redirect_event_read`<br>`sdk.RedirectEventRead()` | 0.1.0 | declared, refused | RedirectEventRead reads the redirect this add-on is observing. What it carries is at most what click_events may carry — prefix-derived and country-level, and no client address in any form. Which declaration class an add-on must hold to reach it is the host's to state. A host that does not implement it yet answers ErrNotAvailable. |

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
| `body` | string | The body, base64 when it is not UTF-8 |

#### `HTTPResponse`

What an add-on answers a request with.

| Field | Type | Notes |
| --- | --- | --- |
| `status` | number | The HTTP status code |
| `content_type` | string | The response's Content-Type |
| `location` | string | For a redirect; never a permanent one, which the host enforces rather than trusts |
| `set_cookie` | array | Cookies to set, bounded by the same prefixes the manifest declares — a namespace an add-on owns is one it owns in both directions, or it could overwrite a cookie it is not allowed to read; the host applies its own Secure, HttpOnly and SameSite attributes |
| `body` | string | The body, base64 when it is not UTF-8 |

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
