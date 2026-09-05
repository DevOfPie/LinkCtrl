# Changelog

Notable changes, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

Two things are versioned separately, and the distinction matters when deciding
whether an upgrade is safe:

- **The REST API is `/api/v1`** and is a stable contract. A breaking change there
  becomes `/api/v2`, not a major version bump here.
- **The product** is pre-1.0 while account lifecycle and identity are incomplete.
  SSO is a later phase, and a dashboard redesign is under way. Three entries left
  this list at 0.3.0 because they were built: account recovery — a forgotten
  password is recoverable by the person who forgot it, on an instance with a
  mailer — account deletion with subject erasure, and two-factor authentication.
  Each of the rest
  moves the product surface, so the version stays in the `0.x` range until they
  have settled. `0.x` here means "the product surface may still move", not
  "unfinished": everything documented as built is tested and exercised end to end.
  *(This read "pre-1.0 while Phase 2 is outstanding. Shared workspaces, folders
  and custom domains will change the dashboard and add tables" until 0.2.0 — all
  three shipped in it, so the sentence named its own contents as future work.)*

The database schema only ever changes additively within a minor version, and
migrations run at boot.

## [Unreleased]

### Fixed

- **Purging an add-on's leftover data no longer offers a still-installed add-on's
  data for deletion.** An add-on that is on disk but did not start — a module
  that fails to load, or a manifest that stops validating while the tables it
  created survive — was reported as an orphan, and the manager's confirmation
  said it had been uninstalled. Following that confirmation dropped a schema the
  add-on was going to need at its next successful start. The instance now
  distinguishes *installed but not running* from *not installed*, everywhere it
  says so: at boot, in the manager's list, and in the confirmation.

  Loss was bounded and is worth stating for anyone who hit it: the add-on's role,
  its account links and its saved settings survived, and its migrations re-run at
  the next successful start — so the add-on came back structurally intact with
  empty tables.

- **A purge can no longer delete the schema of an add-on that was installed a
  moment earlier.** The check and the deletion did not hold the lock the install
  path holds, so an install landing between them returned success and had its new
  schema dropped underneath it. This needed no unusual timing to reach: a script
  that installs and then purges stale orphans is enough.

- **An add-on install that fails no longer keeps its compiled module in memory
  for the life of the process.** About 10 MB per failed attempt, invisible and
  irreversible short of a restart, which is felt most in the loop where it is
  least welcome: rebuilding a module that will not start.

- **Installing an add-on from a URL asks the origin not to compress it.** An
  origin serving the bundle with `Content-Encoding: gzip` delivered it inflated,
  so the SHA-256 you typed was compared against bytes that were not the ones on
  the release page — reported as a digest mismatch, sending you to check a digest
  that was correct.

- **An add-on whose outbound-origin setting holds a malformed entry warns once
  when it loads, rather than once per request.** The line is worth having and was
  reachable at whatever rate a module chose to call.

### Changed

- **A sign-in label may hold letters, marks, numbers, punctuation, symbols and
  spaces, and nothing else.** It is drawn on the sign-in page an unauthenticated
  visitor is asked to trust, and the previous rule refused line breaks while
  admitting the right-to-left override and zero-width characters — so a label
  could read as something other than what it said. **A manifest that validated
  before may now be refused**; the refusal names the character.

- **An instance with add-ons enabled refuses to start if `HTTP_REQUEST_TIMEOUT`
  is 10 seconds or under.** Installing an add-on from a URL spends up to ten
  seconds fetching, inside that request — so under it the fetch bound never
  fires and the install finishes hashing, unpacking and compiling under a context
  that has already been cancelled. The default of 15s is unaffected.

- **An add-on's redirect may not carry a backslash in its location.** Some
  browsers read one as a path separator and follow it to another origin, which
  the neighbouring check in this product's own sign-in flow has always refused.


- **Some dashboard controls say what they do, and some paragraphs stopped saying
  it for them.** Deleting an add-on's leftover data is a trash-can control on the
  row rather than a button labelled *Purge*, which is the word the confirmation
  page never used. Replacing your recovery codes is *Replace my recovery codes*
  rather than *Issue new recovery codes* followed by a sentence explaining that
  the old ones stop. The bare *Change* beside a domain now says *Rename*, and the
  one beside a member's role says *Change role*.

  **Large objects an add-on's database role owns are a row of their own**, listed
  with the delete control disabled, instead of a footnote under a control that
  could not delete them. Dropping the schema does not remove them, and a dead
  control in the place you look says that better than a sentence somewhere else
  did.

  Nothing about what any of these operations do has changed.

### Added

- **A sign-in page that can offer what an installed add-on made possible.**

  An authentication add-on could sign somebody in and serve its own pages, and
  there was still no way to *start* the flow except by being handed a URL. Now an
  add-on may ask for a link on this server's sign-in page, and you decide whether
  it appears.

  **Asking is the manifest's; agreeing is yours.** The add-on declares two fields
  — the words to draw, and which of its own pages the link should reach. Nothing
  appears until you turn on **`sign_in_link`** on that add-on's page in the Add-on
  manager, and it is off until you do. A new version of an add-on cannot change
  what your visitors see, and an add-on cannot declare a setting by that name to
  answer for you.

  **The link's destination is this server's.** The manifest names a page inside
  the prefix the add-on already has — never a host, never a scheme, never a path
  that climbs out — and the address is composed here and checked afterwards
  against that same prefix. The words are the add-on's and are escaped like every
  other value on every page; the icon, the colour, the position and the order when
  two add-ons offer are not an add-on's to decide.

  **Nothing changes for an instance that runs no add-ons.** The sign-in page is
  byte for byte the page it was, and the password form does not move on any
  instance: an add-on's link is drawn below it, never in place of it, so an
  instance whose add-on is broken still lets its operator in. A link is drawn from
  a module this server actually **loaded** — a failed add-on offers nothing rather
  than a link that 404s.

- **OIDC sign-in, as a first-party add-on rather than as a feature of this
  server.**

  [`DevOfPie/LinkCtrl-OIDC`](https://github.com/DevOfPie/LinkCtrl-OIDC) is an
  OpenID Connect relying party that installs into an instance the way any other
  add-on does: discovery, an authorization-code flow with PKCE, a token exchange,
  an ID token verified against the provider's key set, and an assertion this
  server acts on. It is published separately, it consumes only this project's
  published SDK, and nothing about it is compiled into this server — which is the
  point. If it could not be built against the add-on interface, the interface was
  wrong.

  **Connecting comes before signing in.** An assertion about an external identity
  nobody has connected signs nobody in. You sign in with a password, visit the
  add-on's linking page, and from then on that provider identity reaches your
  account. There is deliberately no matching on the email address an assertion
  carries. Every session minted this way is recorded as
  `session.minted_by_addon`, naming the add-on and the provider, in the audit log
  of the organization the session resolved to rather than the instance-wide one.

  **What running it costs you** is in
  [docs/configuration.md](docs/configuration.md) — the two parties you trust, what
  happens when the provider is down, and the fact that a provider on a private
  network address cannot be reached at all, because this server dials globally
  routable space and nothing else.

  **Its first release is `v0.1.0`**, and it is verifiable without trusting the
  page you found it on: the release publishes a `SHA256SUMS` for the bundle you
  hand the Add-on manager, the manifest inside names the module's own digest, and
  a build provenance attestation over that digest says which workflow, tag and
  commit produced it. This server's acceptance test installs that artifact and
  nothing else.

  **The public demo does not run it**, deliberately: there is no identity provider
  behind the demo and a sign-in flow against a throwaway one shows nothing you
  could use.

- **A module can arrive from a URL, with a digest you supply.**

  Installing an add-on no longer means having its files on the machine you are
  sitting at. The install control on the Add-on manager, and
  `POST /api/v1/addons`, now take a **bundle URL** and the **`sha256` that
  bundle must hash to** as an alternative to the two file parts. Uploading still
  works exactly as it did, as does placing a directory in
  `LINKCTRL_ADDONS_DIR` and restarting; all three produce the same add-on, and
  from the digest check onward they are one code path.

  A bundle is a **`.tar`, a `.tar.gz` or a `.zip`** holding `addon.json` and the
  module it names, and nothing else. One object, because a manifest and a module
  fetched separately could come from two different moments — and because it is
  what makes the next sentence structural rather than a promise.

  Ship whichever container your release pipeline already emits. **The file's
  name plays no part**: what a bundle is comes from its leading bytes, so a
  `.tar.gz` that is really a zip installs and a `.tar` that is really an error
  page is refused. All three carry the same rule about what may be inside —
  **exactly two plain files with plain names**, so no directory, no symbolic
  link, no path and no repeated name — and a compressed bundle is bounded a
  second time on what it may amount to once opened, at the smaller of 32 MiB and
  fifty times what was fetched. That figure is where decompression **stops**, so
  a container expanding by more than a module plausibly does is refused
  part-unpacked rather than unpacked and then declined, and a small archive is
  never refused for a ratio that is really its padding.

  **The digest is yours and never the URL's.** It covers the whole bundle, it is
  checked before an archive reader or a JSON parser is pointed at the bytes, and
  nothing is written unless it matches. A checksum published beside a module
  proves nothing: whoever can serve the one can serve the other. What this
  bounds is that the bundle is the one you meant — it cannot make a digest you
  copied off the same page mean anything, and the install form says so where you
  are about to paste them. Publisher identity, in a module store, is what would
  answer that; this is the foundation it goes on top of.

  **Where the fetch can reach is not configurable.** `https` only. Addresses are
  checked after the name resolves, on every address it resolves to and on every
  hop, against the same globally-routable-unicast policy an add-on's own fetch
  meets — so a URL install cannot be used to make your server probe loopback,
  link-local, a cloud metadata service or a private range. A redirect that
  leaves the origin you typed is not followed. Ten seconds for the whole
  transfer, 32 MiB at most: a large module over a slow link should be uploaded
  instead, where the bytes travel on your own request.

  **A refusal says which bound it hit** — a bad address, a redirect off the
  origin, a status that was not `200`, a digest that did not match, an archive
  that is not a bundle, an archive that unpacks to too much — rather than
  telling you to check a digest that is fine.

  This needs `addons.manage`, like every other add-on lifecycle operation, and
  is in the instance-wide audit log the same way; the server log adds one line
  naming the origin the module came from. `docs/SECURITY.md` states the trade in
  full: you are trusting the URL's host to serve the bytes your digest names,
  and the digest is what makes that a bounded trust rather than an unbounded one.

- **An add-on can reach outward, and only where the operator pointed it.**

  Until now nothing an add-on could import touched the network, which meant the
  kind of add-on this host was built to run — one that signs people in through an
  identity provider — could not be written at all. It can now, through one new
  host function and one new permission, `network.fetch`.

  **The manifest declares a need and never a destination.** An add-on marks one of
  its settings as carrying origins; the operator fills that in. A manifest naming
  a host anywhere — a default value, a list of options, a URL inside a permission
  token — is refused at load. So an add-on's author cannot widen its own reach, and
  the only person who decides where this server connects is the person running it.
  The permission vocabulary is nine tokens as a result.

  One thing to know when you *upgrade* one: a new version cannot name a
  destination, but it can mark a setting you already filled in as one that names an
  origin — a `homepage` you typed into v1 becoming an origin v2 dials. Installing a
  version costs `addons.manage` either way, so nothing is escalated, but
  `docs/SECURITY.md` says to read an add-on's origin settings alongside its
  permissions whenever an upgrade adds `network.fetch`.

  **Absent configuration it reaches nothing.** An add-on holding the permission
  with no origin named answers `unconfigured` to every call and opens no socket.
  That is the ordinary state of one that has just been installed, and it is stated
  rather than papered over: an add-on that talks outward does not work until it is
  configured.

  **What the host enforces is not negotiable by either party.** https only; `GET`
  or a form-encoded `POST`; **no request header the add-on chose**, so a
  credential or a `Host` override cannot be put on the wire; no response header
  back but the content type; every address the name resolves to checked at the
  moment of dialling; no redirect followed off the origin it started on; a response
  size cap **on the headers as well as the body**, the second fixed at 64 KiB
  because no provider needs it raised and Go's own default is forty times the body
  cap; a request timeout; and no connection kept alive between invocations.

  **The address check is an allowlist, and it is worth knowing which way round it
  is.** An address is dialled only if it falls in globally-routable unicast space,
  and everything else is refused — loopback, link-local (the cloud metadata service
  above all), unique-local and the private ranges among them, but also any range
  nobody has thought about, however the name got there and whatever it answered
  last time. Written the other way round it would have been a list of everywhere
  that is not the public internet, which is not a list anybody holds in their head.
  The cost is real and it is deliberate: this will eventually refuse an origin that
  was perfectly legitimate — IPv6 space allocated after this release is the case to
  expect — and the symptom is an add-on reporting that a name will not resolve. So
  every refusal writes one log line naming the address and the rule that refused
  it. **If an origin you named is not reachable, grep for `address_rule=`.**

  **Where you watch this is the counter, not the log.** An add-on nobody has
  configured, one pointed at an origin you did not name, and one calling from an
  invocation that may not fetch are all things a module produces as fast as it
  likes on a page anybody can reach — so those three write at `debug` and
  `linkctrl_addon_fetch_total{addon,outcome}` is what carries them, beside the
  Add-on manager's own breakdown of the same words. The cost is stated: an
  operator watching only the log sees nothing when an add-on is inert. An address
  refusal keeps its warning, because `address_rule=` names something no counter
  can.

  **Nothing on the redirect path may fetch.** Both redirect classes are refused,
  whatever the manifest declared: an inline module holds a visitor's request open
  against a deadline in milliseconds and an observing one has no caller whose
  budget a network round trip could be spent against. A fetch is callable from an
  add-on's own page handler and from nowhere else.

  **A page an add-on serves has a bound of its own.** `LINKCTRL_ADDON_ROUTE_DEADLINE`,
  ten seconds by default, covers loading the module, running its handler and every
  host call inside it. Before it, a route was bounded only by
  `LINKCTRL_HTTP_REQUEST_TIMEOUT` — the same fifteen seconds every request gets —
  which kills the module and this server's ability to answer at the same instant,
  and which an operator may have set to zero. Ten leaves five seconds to turn a
  killed module into a page you can read and a counter you can alert on. Until it
  elapses, a module that will not return holds one of the sixteen instance slots
  for as long as the visitor waits, and those slots are shared with the redirect
  path.

  **Two nesting rules are now enforced at start-up**, so a bound that cannot fire
  is refused rather than shipped: `LINKCTRL_ADDON_ROUTE_DEADLINE` must be under
  `LINKCTRL_HTTP_REQUEST_TIMEOUT`, and `LINKCTRL_ADDON_FETCH_TIMEOUT` must not
  exceed the route deadline. If you have raised either, raise the one above it too
  or the instance will tell you which line to change.

  **This is the one upgrade break in this release, and it is narrow.** If you run
  add-ons — `LINKCTRL_ADDONS_DIR` is set — **and** you have set
  `LINKCTRL_HTTP_REQUEST_TIMEOUT` to `10s` or less, this instance will not start
  until `LINKCTRL_ADDON_ROUTE_DEADLINE` comes down below it. Both values were
  accepted before, because the route deadline did not exist; the pair is refused
  now because a route deadline at or over the request timeout never fires, and a
  bound that cannot fire is worse than no bound. The remedy is to lower
  `LINKCTRL_ADDON_ROUTE_DEADLINE`, not to raise a request timeout you set for a
  reason. An instance with `LINKCTRL_ADDONS_DIR` unset is not held to the rule and
  starts exactly as it did.

  **You can see it happening.** `linkctrl_addon_fetch_total{addon,outcome}` counts
  every attempt and every refusal by add-on, with a closed eleven-word vocabulary
  the add-on itself branches on, and `linkctrl_addon_fetch_duration_seconds{addon}`
  times the ones this instance actually attempted — a refusal it decided itself is
  counted and not timed, so a blocked address does not show up as latency. The Add-on manager renders both beside
  the redirect figures, with what each refusal means for you rather than for the
  add-on's author.

  New variables: `LINKCTRL_ADDON_ROUTE_DEADLINE`, `LINKCTRL_ADDON_FETCH_TIMEOUT`,
  `LINKCTRL_ADDON_FETCH_MAX_BYTES`. **`fetch` and `route` join the reserved add-on
  names** for the reason `pool` did: those three variables live in the same
  `LINKCTRL_ADDON_<NAME>_<X>` namespace as an add-on's own settings, so an add-on
  in a directory called `fetch` or `route` would make one variable mean two
  things. Such an add-on stops loading when you upgrade, with the reason and the
  reserved list on stderr; rename the directory. `docs/SECURITY.md` carries the
  disclosure — this is the sixth connection that leaves this product and the first
  whose destination somebody outside this project chose.

- **The Add-on manager: one page for what this instance runs.**

  `/instance/addons`, in the identity menu, behind `addons.manage` — so on an
  instance that runs no add-ons, and for every account but the one that
  administers the box, it is not there at all.

  **The list is what is installed**, with each module's name, version,
  declaration class (`none`, `redirect-observe`, `redirect-inline`), failure
  class and the permissions its manifest *declares* — every one of them, with any
  this host grants to nobody struck through, because a permission that is
  declarable and not held is a thing to see rather than a thing to omit.

  **Per-module performance is on the page as numbers**, not as a link to
  `/metrics`: each module's own p99 on the redirect path and how many of its
  invocations the host stopped waiting for. That is the answer to *which add-on
  is slowing my redirects* on an instance that scrapes nothing. Two things about
  the figures are stated on the page and worth repeating here: they are
  cumulative since the process started rather than a rate over a window, and a
  module that has never run on the redirect path shows a dash rather than a
  zero — no observations and *fast* are different facts.

  **Install and remove are driven from the page**, through the same API and the
  same permission, with nothing private behind it. Removal is select-mode: press
  *Remove…*, each row's chevron becomes a checkbox in the same column, and one
  confirmation covers however many were ticked. That confirmation carries a
  purge choice per module — **unticked**, always — and states what removing a
  `required`-class module costs before it is removed.

  **A detail page behind each row** carries the module's own latency broken down
  by class, its declared settings, the permissions it declared — struck through
  where this host withholds one — and the schema it owns with the size that schema
  is *right now*.

  **An add-on's declared settings are configurable from that page.** A manifest
  declares settings with a type each — text, secret, select, toggle — and the
  host renders the matching input, saves the value into a table of its own, and
  hands it to the module through `config_get` on its next invocation — including to
  an instance the host had already built, so a module that reads its settings at
  start-up sees the new value without a restart. A secret is never shown again
  after it is saved: the field says *set* or *not set*, and clearing one is its
  own deliberate act. **That withholding is against the form and the API, and it
  holds against the stored value's own record** rather than against the manifest in
  front of you — so a *replacement* add-on installed under the same name cannot get
  its predecessor's credential rendered back into a page by re-declaring the setting
  as plain text. It is **not** a bound on the credential: settings are keyed on the
  add-on's name, so that replacement still reads the value through `config_get`,
  the same way it inherits the identity mappings written under the name. Removing
  an add-on deletes neither, and neither does a purge. **A setting the deployment's own
  environment answers is not editable here** — `LINKCTRL_ADDON_<NAME>_<SETTING>`
  wins, and the page names the variable to edit instead of offering a control
  whose write nothing would read. Every save is in the instance-wide audit log as
  `addon.settings_saved`, naming which settings the save wrote and never their
  values.

  The add-on ABI moves to **0.1.3** for it. `config_get` gains a source and
  changes nothing else — no parameter, no status, and the environment still
  outranks everything — which is a shape
  [the deprecation policy](docs/addon-abi.md#an-answer-that-gains-a-source) did
  not have a row for until this release and now calls additive. Generation `1` is
  unmoved and nothing new is importable, so an add-on built against `0.1.2` needs
  no rebuild. What the policy fixes alongside it is the part a publisher needs:
  a setting is read afresh for each invocation and is stable within one.

  **Orphaned data is named, and the page is where the offer meets the act.**
  Removing an add-on never deletes its data, so every `addon_*` schema with no
  installed module is listed with its size and the add-on it belonged to, each
  offering its own purge behind a confirmation. A purge is `DROP SCHEMA …
  CASCADE` and it is audited as `addon.data_purged` carrying how much went — after
  the drop there is nothing left to measure, so the audit row is where that figure
  keeps. The API's purge response and the server log carry it at the time — and a
  size the catalogue could not measure is left out of the audit row rather than
  written as `0`, which is what an empty schema honestly measures. Four things
  deliberately survive a purge and the confirmation says so: the `addon_<name>`
  database login role, so re-installing under that name works as it did; any large
  objects that role owns, which live outside every schema; any external-identity
  links written under that name; and any settings saved under it. The confirmation
  counts the last two, because both are keyed on the *name*, so whatever is
  installed under it next inherits the account mappings **and** the configured
  values — a stored secret included. Nothing in this release deletes the settings
  of an add-on that is no longer installed: a settings write is refused for a name
  that is not loaded, so `psql` is the route.

  **Everything on the page has an API**, under the same `addons.manage`, beside
  the install and remove below: `GET /api/v1/addons` lists what is installed with
  each module's class, declared permissions and redirect-path figures, `GET
  /api/v1/addons/{name}` is one module's detail, `PUT
  /api/v1/addons/{name}/settings` saves its declared settings, `GET
  /api/v1/addons/orphaned-data` lists the schemas no installed module owns, and
  `DELETE /api/v1/addons/orphaned-data/{name}` purges one — answering `200` with
  the row that went, because after the drop there is nothing left to measure.

  **The demo instance runs one.** A first-party `redirect-observe` sample,
  `pageviews`, is built into every image and switched on only where
  `LINKCTRL_ADDONS_DIR` points at it — so the manager has something real to show
  and nobody else pays for it. Its source is in `examples/addons/`.

- **An add-on can be installed and removed while the instance is serving.**

  Two operations under one new permission, `addons.manage`, held only by the
  account that administers the instance and held by no API key at all — a key
  that could install an add-on would carry whatever that add-on's own manifest
  declares, which is a reach nothing about the key bounds.

  **A module is uploaded.** `POST /api/v1/addons` takes a `multipart/form-data`
  body carrying the `.wasm` and the `addon.json` that describes it, at most
  32 MiB together. The manifest is parsed and the module is checked against the
  manifest's digest **before anything is written to disk**, so bytes that are not
  the bytes the manifest describes never reach the directory this instance
  executes from. *(This paragraph said there is no field naming a URL and there
  will not be one; the entry below adds one, and neither has been released.)* An install spends
  `LINKCTRL_UPLOAD_RATE_PER_MIN` — thirty a minute per address, on top of the API
  limit, and **the same bucket a QR code's logo upload spends**. Removal carries
  no body and is not charged.

  **The files go into the add-ons directory you already configured**, which is
  the same directory an add-on placed by hand loads from, so there is one answer
  to what is installed. Two things follow and are worth knowing before you rely
  on this: an install reaches **the replica that served the request** and no
  other, and a container filesystem that is not a volume loses it on the next
  deploy. `LINKCTRL_ADDONS_DIR` mounted read-only — which is what
  [configuration.md](docs/configuration.md) recommends, and still the right
  choice for an instance whose add-ons are placed by hand — refuses an install
  with a `503` that says so.

  **Removal unloads without a restart.** `DELETE /api/v1/addons/{name}` takes the
  add-on out of the running set, out of the directory, and then releases what it
  held: its pooled instances, its compiled module and its database connections.
  Invocations already inside the module finish; ones that have not finished in
  five seconds are interrupted, and the answer says so. Because the directory is
  gone before anything is unloaded, removing an add-on whose failure class is
  `required` **cannot leave an instance that will not start**.

  **The add-on's data stays.** An add-on that owned a Postgres schema leaves it
  behind, and the removal's answer names the schema — nothing here deletes it.

  Both acts are recorded in the **instance-wide** audit log, `addon.installed`
  and `addon.removed`, each naming the module, its version and its digest, and
  each readable at `/api/v1/instance/audit`.

  **What an upload cannot install:** an add-on whose manifest declares `.sql`
  migration files ships those files alongside its module, and they are not part
  of the upload. Such an add-on is refused with a message saying so — on the API
  and, since 0.4.0, in the Add-on manager's own words rather than as a general
  *the manifest did not check out* — and is installed the way add-ons have always
  been installed: its directory placed in `LINKCTRL_ADDONS_DIR`, and a restart. There is also no upgrade-in-place:
  replacing an add-on is a removal and an install. And a name that overlaps one
  the directory already claims is refused — `oidc_x` beside `oidc`, in either
  order — **whether or not the other one is running**, because the two share a
  cookie prefix and a settings prefix and neither would load at the next start.
  The install check and the boot check read the same set, so an install cannot
  arrange a start that refuses both.

- **An add-on can run inside the redirect path, and the published redirect
  measurement is now scoped to core.**

  Two classes, declared separately so a module cannot acquire the sharper one by
  accident. `redirect.observe` watches redirects **out of band** — it is fed from
  the click pipeline after the visitor has been answered and after the click is
  durable, so nothing it does can delay or fail a redirect. `redirect.inline`
  runs **on the path**, at one point: after this instance has decided where the
  visitor goes and before the gates that spend a link's budget. An inline module
  may let the redirect stand or veto it, and a veto is answered with the same
  refusal page a blocked bot gets — naming no alias, no destination and no
  add-on. **It is a refusal your visitors will meet, so it is tellable apart from
  every other one**: `linkctrl_redirects_total{outcome="vetoed"}` is its own series,
  zero forever until an inline add-on is installed, and the add-on that decided it
  is named in a log line at info. Like the other gate refusals it records **no
  click**, so a link whose traffic an add-on is refusing shows the drop in its own
  analytics rather than only in a scrape.

  A third grant, `redirect.rewrite_query`, lets an inline module alter the
  destination's **query string and nothing else about it**. It is a token of its
  own on top of `redirect.inline`, because a manifest declaring *run on the
  redirect path* should not turn out to have declared *and edit where the visitor
  goes*. The bound is structural rather than checked: the module writes a query
  and LinkCtrl substitutes it into the URL **it** decided, so the scheme, the
  host, the port and the path cannot move and no tier of the destination
  validator can reach a different verdict. Stripping `fbclid` or `utm_*` from an
  outbound link is what the power exists for.

  **The published redirect latency is now stated as core, with no inline add-on
  on the path** — in [docs/slo.md](docs/slo.md) and
  [docs/SECURITY.md](docs/SECURITY.md), both edited in the same change that made
  an inline add-on possible. That is the boundary rather than a retreat: an
  add-on's own latency is the add-on's, and **availability stays LinkCtrl's**. An
  invocation is bounded twice, once per party: the module's own code by
  `LINKCTRL_ADDON_INLINE_DEADLINE` (25 ms by default, measured rather than
  chosen), and LinkCtrl's own work *starting* the module by
  `LINKCTRL_ADDON_INSTANTIATE_DEADLINE` (500 ms). The runtime kills whichever is
  overrun, the redirect completes without it, and the kill is counted per module
  and per step on `linkctrl_addon_redirect_kills_total{addon,step}` — `call` is an
  add-on to go and fix, `instantiate` is an instance that could not start one, and
  they are different problems with different fixes. A module that fails, is killed
  or cannot be given an instance always means *allow, unchanged* — never a refusal
  — because a bug in an add-on must not be able to take somebody's links down.

  Both halves are measured. Against 2,000 rps for two minutes on 100k links and
  5M click events, core is unmoved at **100% of requests under 20 ms**, and the
  same run with a module that never returns served **every one of 239,932
  redirects with zero failures** while 32,866 invocations were killed. That second
  run still reads **99.83% of redirects under 20 ms server-side**, because the
  figure is core's own work and the time a module held the request is not in it.
  Not one invocation in that run failed to *start* inside its 500 ms bound, with
  all sixteen instance slots held throughout. Both runs are in
  [docs/slo.md](docs/slo.md) with what each one does and does not show.

  **A third run measured the case anybody would actually deploy — an add-on that
  behaves — and the answer was bad enough to change the design.** A module that
  reads its decision and allows the redirect cost the visitor **44.89 ms at p99**
  against a 20 ms target, and none of it was the add-on: **11.05 ms of every
  invocation** was LinkCtrl allocating the module's memory, running its startup
  code and destroying all of it, once per redirect. Two fifths of redirects
  skipped the add-on entirely because every instance slot was busy doing that.

  **So an add-on's instance is now kept and reused rather than built per
  redirect.** The same run reads **1.08 ms at p99**, an invocation costs **451 µs**,
  **9 redirects of 240,001** skipped the add-on instead of 92,546, and no
  invocation was killed at all. Core's own histogram went from 99.996% to **100%**
  under 20 ms.

  **Reuse does not weaken the isolation that came from destroying the instance.**
  A reused instance still holds the guest's own memory, so before one is handed to
  the next redirect LinkCtrl writes back the copy of that memory it took when the
  module started: a package-level variable an add-on wrote during one redirect is
  empty on the next, and a test drives two redirects through one instance to say
  so. An invocation that was killed or trapped is closed rather than reused, so a
  module being killed on every invocation degrades to an instance each — the old
  behaviour — instead of filling the pool with dead ones. Add-on **pages** are not
  pooled and still get an instance per request. What it costs is memory held while
  nothing is running: `LINKCTRL_ADDON_POOL_SIZE` (8) bounds how many instances are
  kept, `LINKCTRL_ADDON_POOL_TTL` (1m) how long an unused one is kept for, and
  `pool` joins the reserved add-on names for the reason the two below do. **It also
  costs a second copy** — the image an instance is reset to is held beside it,
  under the same per-instance cap and outside the guest ceiling — so
  [docs/deployment.md](docs/deployment.md) now sizes a host by that ceiling twice. Neither
  variable changes how many add-on invocations run at once, which is still sixteen
  and still fixed in the build.

  Per-module attribution is first-class:
  `linkctrl_addon_redirect_duration_seconds{addon,class}` is a separate curve from
  `linkctrl_redirect_duration_seconds`, and separate in both directions — the
  redirect histogram now **excludes** the time an add-on held the request, so
  core's curve still describes core after you install one. That is what lets an
  operator tell core's latency from each add-on's and take the problem to the right
  team. An invocation skipped because all sixteen instance slots were busy is on
  `linkctrl_rate_limited_total{limit="addon_inline"}`, and an observation dropped
  the same way on `{limit="addon_observe"}`. The Add-on manager deliberately does
  not render either: a saturation count shown against one add-on blames whichever
  module was asked, not whichever filled the slots.

  An inline invocation reaches only a **redirect-safe subset** of the ABI — the
  ungated host facts, its own settings and the two functions the class exists for.
  Storage, the request, the session and templates are refused there whatever the
  manifest declared, which is the redirect tree's own *no session lookup, no CSRF,
  no template rendering* rule reaching across the boundary. Three add-on names are
  reserved as a consequence of the new variables: an add-on called `inline` or
  `instantiate` is refused at load, because a setting of its called `deadline`
  would be read from `LINKCTRL_ADDON_INLINE_DEADLINE` or
  `LINKCTRL_ADDON_INSTANTIATE_DEADLINE`. `pool` is reserved the same way, for
  `LINKCTRL_ADDON_POOL_SIZE` and `LINKCTRL_ADDON_POOL_TTL`.

- **An add-on can sign somebody in, and LinkCtrl decides what that means.**

  A module whose manifest declares `session.mint` can complete an external
  identity flow — an OIDC sign-in, a corporate provider — and tell this instance
  that somebody authenticated. **It never makes the session.** It makes an
  assertion; LinkCtrl decides whether an account exists for that external
  identity, whether that account may sign in, how long the session lives, and
  whether a second factor is still owed. The cookie is written by LinkCtrl. No
  add-on is ever handed a session token, a cookie or a session row, and there is
  no function in the published ABI that returns one.

  **Connecting a provider is explicit and is never guessed.** The mapping from a
  provider's subject to an account here is written only while the person it
  belongs to is signed in, in their own browser. **Nothing matches on the email
  address an assertion carries** — that is the classic account-takeover shape, and
  it is absent by design rather than by omission: there is no statement in this
  product that resolves an assertion by any column other than the add-on, the
  issuer and the subject. An assertion for an identity nobody has connected signs
  nobody in.

  **A second factor is not bypassed.** An account with two-factor authentication
  enrolled meets its code prompt after an add-on's assertion, exactly as it does
  after a correct password — the assertion gets somebody as far as the prompt and
  no further. An operator whose provider already performed a second factor can say
  so with `LINKCTRL_ADDON_<NAME>_MFA_SATISFIED=true`, and only that exact word
  turns it off.

  **An add-on that signs people in defaults to `required`.**
  `LINKCTRL_ADDON_<NAME>_FAILURE_CLASS` is read before the manifest's class for
  **every** add-on, not only the authentication ones, so an operator can make any
  add-on `required` or `degrade` from the environment; a value that is neither
  stops the instance rather than falling back. With no such variable set, an
  add-on holding `session.mint` is `required` whatever its manifest says, because
  the publisher cannot know whether your instance has another way in and an
  instance that boots with sign-in silently missing is the worse failure. Editing
  `failure_class` in such an add-on's manifest therefore changes nothing, and
  `LINKCTRL_ADDON_<NAME>_FAILURE_CLASS=degrade` is the only way to say otherwise —
  which `docs/operations.md`'s recovery runbook now says where an operator reads
  it. Its consequence is stated: external sign-in disappears while local sign-in
  continues.

  **Two setting names are now reserved, and a manifest using one is refused at
  load.** `failure_class` and `mfa_satisfied` live in the same
  `LINKCTRL_ADDON_<NAME>_<X>` namespace as an add-on's own settings, so one
  variable cannot be your answer about an add-on and a value the add-on reads at
  the same time. An existing add-on declaring a setting by either name stops
  loading when you upgrade, with the reason and the reserved list on stderr.

  The audit log gained `session.minted_by_addon`. Every session minted this way
  leaves a record under it naming which add-on and which issuer vouched — and deliberately nothing about the external identity, so
  the erasure sweep has nothing new to reach. **That includes an account with a
  second factor**, where the session is minted after the code prompt rather than
  at the assertion: the provenance is carried through the prompt, so the record
  describes the session that exists rather than only the assertion that asked for
  it. Deleting an account takes its
  connected identities with it, for the same reason it takes a password-reset
  token: a connection is a standing credential that admits somebody with no
  password.

  **Every add-on's pages are now rate limited per address**, and this is a change
  for add-ons that have nothing to do with signing anybody in. Until now
  `/addons/<name>/…` carried no limit at all, which was correct while an add-on
  could only draw a page; an add-on that can mint changed that, because a stranger
  repeating a request could supersede somebody's outstanding two-factor prompt and
  write an audit row each time. The limit is `LINKCTRL_LOGIN_RATE_PER_MIN`, shared
  with the sign-in form so that alternating between the two gains nothing, and it
  covers **every route an add-on serves** rather than only the add-ons that hold
  `session.mint` — a bound that depends on which permissions a manifest happens to
  declare is a bound the next release can move without anyone noticing. The cost is
  real: a dashboard add-on carrying no credential now spends the same budget, and
  an operator running one behind a shared address or a NAT may have to raise that
  number.

  **A path under `/addons/` that reaches no add-on costs nothing.** It answers 404
  without being charged, on the same rule the 404-probe limit has always followed:
  a request that could not be the thing does not spend the budget the thing has.
  Otherwise a scanner walking two well-known paths under a prefix no add-on serves
  would deny people their sign-in — and behind a proxy with `TRUSTED_PROXIES`
  unset, deny it to every visitor at once.

  If you watch `linkctrl_rate_limited_total{limit="login"}`, note that it now
  counts add-on page refusals as well as credential ones.

  There is no screen yet for reviewing or removing a connection; that arrives with
  the Add-on manager.

- **An add-on gets this machine's clock and this machine's entropy.**

  The runtime add-ons run in defaults to a *fake* clock and a *fake* random source,
  and LinkCtrl was shipping those defaults. The random source was a compile-time
  constant, so every module on every deployment drew the same bytes — and because
  a request gets a fresh module instance, every visitor was handed the same value.
  The clock started at 2022-01-01 and advanced a millisecond per reading. An
  authentication add-on built on that would have given every LinkCtrl instance on
  earth one `state` parameter, one nonce and one PKCE verifier, and checked token
  expiry against 2022.

  Both are now the operating system's. `crypto/rand` and `time.Now` inside a
  module do what a publisher assumes they do, so **a module built against an older
  SDK needs no rebuild** — the repair is underneath those calls. The ABI also
  gains `random_bytes` and `time_now`, the same two sources with a documented
  shape, neither of which costs a permission. `sdk`'s own documentation said the
  old behaviour out loud and no longer does.

  The add-on ABI moves to **0.1.1** for three new functions — `random_bytes`,
  `time_now` and `identity_link`, which is how an external identity is connected
  to the account of somebody already signed in — and for `session_mint` becoming
  live. Both are additive under the policy in
  `docs/addon-abi.md`, so the *generation* a manifest declares does not move: an
  add-on built against `abi_version: 1` keeps loading, and one that wants the new
  functions rebuilds against the newer SDK.

- **An add-on can serve pages, under its own prefix, and LinkCtrl draws them.**

  A module whose manifest declares `routes.own_prefix` now answers requests under
  `/addons/<name>/` on the dashboard host — never on the link host, which still
  serves short links and nothing else. Configuration reaches it the way it reaches
  the product: `LINKCTRL_ADDON_<NAME>_<SETTING>` in this instance's environment, for
  the settings its manifest declares, with a value here outranking the manifest's
  own default. And an add-on can ask who is signed in on the request it is
  answering, which costs a grant of its own — `session.context`, which is why the
  permission vocabulary below is seven tokens rather than the six it was planned
  as — because an add-on that draws a page has not thereby asked to know the
  identity of everybody who opens it.

  **The add-on does not write the HTML, and that is the whole of the security
  claim.** What a module answers is *text*. The content types it may name for
  itself are `text/plain` and `application/json`, neither of which a browser
  executes; `text/html` is refused at the moment the module writes it; and by
  default LinkCtrl wraps the text in its own page, escaped like every other value
  on every other page. So a module that answers with a script tag, an inline
  handler or an external reference puts the *characters* of one on the screen —
  asserted against a real module that tries all three — and the
  Content-Security-Policy is byte-identical to what it was before add-ons could
  draw anything. There is no sanitizer to get wrong, because there is no markup
  path to sanitize.

  The cost of that shape is stated rather than buried: an add-on's page is plain.
  It ships no markup, no stylesheet, no font and no image, and the ABI function
  that would change that is declared and still refused. What an add-on gets
  instead is this product's own layout, its theme in both modes, and no front-end
  toolchain to bring.

  **Three things worth knowing before you install one.** Those pages are reachable
  **without signing in** — they have to be, because an add-on that authenticates
  somebody is answering a request from a person who has no session yet — and an
  add-on learns nothing about who is signed in when nobody is. A module holding the
  routes grant can **redirect a visitor anywhere**, since sending somebody to an
  identity provider is the point of one; LinkCtrl enforces only that the redirect
  is never permanent. And sixteen add-on invocations run at once across the instance,
  a further page request waiting on the request's own timeout: each gets an
  instance of the module to itself and each instance is capped at **8 MiB** of
  memory, and eight instances are kept warm between invocations, so add-ons add at
  most **192 MiB** to what this instance holds. That
  isolation is deliberate — one visitor's state cannot be left where another
  visitor's request can read it — and it means an add-on keeping state between two
  requests of one flow keeps it in the schema it owns, where it survives a restart
  and every replica can see it. **Kept warm does not weaken it.** A reused instance
  still carries the guest's own memory, so before one is handed on LinkCtrl writes
  back the copy it took of that memory when the module started: what the last
  redirect left is not what the next one reads, and an invocation that was killed
  or trapped is closed rather than reused at all. A module that asks for more memory than its cap is
  stopped by the runtime and answers 502 for that one request; one whose memory
  section *demands* more than the cap as its minimum is refused at load, with the
  add-on named. A module that merely declares a larger *maximum* loads and is held
  to the cap regardless, so a toolchain's choice there changes nothing. A request
  too large to cross into a module answers **413** and never reaches it, so a body
  somebody chose the size of cannot be reported in your log as the add-on failing.

  A cookie an add-on sets is bounded by the same declared prefixes as the ones it
  may read, scoped to its own path, with `Secure`, `HttpOnly` and `SameSite`
  applied by LinkCtrl — and **how many it sets is not something it decides**.
  LinkCtrl carries an add-on's cookies inside one cookie of its own,
  `linkctrl_addon_<name>`, with a second for the ones that outlive the browser
  being closed, so an add-on occupies two slots of a visitor's cookie store no
  matter how many cookies it sets or how often somebody visits its page. Browsers
  evict when that store fills, and the cookie evicted need not be the one that
  filled it: without this, an add-on holding nothing but the routes grant could
  sign a visitor out of LinkCtrl on every visit to its page, without ever naming a
  cookie it was not allowed to name. A cookie's `max_age` is bounded too, at 400
  days — the longest lifetime a browser would honour anyway — and a longer one is
  refused rather than quietly written as something else. Each jar holds about
  3 KiB; past that an add-on's oldest values go and the log says which add-on ran
  out of room.

  An add-on's configured secret is held in the type that refuses to print itself,
  whatever the manifest called the setting, so it cannot reach a log through a
  line about the add-on. Settings are edited from the Add-on manager's
  detail page, which is in this release; an environment variable still wins over
  a stored value, and changing *that* takes a restart.

  **Two add-ons cannot both load when one's name plus an underscore begins the
  other's** — `oidc` and `oidc_x`. Both are refused, counted as
  `linkctrl_addon_loads_total{outcome="name_collision"}`, and the boot log names
  the pair, because a cookie prefix and a `LINKCTRL_ADDON_` variable are each the
  add-on's name with something joined onto it, so the two namespaces overlap and
  there is no honest answer to whose a shared one is. Neither is awarded the
  other's, including the one that would have loaded first: rename a directory and
  the `name` in its manifest with it. If either add-on is `required`, the instance
  does not start until you do.

- **An add-on can have tables of its own, in a schema of its own, that it cannot
  leave.**

  A module whose manifest declares `storage.own_schema` now gets a Postgres schema
  called `addon_<name>`, and two host functions to read and write it. The schema
  boundary is the whole of the permission: nothing the host offers names another
  add-on's schema, and nothing reaches this product's tables. An add-on can still
  hand *its own* schema to anybody, because it owns it — one `GRANT` does it — so the
  host reads the schema's grants at every load and refuses an add-on that has given
  them to anyone but itself, until an operator revokes. Storage lands before routes and
  hooks deliberately — it is the first add-on capability with data to lose, and an
  add-on that misbehaves here damages only what it owns.

  **The boundary is a database role, not a search path**, and the distinction is the
  substance of it. A search path decides where an *unqualified* name resolves and is
  never consulted for `public.links`, so on its own it confines nothing; privileges
  are what refuse the read. Privileges only bind if the session *is* the confined
  role, so the host creates one login role per add-on and opens a connection
  authenticated as it, with a credential generated at every boot and stored nowhere.
  Issuing `SET ROLE` on the application's own connection was tried, measured against
  Postgres 17 and rejected: a single `DO $$ BEGIN EXECUTE 'RESET ROLE'; … END $$`
  escapes it, and `SET SESSION AUTHORIZATION` is checked against the *session* user
  rather than the current role, so it succeeds whenever the application connects as
  a superuser — which the shipped compose file does. Authenticated as the role, both
  are refused, and so are schema-qualified reads, the same read hidden in a CTE, a
  `SECURITY DEFINER` function the add-on's own DDL installed, `COPY … TO PROGRAM`,
  and two commands in one payload. All of that is asserted from inside a real
  wasm module compiled against the published SDK, which panics if any of them works.

  One statement per call, because the host parses through Postgres's extended
  protocol. A read runs in a `READ ONLY` transaction, so the read function cannot
  write. Each statement gets five seconds and each result a megabyte, and one add-on
  gets four database connections. Four bounds the add-on; it is not yet a promise to
  the product, and the release notes say so rather than the opposite: the guard that
  refuses `DB_MAX_CONNS + DB_REDIRECT_MAX_CONNS > 90` against the shipped
  `max_connections = 100` does not count add-on pools, so several storage add-ons are
  connections nothing checks. Configure with that in mind on an instance running
  more than a couple.

  **The host runs an add-on's migrations**, at load, before the listener opens, with
  the same session lock that serializes the product's own across replicas. An add-on
  ships them in a `migrations/` directory and the manifest names every file with its
  own digest, which does two things: the DDL that runs is the add-on *author's*
  rather than whatever is on disk, and the set is closed — a `.sql` file the manifest
  does not list refuses the add-on, so DDL cannot be added to an installed module
  without editing the artifact that describes it. The migrations are applied as the
  add-on's own role, so DDL naming another schema is refused by Postgres rather than
  by a parser, and a `SECURITY DEFINER` function it creates is owned by a role that
  can reach nothing. The host then asks Postgres itself three questions and refuses the
  add-on if any answer is not empty: what does this role own that is not in its
  own schema, what is in its schema that this role does not own, and what has it
  granted on that schema to anybody but itself. The first two are set
  differences over the catalogues Postgres's own `DROP` statements consult —
  `pg_shdepend` for `DROP OWNED BY`, `pg_depend` for `DROP SCHEMA` — rather than a
  list of the places an add-on might have put something, because three earlier
  versions of that list each turned out to be missing one. The third reads the
  schema's own access list and the access lists of the relations in it, because a
  grant is not an object and no catalogue of objects records one. Migration state
  is a goose table inside the add-on's own schema, so re-loading a module applies
  nothing twice and an add-on's state has no half in a table the product owns.

  **Six costs, stated rather than implied.** The application's database user now
  needs `CREATEROLE` — and password authentication has to work for the new role — for
  an add-on that stores data; a deployment authenticating by `peer` or by a cloud
  IAM token cannot load one, and it says so instead of running the add-on
  unconfined. A `required` add-on whose migration fails stops the instance, so
  somebody else's bad release can hold an instance down whose own configuration did
  not change; `docs/operations.md` has the recovery order. And a confined role still
  reads `pg_catalog`, which Postgres does not make revocable, so an add-on can
  enumerate the names of tables it cannot read a byte of.

  **A restore has to carry roles now, and the shipped procedure did not say so.**
  `pg_dump` carries none of them — that is `pg_dumpall --roles-only` — so a restore
  into a cluster whose roles were not restored separately leaves an add-on's tables
  owned by the application. The add-on is then refused on its own rows, and the load
  says which tables rather than failing somewhere inside a migration.
  `docs/deployment.md`'s backup section carries the roles dump and the order to
  restore in.

  And **two capabilities are accounted for rather than closed, and a third is
  narrowed**, each because closing it was measured and is not available. A confined
  role can create a Postgres **large object**, which belongs to no schema —
  `linkctrl_addon_large_objects{addon}` publishes the count, it should be zero
  forever, an add-on owning one is refused at its next load, and the purge in
  `docs/operations.md` grew the `DROP OWNED BY` line that removes one. It can take
  one of the product's job **advisory locks**, which the host now releases before the
  connection is reused, bounding the hold to the add-on's own statement. And it could
  create a **temporary table**, which is outside its schema as much as a large object
  is: installing a storage add-on now revokes `TEMPORARY` on the database from
  `PUBLIC` and grants it back to the application, after which every spelling of it is
  refused. That revoke is a narrowing and not the boundary — it does nothing unless
  the application owns the database, and no dump carries it — so the post-condition
  above is what holds, and it reports a temporary relation whether the revoke took or
  not. An operator sharing this database with another application that uses temporary
  tables loses them; `docs/deployment.md` says so rather than leaving it to be found.
  A third narrowing has none of those conditions attached and is why the three are
  described together: the confined role can set any user-settable Postgres parameter
  on *its own role*, where it survives every boot and is inherited by every
  connection the add-on's pool opens — `work_mem = '4GB'` was accepted and read back
  by a fresh session — so each load now clears the role's settings before re-pinning
  its search path. That one needs nothing more than the `CREATEROLE` this release
  already asks for, and a restore does not undo it. **Clearing them means in every
  database**, which is not what the statement that does it clears: Postgres keeps a
  role's defaults once for the cluster and again for each database, the confined
  role can write the second kind for *any* database — including one it cannot
  connect to — and those outlived every reboot until this release. The load now
  reads the databases a role has settings in out of the catalogue and resets each,
  rather than naming one and being evaded by another, and the load's post-condition
  refuses an add-on whose role still carries one — so parking a setting earns the
  add-on nothing either way: the load resets every scope before it checks, so a
  setting parked from a query is cleared and one parked inside the add-on's own
  migrations is refused. **The repair is per add-on and runs at
  that add-on's load; nothing sweeps roles no add-on claims.** Removing an add-on
  never removes its role, so a setting it parked before it went stays in the
  cluster. That leftover is inert — a session default is read only by a session
  that logs in as the role, and nothing logs in as an add-on's role once its module
  is gone — and re-installing the add-on clears it. Clearing it by hand is one
  `ALTER ROLE … RESET ALL` per scope, in `docs/operations.md` with the query that
  lists them. LinkCtrl does not do it for you, because a name beginning `addon_` is
  not evidence LinkCtrl created the role and a cluster's roles are not all its own.

  **Removing an add-on does not remove its data.** Delete a module's directory and
  the schema stays; the next boot enumerates `addon_*` schemas nothing claims and
  warns about them. Nothing deletes one — a purge is an operator's explicit act.
  There is no quota on how large a schema may grow either, which is the same answer
  the audit log gets: `linkctrl_addon_schema_bytes{addon}` makes the **stored** growth
  visible, measured hourly by every replica, with `linkctrl_addon_large_objects{addon}`
  beside it for the stored growth a schema's size cannot show. **The schema size counts
  every relation in the schema that has storage** — tables, sequences, materialized
  views — rather than a list of the kinds anybody thought of. That is a correction made
  before release rather than after: the first version summed ordinary and materialized
  tables, so a **sequence** in the add-on's own schema was 8192 bytes it reported as
  nothing, and it read `0` for a schema holding 188 MB of them. It never needed a
  misbehaving add-on either — the migration table the host creates in that schema
  declares an identity column, and an identity column owns a sequence. The qualifier is
  deliberate: both gauges, and the post-condition above, cover the objects Postgres
  catalogues, and a session holding a `WITH HOLD` cursor keeps a temporary *file* on
  disk that is in no catalogue and under no gauge — transient, freed when the
  connection ends, and bounded only by a `temp_file_limit` a superuser must set. So
  watch the filesystem as well as these two, which is what `docs/SECURITY.md` now
  says.

  **On more than one replica**, each mints the add-on role's password for itself, so
  the newest replica's boot invalidates the credential the others hold; they re-mint
  on their next connection and log it at warn. `docs/deployment.md` says what that
  line means on a single-replica instance.

- **An add-on gets what it named and nothing else.**

  A manifest's `permissions` array is now the whole of what a module may do, and
  the host enforces it rather than trusting the module. Every function in the ABI
  names the permission it costs; the host resolves a manifest's declarations at
  load and refuses any call whose grant is not held, with a status the module can
  branch on and a `linkctrl_addon_refusals_total{addon,permission}` counter an
  operator can alert on. The check lives in the host's dispatch rather than in each
  function, so a capability cannot arrive with its check somewhere else.

  **The vocabulary is closed and it is seven tokens**: reading the add-on's own
  settings, owning a Postgres schema, serving a path prefix and rendering its own
  templates, asking who is signed in, minting a session, observing redirects out of
  band, and running inside the redirect path. A `permissions` entry outside that
  list refuses the add-on at load, for the same reason an unknown manifest field
  does. Four functions cost nothing and are ungated
  deliberately: asking the host its ABI version, drawing random bytes, reading
  the clock, and writing a line to the log, which is the one capability that was granted on
  purpose.

  **Ungated is not the same as trusted.** Because every loaded module can write to
  the log, including one that declared nothing at all, the host neutralizes the
  message before the line is written and bounds it at 4 KiB. What survives as
  itself is the set of **graphic** characters — every letter, mark, digit,
  punctuation mark, symbol and space, in every script, with one exception — and
  everything else appears in the line as its escape: a newline, a control character,
  an ANSI escape, every format and bidirectional control, every unassigned or
  private-use code point, and the 268 graphic code points the host treats as
  invisible — 267 of them Unicode's default-ignorable characters, which the
  host works out from Unicode's own definition rather than asking for the one table
  Go ships under a nearly identical name, and the 268th `U+2800 BRAILLE PATTERN
  BLANK`, which that definition does not carry and which is escaped as the one blank
  nothing treats as whitespace.

  **That is a published property and not every character that renders as nothing**,
  and the difference is stated because Unicode publishes nothing for the second.
  Eight combining marks it annotates as *"shape shown is arbitrary and is not visibly
  rendered"* — `U+2D7F`, `U+17D2`, `U+10A3F`, `U+1107F`, `U+11A47`, `U+11A99`,
  `U+11F42` and `U+16FE4` — reach a line as themselves, as do seventeen space
  characters and thirteen prepended concatenation marks. **What bounds that is that
  the log is write-only to a module**: an add-on may post a line to it and has no way
  to read one back — `log` returns a status and no bytes, no other function in the ABI
  hands log content back, a module gets no preopened file and its output streams are
  discarded, and its storage is a schema of its own that the log does not live in. So
  what survives is something an operator can see, and it becomes a channel only if an
  operator sends the log to the add-on's author. All four are asserted, and the two
  about files and streams are asserted from inside a module rather than by reading the
  host's own settings, since the settings are what a later change would move.

  **The neutralization is the module's boundary and not one function's**, which is a
  correction to what the previous entry implied. A manifest that fails validation is
  reported with the value it failed on, and Go's `%q` leaves every mark and every
  letter alone, so a hostile name, version or migration filename reached an operator's
  log and an instance's fatal message untouched.

  **It is enforced by the logger and not by a rule about which lines to write
  carefully.** The host wraps the logger it is given, and everything in the add-on
  subsystem writes through one derived from it — including the line that names a
  migration as it is applied, which is written by a different package on the path that
  runs when nothing is wrong. Two earlier rounds fixed the places they could find and
  wrote the list down, and the list was wrong both times; there is no list now,
  because there is no place that can be missed. An error handed back to the page layer
  is neutralized the same way, once, where it leaves the subsystem rather than at each
  of the points one is built.

  **An operator's manifest error is not a log line and no longer carries a log
  line's bound.** The host reports every problem with a manifest at once, so somebody
  publishing an add-on for the first time fixes them in a single pass instead of one
  per restart. The escaping used to bring the log's 4 KiB limit along with it, cutting
  that list with nothing to say it had been cut and running the whole of it onto one
  line. The escaping and the limit are separate things now: the list arrives whole and
  line by line, while the same failure still occupies exactly one bounded record in
  the log.

  The residue property Go ships is the leftovers of that definition and not the
  definition, and it falls 398 code points short of it. The 260 of those that a
  reader could ever have seen are the **variation selectors**: invisible marks that
  ride on the character before them, which a module could have used to carry text out
  of a log line that read as ordinary. The other 138 are format characters, which were
  escaped either way.

  **Those 260 are deleted rather than escaped, and they are the only thing this
  boundary removes.** A selector has nothing of its own to show a reader, so spelling
  it out would put `\ufe0f` through the middle of every emoji anybody logs and buy
  nothing; deleting it costs nothing either. `❤️` arrives as `❤` and is still a heart,
  `😀` is untouched because it carries no selector to lose, and a selector hung off a
  letter, a space or a block-drawing character takes nothing with it when it goes.
  There is no exemption for the legitimate emoji case: whether a selector is visible
  at all depends on the font in front of the reader, and Unicode publishes no property
  that says which characters those are — two narrower rules were written and both were
  broken before release: the **first** by a progress bar drawn from `█` and `░` that
  carried a secret through byte for byte, and the second by a channel built out of the
  very emoji Unicode registers, whose selector a renderer is free to ignore. The
  exception is the **backslash**, which is doubled, because
  it introduces every escape the host writes: without that, a module writing `\`
  and `n` produced the line a real newline produced, and the mark on a truncated
  line was something a message could end with itself. **The carve-outs run the
  other way and are named**: Unicode's prepended concatenation marks — the Arabic,
  Syriac and Kaithi signs that scope the digits after them — are meaning rather than
  concealment and are left alone, read from Unicode's property so that a host built
  against a newer revision carries what it added. Stated as what is *permitted*
  rather than as a list of what is caught, because a list of invisible characters is
  behind the next Unicode revision the day it is written. So an add-on cannot forge
  a record that reads as this product's own, cannot make a complete message read as a
  truncated one, and cannot put a character that renders as nothing in front of a
  reader. That last is stated narrowly on purpose: it is about characters Unicode
  defines as ignorable, not about anything an add-on might write to mislead. A
  no-break space still looks like a space, and two spellings of Å still look alike.
  And writing to the log costs no permission, so an add-on that wants a secret in an
  operator's log can simply write one — what this bounds is what a reader cannot see,
  not what an add-on chooses to say. Nothing is refused for it — a module whose
  message needed neutralizing still gets to speak, which is the whole reason the log
  costs nothing.

  **The refusal comes before the availability status.** A module that declared
  nothing is refused for want of a declaration, not told that the host has not
  implemented the function — so probing for a capability, which the ABI invites,
  only reports on capabilities the module asked for.

  **Running inside the redirect path is a separate declaration, and no release
  grants it.** It is published now so that the release admitting an add-on onto that
  path enforces behaviour against a permission that is already enforced, and so a
  module cannot acquire it by accident while asking to observe redirects. An add-on
  declaring it loads, does not hold it, and the boot log says so.

  **What each add-on holds is readable**: named in the boot log, and on
  `linkctrl_addon_info`, whose `permissions` label carries the grants a module
  actually **holds** rather than the ones it asked for. The Add-on manager is where
  this gets a proper surface.

- **The add-on ABI is published, versioned, and consumed as a generated SDK.**

  An add-on reaches this product through a fixed set of functions it imports from
  the host, and through nothing else — no socket, no file, no database connection,
  no environment. That set is the contract, it is enumerated in one place, and
  `docs/addon-abi.md` is it: the functions, the calling convention, the version,
  and the rules for changing it.

  **The SDK is generated from the host's own definition** into an importable Go
  package, `github.com/DevOfPie/LinkCtrl/sdk`, which depends on the standard
  library and nothing else. An add-on lives in its own repository, imports that
  package and compiles for `GOOS=wasip1 GOARCH=wasm`; a test in this repository
  builds a consumer module against the SDK alone, with the module proxy turned
  off, so the claim is mechanical rather than aspirational.

  **The ABI follows semantic versioning with deprecation windows.** An add-on's
  manifest declares which generation it was built against, and that is checked at
  load, before any of the module is read — a module built against a newer
  generation is refused, and so is one whose generation has been retired.
  `linkctrl_addon_loads_total{outcome="abi_unsupported"}` counts it and the boot
  log names both versions, because the fix is a version rather than a file. A
  deprecation runs for at least two minor releases and 90 days, is announced in
  four places including the SDK's own Go `Deprecated:` markers, and what counts as
  breaking is a table rather than a judgement call.

  **Sixteen functions work; the rest are declared and refuse.** Logging, reading the
  add-on's own declared settings, asking the host its ABI version, the two storage
  calls, and — since the add-on pages entry above — reading the request, writing
  the response and asking who is signed in are live, as are the clock, the random
  source, minting a session and connecting an identity. Rendering a template and
  redirect observation are the two that remain declared — their names fixed, their
  signatures fixed enough to compile against — and answer a refusal a module can
  branch on until the release that implements them. Rendering is the one that
  will not simply be filled in: a page's HTML is composed by the host and an
  add-on returns text, so the function as declared has no behaviour to grow into
  and what happens to it is an open question about a published contract. So an add-on can be written against the whole contract now instead of being
  rewritten per release. Implementing a declared function is explicitly not a
  breaking change, and neither is finishing the parameters of one no release has
  implemented: that is what would otherwise have cost a version per limb, and the
  one place it costs a publisher anything is named in `docs/addon-abi.md` along
  with the rule.

  **No function hands an add-on a client's address, in any form.** That is a
  property of the surface and not a promise about somebody else's code: the record
  carrying redirect data is bound to what `click_events` may carry — country-level,
  and that table has no address column — and a test reads the column list out of
  the migration to hold the bound. Region and city are refused too, though the
  columns exist, because they resolve transiently and are never stored. An add-on
  cannot store what it is never handed. A module's only route to the operator's log
  is the ABI's own `log` function, attributed to the add-on; its output is still
  discarded.

  **Nor does any function hand an add-on a credential of this instance's.** An
  add-on that serves a route sees the cookies whose names begin with one of the
  `cookie_prefixes` its manifest declares, and a declared prefix must begin with
  the add-on's own name — so an authentication add-on gets its own state cookie
  and cannot ask for this instance's session cookie, which is server-side and
  opaque and therefore *is* the credential rather than a description of one. The
  same namespace bounds what it may set, because a cookie an add-on is not allowed
  to read is one it must not be able to overwrite. Neither can an add-on be denied
  its own namespace by whichever registered first, since the namespace comes from
  the name. That alone did not stop two add-ons claiming each other's, and the rest
  of the answer is the name-collision refusal described above. Every payload the
  host composes is enumerated field by field for the same reason, including the one
  it hands back when it accepts a module's authentication claim: that one carries
  when the session expires and whether a second factor is still owed, and no
  token, no cookie and no row of the sessions table.

- **Add-ons: an instance can load WASM modules, and refuse the ones that do not
  check out.**

  Point `LINKCTRL_ADDONS_DIR` at a directory holding one subdirectory per add-on,
  each with an `addon.json` and the `.wasm` that manifest describes. At boot each
  module is verified against the `sha256` in its manifest and either instantiated
  or refused — a module whose bytes do not match is never compiled. What happens
  when one will not load is the add-on's own declaration unless you say otherwise:
  `required` stops the instance with the reason, `degrade` logs it, counts it, and
  the instance serves without the module, and `LINKCTRL_ADDON_<NAME>_FAILURE_CLASS`
  outranks the manifest for any add-on. A manifest that cannot be parsed still
  stops the instance whatever either of you said, because there is no add-on left
  to have a class.

  Three metrics come with it, on the metrics listener as everything else there is:
  `linkctrl_addon_loads_total{addon,outcome}`,
  `linkctrl_addon_info{addon,version,abi_version,failure_class,permissions}` and
  `linkctrl_addon_refusals_total{addon,permission}`. All three are absent entirely
  on an instance with no add-ons directory — as are the two later milestones added,
  `linkctrl_addon_schema_bytes` and `linkctrl_addon_large_objects`, so the presence
  of any `linkctrl_addon_` series is the answer to whether this instance is running
  an add-on at all.

  **Unset is the default and it costs nothing.** No WASM runtime is constructed,
  no goroutine is started, no route is mounted, no table is created and no metric
  series is published — each of those absences asserted by a test rather than by
  this paragraph.

  **What a loaded module can reach is one published list.** `schema_version` is
  checked for equality and unknown manifest fields are refused, deliberately: a
  manifest this host does not fully understand is refused rather than
  half-honoured. A key must also be spelled exactly as documented and appear
  once, at every level of the file: nothing hashes `addon.json`, so the manifest
  is the trust root, and a JSON parser's ordinary tolerance — keeping the last of
  a repeated key, binding `SCHEMA_VERSION` to `schema_version` anyway — would let
  a published manifest say one thing to whoever reads it and another to the host.
  Reading it that carefully means holding the whole file, so a manifest is also
  refused above **64 KiB** — far past anything the format can mean, and named here
  because it is a refusal a publisher can meet.

  **An add-on that never finishes loading is skipped rather than waited for.**
  Each add-on gets 30 seconds to compile its module and 30 more to start it, and a module
  that spends it is counted as
  `linkctrl_addon_loads_total{outcome="load_timeout"}`, with the add-on named in
  the log; the failure class its manifest declares then decides whether the
  instance stops or serves without it, exactly as for any other load failure. The
  budget is per add-on, so one module that hangs does not spend anybody else's,
  and it covers the module rather than the database — an add-on's migrations wait
  on the migration lock for as long as this product's own do. Compiling and
  starting an ordinary add-on takes well under a second.

  **The trust boundary is the directory.** A module in it is code this instance
  executes; own it, and mount it read-only. See
  [configuration.md](docs/configuration.md) and
  [SECURITY.md](docs/SECURITY.md).

  The runtime is [wazero](https://github.com/tetratelabs/wazero), which needs no
  cgo, so the published binaries and image stay statically linked.

### Fixed

- **An add-on's outbound request is made once, whatever size the answer is.**

  An add-on asks the host to fetch something into a buffer it owns, and the
  add-on interface says a buffer too small means nothing was written and the
  caller tries again at the size it was told. That retry was making the request a
  second time. For reading a document it was invisible; for anything the other end
  counts it was not — an OpenID Connect token exchange went out twice and the
  second one came back `invalid_grant`, so sign-in failed for every response over
  512 bytes, which is every real one.

  The host now keeps what came back and answers the retry from it. A module that
  deliberately fetches the same address twice still gets two requests. Nothing
  about an add-on changes; the fix is entirely in this server. Found by building
  the OIDC add-on against it, which is what that exercise is for.

  **The add-on ABI moves to 0.1.5**, and which kind of fix it is has to be said
  because the policy has two answers for a bug fix that changes an observable
  answer: it is the **additive** one, because the old answer contradicted the
  calling convention's own documentation and nothing could reasonably have relied
  on a request being sent twice. Nothing new is importable, so a module built
  against 0.1.4 loads on a 0.1.5 host unchanged and simply stops being affected.

## [0.3.0] - 2026-08-18

### Added

- **A rolling deploy of every replica, under load, costs nothing — and a single
  container is still a tested configuration.**

  The multi-replica contract below is a promise. This is the measurement of it,
  and the guarantee that it was not bought at the expense of the deployment
  almost everybody actually runs.

  **Three replicas behind a load balancer, every one destroyed and rebuilt while
  2,000 requests a second went through it: zero requests failed, zero were
  retried, cached p99 295µs against a 20ms target, and the whole replacement
  took 35 seconds.** No request ever waited — the load generator never needed
  more than three concurrent connections for the entire run.

  **The drain delay is why, and it now has a price rather than a rationale.**
  The same replacement performed with SIGKILL instead — no drain, no readiness
  change, the listener simply gone — cost 905 retried requests of 239,833, four
  response errors, and a worst case of a full second. Still no failures, and the
  credit for that belongs to the load balancer retrying rather than to this
  product: a balancer configured without retries answers 503 to all 905. If you
  run several replicas, `LINKCTRL_SHUTDOWN_DRAIN_DELAY` is the number that
  decides which of those two paragraphs describes your deploys.

  **A single container remains a supported, tested configuration, and nothing in
  the high-availability work is required to run it.** The release image is
  started on a network carrying nothing but Postgres — no Redis, no load
  balancer, no second replica — and the redirect path, the dashboard, the API,
  the scheduler, cache invalidation and rate limiting are each driven over HTTP
  until they answer. It runs in CI on every push. A future change that makes any
  of them need a second component fails the build, which is the entire reason it
  was written now rather than after something did.

  **Two replicas can no longer both lead the same scheduled job during a
  deploy.** Every binary from 0.2.0 on asks for the same per-family advisory
  locks, so the old replica and the new one contend for one lock rather than
  holding one each, and a test now freezes those assignments so a future rename
  cannot quietly undo it. What remains is a leader that loses its database
  connection while still working, which no deploy causes and every job is
  written to survive being run twice.

  Both runs were taken on the built image against the seeded 100,000-link
  dataset; the figures, the method and what they cannot show are in
  [docs/slo.md](docs/slo.md).

- **Running more than one replica is now a supported configuration, because
  there is finally a contract to support.**

  Multi-replica operation already worked and was already documented. What did
  not exist was a statement of what each health endpoint promises, what a load
  balancer must do with it, and what happens to work in flight when a replica is
  killed rather than shut down politely. Without those, running two containers
  was something you did at your own risk.

  **The load-balancer contract, in one rule.** Wire liveness to `/healthz` and
  readiness to `/readyz`. Then act on the status **code**: 503 means take the
  replica out of rotation, 200 means keep it — *including* `degraded`. A
  `degraded` replica is one whose Redis is unreachable, and it still resolves
  every link, because redirects fall through to Postgres. An operator who reads
  `degraded` as *remove* takes their entire deployment out during a Redis
  outage, which is the exact failure the word exists to prevent. `/healthz`
  touches neither Postgres nor Redis, ever — a liveness probe that followed the
  database would restart every replica at once during one blip.

  **There is no startup probe, and that is now a stated choice** rather than
  something you might notice missing. Migrations run before the listener binds,
  so *not yet ready* and *not yet listening* are the same observable state.

  **What a killed replica loses: at most its buffered click events, and nothing
  else.** Scheduled jobs move to a follower within one tick of that job's family,
  because a Postgres advisory lock is released the instant the session holding
  it ends — nothing detects the death, the next follower simply finds the lock
  free. Webhook deliveries and outbox mail are claimed under a 60-second lease,
  so a dead replica's claims come back on their own and are completed elsewhere.
  Delivery is at-least-once and now says so: dedupe on the `X-LinkCtrl-Delivery`
  uuid.

  **`LINKCTRL_SHUTDOWN_DRAIN_DELAY` gets arithmetic instead of a
  recommendation.** It must exceed your load balancer's health-check interval
  times its failure threshold, plus a check's worth of slack. The shipped `5s`
  is sized for having no balancer at all and is explicitly not a
  recommendation — and because the drain delay and `SHUTDOWN_TIMEOUT` are spent
  in sequence under a 25-second ceiling, raising one trades against the other.

  **No component was added for any of this.** No coordinator, no external lock
  service, no second Postgres, no session affinity. A one-container deployment
  is unaffected: with one replica the holder of every lock is the only
  candidate. The contract is in `docs/operations.md`, and every clause of it
  that is a promise rather than an instruction has a test behind it.

- **This instance can now tell you when a new LinkCtrl is released — and it asks
  you first. Read this one even if you read nothing else here.**

  Once a day the instance asks GitHub whether a newer version exists and, if
  there is one, puts a notification in the instance principal's inbox. Nobody
  else is told: upgrading is the operator's, and a workspace member can only be
  made anxious by it.

  **What the request carries, in full.** This server's source address, and the
  version it is running, in the `User-Agent`. Nothing else — no instance
  identifier, no deployment size, no link counts, no configuration, nothing
  about the people using it. There is no request body, no query string and no
  credential. The response is read for a version number and discarded.

  **Nothing leaves your instance until somebody here has been asked and said
  yes.** On a fresh instance the question is on the setup page that claims it,
  ticked. **On an instance you are upgrading, it is put to the first
  administrator who signs in afterwards, and the check does nothing until they
  answer** — an upgrade cannot consent on your behalf, and neither can this
  release note. The consequence is worth stating: an instance nobody signs into
  never asks, and never tells you a release exists.

  Either way it is asked once. `LINKCTRL_UPDATE_CHECK=false` on the deployment
  is the answer given from the other side, it overrides whatever was said in a
  browser, and it cannot be overridden from one; air-gapped and
  egress-restricted deployments want it, and `docs/deployment.md` says what
  leaving it on costs them. A client claiming an instance through
  `POST /api/v1/auth/setup` can send `update_check`; omitting it leaves the
  question for whoever signs in.

  **`docs/SECURITY.md` no longer says there is no phone-home in the default
  configuration**, because there is one. That sentence was part of why somebody
  would self-host this, so it is edited rather than qualified: the *Egress* row
  now enumerates **five** outbound connections instead of four, and this is the
  only one of them that an ordinary instance turns on by agreeing to a question
  rather than by being configured.

  A failed check is silent — one debug line, no retry until the next day, and
  never a startup failure or anything a user sees. **So no notification is not
  evidence of being up to date:** GitHub's unauthenticated API is rate-limited
  per source address, and a throttled check looks exactly like a check that
  found nothing.

- **An API key belongs to your account, not to one organization.** A key used to
  be minted *into* the organization you were standing in and could never leave
  it. It is now minted by your account and reaches the organizations your account
  belongs to, the way a personal access token does elsewhere.

  **Nothing about the keys you already have changed.** Every key issued before
  this release stays pinned to the organization it was created in, and the
  upgrade writes no rows. What changed is what a *new* key is.

  **Three reaches, and the page names each.** *Workspace* is the default and is
  unchanged — the key acts where you made it. *Organization* pins the key to one
  tenant for its whole life. *Account* is the new one, and it is what an unpinned
  key now is unless you pin it: each request resolves one organization the way a
  sign-in does, following where you are working.

  **The same key is more powerful in one organization than in another, and that
  is deliberate.** A key's permissions are its scopes intersected with your role
  *there*, worked out on every request. Own an organization and the key can do
  what an owner can; be a viewer in the next one and the identical key can only
  read. Being demoted narrows every key you hold in that organization at once,
  without touching any of them.

  **An account key reaches an organization you join later.** That is what account
  means, and the alternative would be a key whose reach is a snapshot of a
  membership list nobody can see or correct. If you want the snapshot, pin the
  key. Pinning is irreversible: a rotation may narrow a key's reach and never
  widen it, so an account key can rotate into a pinned one and a pinned key
  cannot rotate back.

  **Your key list is your account's, not the current organization's.** It used to
  show only keys from the organization you were signed into while revoking
  reached every key you owned — so a key you could not see, you could still
  delete. Both now answer the same question.

  **`GET /api/v1/workspaces` answers an account key with every organization it
  reaches**, which reverses what 0.2.0 changed. That release narrowed the
  endpoint to one organization because a key was issued for one and reading
  about the others disclosed tenants it could not act in. An account key acts in
  them, so those are the tenants it is told about — minus any an administrator
  has cut it out of. A **pinned** key still sees one, for the original reason.

  **An administrator can stop an account key in their organization without
  destroying it.** Holding `apikeys.write` organization-wide, deleting somebody
  else's account key cuts your organization out of its reach: the key keeps
  working for its owner everywhere else, and the record is `apikey.reach_revoked`
  rather than `apikey.revoked`. A key pinned to your organization is still
  revoked outright, because your organization is all it ever reached.

  An account key needs an **organization-wide** membership wherever it lands. If
  your role in an organization is scoped to a single workspace, an account key
  does not act there — mint a key in that workspace instead.

  For API clients: `POST /api/v1/api-keys` takes an optional `organization_id`
  which pins the key, `POST /api/v1/api-keys/rotate` takes an optional `reach`,
  and every key representation gained a nullable `organization_id`. `lctl apikey
  create` gained `--pin`. The audit log gained `apikey.reach_revoked`.

- **Two-factor authentication, with recovery codes that make it survivable.**
  TOTP — the six digits an authenticator app shows — on top of the password.

  **It is off until an operator turns it on, and then off until each person turns
  it on.** Set `LINKCTRL_MFA_SECRET_KEY` to at least 32 bytes
  (`openssl rand -base64 48`) and the account page offers enrolment; leave it
  unset and nothing changes, which is what every instance was before this. The
  variable encrypts each account's secret at rest, and it is deliberately **not**
  `LINKCTRL_API_KEY_PEPPER`: sharing one value would mean rotating an API-key
  secret also locked every account out of its authenticator.

  Enrolling shows a QR code and the secret in text beside it — a phone cannot
  photograph its own screen — and nothing is written to the account until a code
  from that secret verifies. An enrolment you abandon leaves the account exactly
  as it was.

  **Ten single-use recovery codes** are issued at the same time and shown once.
  They are the answer to a lost phone, and they are why this shipped after account
  recovery rather than before it: a second factor makes being locked out strictly
  more likely. Using one is recorded and notifies the account, because it means
  either the phone is gone or somebody else has your codes. You can issue a new
  set at any time, which stops the old one working.

  Signing in gains a step: the password, then the code. Wrong codes count against
  the same lockout a wrong password does, so getting the password right does not
  buy an unlimited supply of guesses at six digits. A code that has just worked
  cannot work again inside its own window. Clocks are allowed to be thirty seconds
  out in either direction, and no more.

  **A password reset does not skip the code.** Proving you can read an email is
  one factor; letting it stand in for the other would make this worth exactly the
  mailbox. If you have lost the password *and* the phone, a recovery code is the
  way in; if you have lost all three, whoever runs the instance can help.

  Turning it off needs the password **and** a code — an authenticator code or a
  recovery code — and removes the authenticator entry and every recovery code
  together. An API key cannot do it: a key is not a person.

  Nothing new is downloaded and nothing new is dialled. The algorithm is RFC 6238
  over the Go standard library, the QR code is the generator the link pages
  already use, and a source scan now fails the build if anything on the
  authentication path grows an outbound connection.

  **What is not here, and was considered:** WebAuthn, passkeys, SMS and push, each
  a separate credential model with its own recovery story; and any way for an
  organization to *require* a second factor of its members, which needs a
  permission, an enforcement point and an answer for members who cannot enrol.

  For API clients: `POST /api/v1/auth/login` now answers `401` with an
  `mfa-required` problem document carrying `mfa_token` when the account has a
  second factor, and `POST /api/v1/auth/mfa/challenge` completes the sign-in. A
  client that has not been updated gets no session and an error it does not
  recognise, rather than believing it signed in.

- **An account can be deleted, and what it leaves behind is erased.** Until now
  nothing in this product removed a user. The schema had said otherwise since the
  first migration — `users.anonymized_at` carried the comment *"set by the GDPR
  erasure routine"* and had no writer — and by 0.2.0 five separate places
  described erasure in the present tense while none of it existed. Those
  sentences were corrected then. This is the feature.

  **`DELETE /api/v1/account`, and a section on the account page.** Both act on
  the calling account and nothing else: there is no administrative
  delete-somebody-else, because who may end another person's account is a
  permission question this release does not answer. Confirmation is the
  account's own password, and an API key is refused — a leaked credential must
  not be able to delete the person who owns it.

  **Two refusals, each naming what to do about it.** The account that
  administers the instance cannot be deleted; move the principal first with
  `lctl instance principal move --to <email>`. Nor can the sole owner of an
  organization that still exists — hand it over or delete it first, and the
  refusal says which organizations are blocking. Being left belonging to no
  organization at all is *not* refused: it is the ordinary way to arrive here.

  **What happens at once, in one transaction.** Sessions, API keys,
  memberships, notifications, outstanding password-reset links and any
  instance-level grants are removed. The account is marked deleted, and the
  address becomes available for a new account. When the call returns, no
  credential reaches the account.

  **What happens within the hour.** The audit log and the destination-dispute
  queue keep their rows — they are records of what happened, and one that
  vanishes with its subject is not a record — and an hourly pass replaces the
  name and address in them with the fixed label `deleted account`, then clears
  the account row's own fields. It reaches two more tables for the same reason:
  the invitations you redeemed and the notification your inviter received when
  you accepted, both of which belong to somebody else and so were never yours to
  delete. Access does not wait for that pass; only the residue does.

  **Two consequences worth knowing before you rely on it.** The surviving actor
  id is what keeps an erased person's entries correlated with each other, and
  that makes it pseudonymous rather than anonymous data: the residue identifies
  nobody from inside this instance, and anybody holding an external
  id-to-person mapping can still re-identify the actor. And **two addresses are
  deliberately left**: an *outstanding* invitation addressed to you, because
  that is an offer to an address rather than a record of a person and the
  address became reusable the moment the account was deleted; and a queued mail
  message not yet purged, which is bounded by the mail retention schedule but
  can outlive the account on an instance whose relay is down. An address in the
  **detail** of an audit record *is* reached — this paragraph said it was not
  while the release note below said it was, and the code is what settles it.
  `docs/SECURITY.md` states all of it.

  **Reusing a deleted address is correct, and will look wrong the first time.**
  A new account can be created at an address that appears in old audit entries
  under a tombstone. They are different people, and nothing about the old
  account carries over.

  Your links are not deleted. They belong to the workspace, which outlives you
  leaving it.

- **A forgotten password stops being permanent.** Until now the only route back
  into an account whose password was lost was an operator editing an argon2 hash
  in the database on somebody's behalf — true of every account on every instance,
  including the one that administers the box. There is now a *forgot your
  password?* link on the sign-in page, a `GET/POST /forgot` form behind it, and a
  `GET/POST /reset/{token}` page that the emailed link lands on. The API has the
  same pair: `POST /api/v1/auth/forgot` and `POST /api/v1/auth/reset`. Both are
  unauthenticated, necessarily, and both share the sign-in rate limit rather than
  getting one of their own.

  **It needs a mailer, and with none it refuses out loud instead of pretending.**
  `SMTP_HOST` unset is the shipped default. On such an instance the sign-in page
  draws no link at all, `/forgot` says the instance cannot send mail and names
  the operator's route, and the API answers `503`. Every other optional-mailer
  feature in this product degrades to a lesser behaviour; this one cannot,
  because the mail *is* the mechanism — being told to check an inbox nothing was
  sent to is worse than being told there is no reset here.

  **The form never says whether an address has an account.** Same page, same
  status, same body and the same argon2 cost whatever you type. The answer goes
  to the address: an address that cannot be recovered — no account, a suspended
  one, or one that signs in some other way — receives a message saying no link
  was created and pointing at the operator, which is what registration already
  does for an address that is taken. The cost is stated: this sends mail to
  addresses that never registered.

  **What a completed reset does, in one list.** The link works once and lapses
  after an hour; asking again replaces it. Every session on the account is
  signed out, including any you did not open, and every other outstanding reset
  link stops working. **API keys are not revoked** and keep working — a key is a
  separate credential with its own rotation, and taking them out would turn a
  recovery into an outage. No session is started: you sign in with the password
  you just set. The reset is written to the instance-wide audit log as
  `password.reset`, with the account as the actor and a network prefix rather
  than an address.

  A link that has been used, has lapsed, or names an account that cannot be
  recovered is answered `404` — the same answer for all of them, so the endpoint
  cannot be asked which.

- **This product now accepts a file.** One kind of file, for one thing: an image
  uploaded against a QR code, over `PUT /api/v1/links/{id}/qr/logo` for the
  link's default code and `PUT /api/v1/links/{id}/qr/codes/{slug}/logo` for a
  code named by its slug, with a `multipart/form-data` body, and removed with a `DELETE` at
  the same path. It needs `links.update` — the permission that already changes
  how a code is drawn — and an API key that holds it may use it. **The QR panel
  on a link's page does it too**, so a logo is not an API-only feature.

  **And it is drawn into the middle of the code** — in the panel, in the
  thumbnail, and in both downloads, so the picture on the page is the picture in
  the file. Nothing already printed changes: a code only gains a logo when
  somebody uploads one to it, and what the code *says* is untouched either way.

  **Two things change about a code the moment it carries one, and both are
  visible.** The image covers a centred square **three tenths of the code's
  width** — 9% of its area — and error correction goes to **level H**, the
  level that lets a reader recover a code with part of it covered. H packs the
  code tighter, so the picture is a little denser than it was and can come out
  larger at the same setting. The fraction is not a taste and not a guess: the
  arithmetic bounds it — at most three quarters of what level H can recover, the
  rest left for print, paper, lighting and camera angle — and the size inside
  that bound was chosen by decoding every combination this product can draw, at
  simulated distance, through two independent decoders. There is no control for
  the size or the position of the logo, because the derivation only holds for a
  centred square and a control this product cannot bound is one it will not
  offer.

  **`level` is therefore not yours to set while a code has a logo.** A request
  naming another one is accepted and answered with `H` rather than refused —
  `{}` means "the defaults" in this API, and refusing would fail a request that
  only changed a colour. The response and every later `GET` report what was
  applied. **Removing the logo returns the code to the rule below** rather than
  to a remembered level: the payload is unchanged, so a picture already on a
  poster still resolves, and holding `H` on a code with nothing covering it packs
  in modules it does not need. *(It stayed at `H` until this release.)*

  **Whether a logo'd code scans is measured, and the measurement is kept.**
  There is no QR decoder in this product and adding one would be a dependency,
  so the decoding lives in the repository's verification tooling instead:
  `make verify-scan` renders every symbol size this product produces, four logo
  shapes, and the smallest, default and largest stored size of each — and then
  the same range again with **no logo at all**, as a control, so that a picture
  that will not read can be told from a decoder that will not read it — then
  shrinks every picture to 8, 6, 4, 3 and 2 pixels per module, standing in for
  reading it from further away, and decodes all of it through two independent
  engines.
  That is what the three tenths above was chosen against, and it fails if the
  fraction grows past what still reads. **It is still a measurement and not a
  guarantee**: it is not part of the build, and no test that ships with this
  product will tell you if a logo'd code stops scanning on some particular
  reader. One older engine, ZBar, is stricter than both and loses some of the
  densest codes once the picture is shrunk to the furthest distance the check
  simulates — recorded rather than hidden, because the larger logo is what
  bought it. It reads every one of the codes with no logo, which is what says
  the losses are the logo's doing and not the engine's age.

  **PNG and JPEG, decided by what is in the file** rather than by what it is
  called or what your client says it is — so a `.png` holding a JPEG works and a
  file that is neither is refused whatever its extension. **An SVG is refused,
  and the refusal says why**: an SVG is a document that can carry script and
  fetch other files, and this product will not serve markup it did not write.

  **What is stored is a PNG this server encoded from your image, never the file
  you sent.** That is deliberate and has a consequence worth knowing: metadata
  does not survive, so this is not somewhere to keep an original.

  Three limits, each a number, each answering `422` and naming which one and what
  your image measured: the request body stops at 1,048,576 bytes, the image at
  1024 pixels a side, and the stored result at 1,060,000 bytes. **An image bigger
  than a stored logo holds is resized, not refused.** A stored logo is at most
  262,144 pixels in total; anything over that is scaled down to fit with its
  shape kept, and you are told what you uploaded and what was stored — in the
  panel, and in the API response as a `resampled` object that is there only when
  it happened. **Uploads have a rate limit of their own**,
  `LINKCTRL_UPLOAD_RATE_PER_MIN`, thirty a minute by default and applied on top
  of the API limit rather than instead of it.

  **Choosing a file uploads it.** There is no second button: the image applies
  the moment you pick it, and clicking the browse control leaves it looking
  pressed while the file dialog opens rather than looking like nothing happened.
  Reaching it by keyboard shows the ordinary focus ring instead, because a
  pressed look keyed on focus would stay on for anybody merely tabbing past.

  **For operators: the image lives in the database, in `qr_codes.logo`.** No
  volume to mount, no object store to run, nothing new in a backup procedure —
  and, in exchange, binary in the row and in every `pg_dump`, bounded at about a
  megabyte a code and 20 MiB for a link carrying the maximum twenty. Removing a
  code, a workspace or an organization removes its images with it. Deleting a
  *link* is a soft delete, so its images are cleared by the hourly maintenance
  pass rather than immediately.

  **The link's original code carries one too**, and it is reached the way
  `qr.svg` and `qr.png` reach it — at `…/qr/logo`, without naming a code. That
  shorthand answers for whichever code is the link's default.

- **More than one QR code per link, told apart in the analytics.** A print run
  and a shop-window card against the same short link used to be the same picture,
  and their scans were the same number. Each code now carries a name you choose
  and an identity it prints, so the breakdown on the link page shows you which
  one people actually scanned.

  **Every code already printed keeps counting exactly as it did.** A picture
  that carries no code identity — which is every copy of your original code
  printed before this release — is counted against whichever code is the link's
  **default**, and that starts as the code it always was. There is nothing to
  reprint and nothing to reconcile.

  **Any code can be removed, and any code can be made the default.** The default
  is the role that decides where those untagged pictures land. Removing the code
  that holds it hands the role to the oldest code left and tells you which one,
  because that is where your old posters start being counted; a link's last code
  cannot be removed, since a link always has one. Your original code gains a
  printed identity of its own at the moment you add a second code — the moment
  there is something to tell it apart from — and until then it carries the
  picture it always had.

  **Adding a code keeps every size exact, and says so on the one occasion it
  cannot.** A printed identity makes what your original code encodes a little
  longer, and a longer payload sometimes needs a bigger grid of squares. Both
  codes are re-measured against what they now encode: almost always the size you
  set is kept and only the squares behind it change, and there is nothing worth
  saying. When that size is too small to hold the bigger grid with a margin
  anything can read — which takes a code sitting at the very bottom of the size
  control — it is raised to the smallest size that does, and you are told, with
  both numbers. What is never allowed is a code that says one size and produces
  another.

  A link carries up to twenty codes. Twenty is the number that keeps the panel a
  list and the analytics a chart rather than a wall.

  What a code is not: it has no destination of its own, no expiry of its own and
  no gate of its own. Those belong to the link, which is what makes changing the
  link's destination change every printed code at once — the point of the product.
  A code that pointed somewhere else would be a second link.

  **Removing a code keeps what it already recorded, and stops it growing.** Scans
  from a picture printed with a code you have since removed are counted as the
  link's default code from then on, rather than being credited back to the code
  that is gone. That will look like an interrupted line to somebody reading a
  chart across the removal, so it is worth knowing before you remove one.

  Over the API: `GET`/`POST /api/v1/links/{id}/qr/codes`, and
  `GET`/`PUT`/`DELETE /api/v1/links/{id}/qr/codes/{slug}` with
  `.../{slug}/image.svg` and `.../{slug}/image.png` for the pictures. **The
  existing five `/qr` endpoints are unchanged and answer for the link's default
  code**, so nothing written against 0.2.0 has to move — with the caveat that the
  default is a role that can be moved, by `PUT .../{slug}/default`, and a client
  that wants a particular code rather than the role names its slug. `DELETE
  /api/v1/links/{id}/qr` restores the default code's style and no longer deletes
  its row: the row now carries the code's printed identity.

  The new identity is a second reserved query parameter, `qrc`, beside `src`.
  Like `src` it is **forwarded** to your destination when a link has query
  forwarding on, and like `src` it is not evidence of anything: anybody can type
  one. A value that is not one of that link's codes is counted as the link's
  default code and is never stored, which is what stops the parameter being a
  way for a stranger to write rows into your analytics.

- **Clicking a notification goes to what it is about, and marks it read.** In the
  bell and on `/notifications` alike. Until now an item was a sentence and
  nothing else — "an automation rule fired", with no way to reach the rule — and
  the destination is now derived from the notification's own kind and data: a
  filing opens the review queue at the row that is waiting, a firing opens
  `/automation`, a domain warning opens `/domains`, an accepted invitation opens
  `/invites`, and a dispute allowed on appeal opens `/links`, where the refused
  link can now be created.

  **Two kinds lead nowhere and say so** rather than sending you back to the list
  you were reading: an audit-growth warning, because the audit log has no
  dashboard page and what to do about it is a configuration variable; and a
  dispute whose refusal was upheld, because no page shows a refusal that stands.
  Those two render without anything to click. Every kind the code declares has an
  answer of one sort or the other, and a test reads the vocabulary out of the
  source rather than from a list, so a kind added later fails the build instead
  of quietly becoming unclickable.

  Opening one is a form submit rather than a link, because it changes state and a
  state change behind a `GET` is one any prefetch can fire.

- **A read notification can be marked unread**, which is the undo for having
  opened one by accident — and this release is what makes the accident common.
  `DELETE /api/v1/notifications/{id}/read` over the API. No schema change: unread
  has always meant the read timestamp is absent, so putting one back is removing
  a value.

- **`/links/{id}/qr` and `/disputes/reviewers`** — the two on-demand panels
  below, each also served as an ordinary page. Bookmark one, open it in a second
  tab, or share the URL.

- **A QR code can be downloaded as a PNG.** `GET /api/v1/links/{id}/qr.png`, and
  a PNG entry in the QR panel's download menu beside the SVG one. Until now the
  only file you could get was vector text, and turning it into something most
  programs open meant finding a converter. *(It was a worded **Download the PNG**
  button until the QR tab was relaid out later in this release; the file and the
  endpoint are unchanged.)*

  **It is the same picture as the SVG, and that is asserted rather than
  intended.** Both are drawn from one grid at one size with one offset, and a
  test walks every square of the code checking that the pixel at its centre in
  the PNG carries the colour the SVG's shapes put there. What is *not* claimed:
  that a browser's rendering of the SVG is byte-identical to the PNG. No two
  rasterisers agree to the byte, and the claim is about the squares.

  **Output stops at 2048 pixels.** The image is two colours at one byte a pixel,
  so the largest buffer a request can cause is 4,194,304 bytes; a size above the
  cap is refused rather than quietly shrunk. Nothing joined this program's
  dependency list for it — the encoder is Go's own `image/png`.

### Changed

- **A link's page shows one section at a time, behind tabs.** It was every
  section in one column — the edit form, the QR code, routing rules, the split
  test, signed links, analytics, recent activity and the danger zone, stacked —
  and finding anything below the fold meant scrolling past everything above it.
  A tab strip now selects one panel: Edit, QR, Routing, Split, Signed,
  Analytics and Danger, with recent activity folded into Analytics since it is
  the same data one row at a time. The strip scrolls sideways on narrow
  screens rather than wrapping. Each tab is a real URL (`?tab=`), so
  bookmarks, refresh and the back button keep working, and a save made in any
  section returns to the tab that section lives on. No section's behaviour,
  permissions or form fields changed.

- **A link's QR settings live on the QR tab, and the small code beside the
  heading opens it.** The popup that used to hold the style form and the
  downloads is gone: with the page behind tabs, everything it held is one
  click away on the QR tab, so the codes list, the full drawing, both
  downloads, the style form and the logo controls now render there directly.
  Clicking the thumbnail — from any tab — switches to the QR tab. Nothing
  else moved: `/links/{id}/qr` still serves the same contents as their own
  page for bookmarks and second tabs, every saved link and permission is
  unchanged, and the reviewer panel on the dispute queue keeps working as it
  did.

- **Every tab on a link's page now says what its section holds, on the tab
  itself.** Tabs answered where a section went and not what is configured in
  it; reading a link's setup meant opening seven panels in turn. Each tab now
  carries a badge: QR counts the link's codes, Routing its rules, and
  Analytics shows the clicks in the selected window. Split shows which kind of
  test is running — unequal shares for weighted, equal shares for sequential —
  and Signed shows a check when the link requires signed access, the one badge
  with colour. An empty section shows a muted `0` or a small cross rather than
  nothing, so every tab keeps its width and two links compare position by
  position; the cross always means the section is empty, never "off". The edit
  form and the danger zone carry no badge: editing is always available and
  deleting is a permission, so neither has state worth a chip.

- **The QR tab asks for less attention.** It carried the four explanatory
  paragraphs the owner named, six download buttons for two pictures, two save
  buttons for one row and a sentence stating a limit nothing counted against. Nothing on it was
  broken; all of it cost more reading than it returned.

  **A code's row is one click target.** Clicking anywhere in a row selects that
  code, where before only its name was clickable while the whole row painted as
  selectable. The controls at the end of the row are held off that area by a
  real gap, so reaching for a download no longer risks the remove beside it.

  **One download control per code, with a menu.** The two worded buttons on each
  row and the pair repeated below the picture — four controls for two files —
  are one icon that offers **PNG** and **SVG**; a format added later is an entry
  rather than a fifth button. **Remove is a `−`**, and both carry accessible
  names saying which code they act on.

  **Which code is the default is now visible without reading.** Every row
  carries a filled or empty icon instead of a *Make default* button on the rows
  that are not the default and nothing on the one that is. Exactly one is filled;
  clicking an empty one moves the fill and every icon in the list follows. The
  icons are drawn for anybody who may see the codes, not only for somebody who
  may change them — which code is the default is a fact about the link, and the
  button it replaced was the only thing about it a reader could not see.

  **One button saves.** *Rename* is gone and **Save** writes the name along with
  the style, so changing a colour and a name together is one press instead of
  two. **Restore defaults** is always drawn, disabled with a reason when there is
  nothing stored, rather than absent — a control you cannot find is not the same
  as one that has nothing to do. Nothing about what any of them writes changed,
  and `PUT /api/v1/links/{id}/qr/codes/{slug}` still replaces a code's label and
  style exactly as it did.

  **A `N/20` counter sits above the list**, and the sentences that carried the
  limit in prose are gone. The URL printed under the picture is gone with them:
  it is the link's own short URL, which the page already states twice.

  All of the tab's prose — those four paragraphs and the three shorter ones
  beside them — went from about 1900 characters to under 900, measured rather
  than judged, and a test holds it there; by the end of this release a later
  read of the same tab takes it under 300. What survives is what a reader cannot
  work out from the control beside it, which by then is two sentences: keep a
  code's two colours far apart, because a light code on a dark field is refused
  by many readers, which is why the picture paints its own background whatever
  the page theme is; and restyling never changes what the code says, so a code
  already in print still resolves to the same URL after its colours move.

- **The QR tab, after a third read of it: the list holds still, the size control
  tells the truth while you drag it, and a save keeps your place.**

  **The codes list is alphabetical by name** and stays put. It used to lead with
  whichever code was the default, so choosing a different one re-ordered the
  list under you and the change read as nothing having happened. Which code is
  the default is the filled dot on its row — that is what says it now, and the
  list does not move. The same order comes back from
  `GET /api/v1/links/{id}/qr/codes`, where it was default-first before.

  **Dragging the size slider moves the number beside it, and typing a number
  moves the slider.** The two are one setting and until now neither followed the
  other, so the box said one thing while the slider was about to save another.
  This is the first piece of interactive JavaScript this dashboard carries: a
  single file this server serves, with the Content-Security-Policy unchanged and
  nothing added to the build. **With scripts off nothing is lost but the live
  echo** — both inputs still carry their values, the form still saves, and a size
  outside the range is still refused with a sentence rather than quietly
  clamped.

  **Saving no longer throws you back to the top of the page.** Every write on the
  tab now returns to the position you were reading at, and you are never shown a
  different one on the way. A browser cannot be scrolled before it has laid the
  page out, so what happens instead is that the page is held back for the moment
  between the two: on the load after a save, and on no other load anywhere in the
  product, the page appears at your position rather than appearing at the top and
  moving. *(Two earlier attempts in this release got you to the right place while
  still showing you the wrong one first — the second only on a connection slower
  than a local one, which is why it took a report from outside to see it.)*

  **The remove button stays on a link's only code, grayed out**, with *Every link
  must have at least 1 QR code.* on it — where before it simply was not there and
  a sentence under the list explained why. **Adding a code is a `+` beside the
  count**, opening a small prompt with a name field, in place of the label, box
  and button that stood under the list; at twenty of twenty it grays out with
  its reason instead of disappearing.

  **The default control's tooltip is the page's own**, shown anywhere over the
  button rather than only over the twelve-pixel glyph inside it, and shown to a
  keyboard as well as to a pointer. It reads **Default QR Code** and **Make
  Default QR Code**. The `+` button and the grayed-out remove button carry the
  same kind of tooltip, which is what lets a disabled control explain itself at
  all — the browser's own tooltips never appear on one. The download button and
  a remove button that is not grayed out are unchanged: their tooltip is still
  the browser's, inside the glyph. Hovering a download or remove button on the
  row you have selected now shows, which it did not: the highlight was the same
  colour as the selected row itself.

  **The logo upload has moved into the style form**, between the two colour
  pickers and the size control, with the **Remove the logo** button beside it, so
  that everything which writes a logo is in one place and in the order the rest
  of the form reads. Its own heading and panel are gone. Nothing about what a
  logo does changed, only where its controls sit — the file is still sent on its
  own, the moment you choose it, and **Save** still posts exactly the fields it
  posted before.

  **The size slider draws a mark at each size it stops at.** The sizes were
  already named in the control and no browser was drawing them, because the
  slider is themed and the marks come with the appearance the theme replaced.
  They are drawn now, and only the sizes the code in front of you can actually
  take: a dense code needs more pixels before its quiet zone reads, so its lowest
  marks are not there to be offered.

  **Five explanatory paragraphs went with all of this**, and the tab's prose is
  now under 300 characters against 900 before and about 1900 two releases ago.
  The last three to go: the note that scans appear under `qr` in the referrers
  breakdown, which the Analytics tab shows and `docs/usage.md` states; the
  sentence on the default code's row explaining that untagged scans are counted
  against it, which the API reference and `docs/usage.md` still state and which
  the filled dot and its **Default QR Code** tooltip already identify; and the
  logo's size limits, which an upload that exceeds them reports with your own
  image's dimensions in the message.

  **Picking a code from the list no longer leaves the page.** It opened the QR
  settings at their own URL — a page with no link heading row, arriving at the
  top — so selecting a code cost you the small preview beside the link's name and
  your place on the tab in the same click. The tab is redrawn where you are
  standing now: nothing scrolls, the preview stays, and the address bar still
  names the code, so a refresh, a bookmark and the back button all work as they
  did. A save on the code you picked comes back to that code rather than to the
  link's default. It is still an ordinary link, so with scripts off it loads the
  link's page on that code instead of redrawing part of it; `/links/{id}/qr` is
  unchanged and still serves the settings as their own page for anybody holding
  that URL.

  **Not done, and stated rather than left to be noticed**: there is still no
  automatic warning when a code's two colours are too close together. The
  advisory sentence stays until this product has a contrast measure it can
  defend, which is a choice rather than a control to draw.

- **The header's workspace label and workspace switcher read as one control.**
  They were two adjacent fragments — a name, then an unlabelled dropdown beside
  it — and nothing said the two were one claim. They now share a single
  bordered container: the current organization and workspace, a hairline
  divider, and the switcher, whose closed face is a chevron alone. The chevron
  opens the dashboard's own menu — hanging off the control it belongs to,
  styled like the rest of the header, listing exactly the workspaces you can
  move to with no blank row — rather than the browser's native dropdown, which
  ignored the control's position, opened on an unselectable empty row, and
  flashed the chosen name into the closed face while the switch was landing.
  With one membership the box holds the name by itself — no divider, no dead
  control — and an account with no workspace gets no box at all. Nothing
  behavioural changed: switching is still one action, still returns you to the
  page you were on, and the list still never offers the workspace you are
  already in. The switcher's accessible name is unchanged — a screen reader
  hears "Switch workspace" on the button, then a menu of workspaces, each a
  real button.

- **A link's QR code stops being a section and becomes a panel.** What is on the
  page is a small rendered code beside the link's name, at the top; clicking it —
  or **Settings and download** in the QR section further down — opens the full
  code, the style form and the download over the page you are on. An owner given
  the task of retrieving a QR code spent about twenty-six seconds finding it, and
  the note asked for exactly this shape. *(Superseded later in this release: with
  the page behind tabs there is no popup and no QR section further down. The
  thumbnail beside the link's name switches to the QR tab, which holds the full
  code, the style form and the downloads directly.)*

  **The download control keeps its text and gains an icon.** The note named "the
  download button being text instead of an icon"; an unlabelled icon is a guess
  for anybody who does not already know what it does, so it is both. *(Superseded
  later in this release: there is one download control per code now, an icon with
  a menu offering PNG and SVG, and what tells you what it does is its accessible
  name rather than a word beside it.)*

- **The QR control is a slider with a number beside it, and asks how big you
  want the code in pixels.** It used to ask for a quiet zone in modules and a
  module size in pixels, which is two numbers nobody printing a poster knows.
  Both are still the arithmetic behind the one control. The slider stops at 128,
  256, 300, 512, 600, 1024, 1200 and 2048 — powers of two plus the three that
  matter at 300dpi — and slides freely between the ends; the box beside it takes
  a typed number for anything the marks do not cover.

  **The size you set is the size you get, exactly.** Ask for 500 pixels and the
  file is 500 pixels. A code is a grid of squares and an arbitrary pixel size
  does not divide into it, so something has to absorb the remainder: it is the
  empty margin around the code, which is white space and can be any number of
  pixels. The squares themselves stay whole, which is what keeps the SVG and the
  PNG the same picture.

  **The margin aims at four squares and never goes under three.** Four is what
  the specification asks for; three is the low end of a quarter either side of
  it, and it is measured rather than assumed — every size and version this
  product draws is decoded at five simulated viewing distances through two
  independent decoders before the number is allowed to stand. On a large code in
  a small picture there may be no scale that lands in that band at all, and the
  margin then comes out *wider* than five rather than narrower than three: extra
  white costs nothing to read, and a thin margin costs a scan. A size too small
  for the code to have any margin at all is refused, with the smallest size that
  code can be drawn at in the message.

  **Error correction moved to the API.** It is a tradeoff between how much damage
  a printed code survives and how tightly it packs, and there is no way to judge
  it from a dashboard; `PUT /api/v1/links/{id}/qr` still sets it, and saving the
  form afterwards keeps whatever it was set to rather than resetting it.

  **And every code now takes the strongest level it can get for free.** A QR
  symbol steps between sizes of grid, and correction below the next step costs
  nothing — so a code is drawn at the strongest level that does not make the grid
  any bigger, which for an ordinary short URL is `Q` where the default of `M`
  bought a level of damage tolerance less at **exactly the same density**. The
  picture is the same size to the pixel and scans from the same distance; what
  changes is how much of it can be smudged, torn or covered and still read.
  `GET /api/v1/links/{id}/qr` reports the level a code is drawn at, as it always
  did. No picture changes size, with one bounded exception: a style that named
  `L` was fitted against `L`'s smaller symbol, and `L` is now a floor rather
  than a choice, so such a code draws about a tenth larger than the size stored
  on it. Storing a size is itself new in this release, so no instance upgrading
  from `v0.2.0` can hold that pair.

  **The `level` you set is a floor rather than an instruction**: it is honoured
  upward and ignored downward. Asking for `H` still gets `H`, at whatever grid
  size that costs — that is how a logo works. Asking for `L`, or for `M` on a
  code where `Q` is free, gets the free level instead, because there is no
  saving to be had from less correction at the same density. `L` is accepted and
  is now a level nothing draws. Nothing was migrated and no stored style was
  rewritten: the rule reaches a row that names `M` exactly as it reaches one that
  names nothing.

  **The preview keeps its own size.** The frame beside the form is a fixed
  square, and a code bigger than it is drawn scaled down to fit, so setting
  2048px changes the file rather than the page. The size itself is in the
  control below it — in the box and on the slider — which is where somebody
  setting it is looking.

  **The reset button says *Restore defaults*.** It used to say *Back to black on
  white*, which named the colours — it clears the size too. *(It is drawn whether
  or not a style is stored since the QR tab was relaid out later in this release;
  with nothing stored it is disabled and says why.)*

  **Codes styled before this release are untouched.** Their stored settings are
  read forward to the size they already produced, so nothing anybody has printed
  changed shape. Re-saving one stores the size it was already drawing, to the
  pixel, whatever margin it was written with.

  **`PUT /api/v1/links/{id}/qr` gains `style.size`**, a size in pixels, and it is
  what the form writes. Give it and the picture is exactly that across, with the
  code centred and the margin taking the remainder; leave it out and `margin` and
  `scale` decide the size the way they always did. A `size` the code will not fit
  inside — smaller than the symbol at your `scale` plus a margin anything can
  read — is **refused**, the way the form refuses it, rather than quietly served
  at some other size; the message names the smallest size that works at that
  `scale` and the smallest that works at any. `scale` accepts up to 75,
  where it stopped at 32 and then 68 — the ceiling is the 2048px raster bound
  divided by the smallest code plus the narrowest margin, so every size the form
  can resolve to is a style the API accepts. A large `scale` with a large
  `margin` still draws past 2048px, and the image endpoints refuse that as they
  always have.

- **Managing dispute reviewers moves off the review queue.** The queue still says
  who reviews it — that is context for a page whose decisions are instance-wide —
  and appointing or withdrawing somebody is behind **Change who reviews**.
  Permissions are unchanged: the roster is `instance.admin` in both places, and
  the queue is `destinations.review` as it was.

  Both panels are the same mechanism, and its defining property is that the
  contents are a route first: the popup is what the browser does with the same
  markup when it can, and a browser too old for it renders the panel inline
  rather than hiding it. No modal library, no CDN and no new script — the
  dashboard's stylesheet and content-security policy are unchanged.


- **The dashboard shell says where you are, at every membership count.** The
  header now names the current organization and workspace on every page. With one
  membership — which is every account that has not accepted an invitation — the
  shell previously named neither: the workspace switcher is drawn only when there
  is somewhere to switch to, and the workspace's name appeared nowhere else. An
  owner given the task *"confirm which workspace you are in"* could not complete
  it.

  The switcher itself now offers **the workspaces you can move to and not the one
  you are already in**, with the current workspace named beside it instead. It
  still does not appear at all with a single membership: a dropdown with one
  entry is a control that cannot do anything, and what fills that gap is a label.

- **API keys moved from the top-level navigation into the identity menu.** Two
  destinations remain up there, Dashboard and Links. Nothing changed about who
  can reach `/keys` or what it does; a key is minted once and then not thought
  about, which is the same reason Members, Domains, Webhooks and Automation are
  in that menu, and the first place an owner looked for it.

- **Campaigns and Folders are navigation.** They were two text links in the
  corner of the links page and are now a bar under the header, on every page of
  the links area.

- **The links list stops trapping the first click.** The search box is the first
  control on the page. The *Create a link* form sat above it, so the first text
  box on a page whose subject is a list was the one that made a new link — it is
  now a panel one click away, directly under the filters, that opens on its own
  when a creation was refused so the reason and what you typed are still there.

  Five of the six filter controls — status, folder, campaign, hostname and sort —
  are behind one **Filters** panel, which opens by itself whenever any of them is
  set. Search stays on the page. **No route, form field or query parameter
  changed**, and the no-JavaScript submit still applies every filter at once.

- **A link's page opens on the link, not on its analytics.** The destination and
  the alias are the first two things you can type into, in view without
  scrolling: at 1280×800 the destination box's top edge is 327px from the top of
  the window, where it used to be 1883px — a screen and a half down, behind three
  statistic tiles, a chart, a world map and six breakdowns. Changing where a link
  points took about thirty-five seconds in a blind task, and the note was
  *"scrolling, which is worse when not looking for the massive QR code"*.

  The eight sections are re-ordered by how often somebody needs them: edit, QR
  code, routing rules, split test, signed links, analytics, recent activity,
  danger zone. Nothing was removed and no section changed what it does — the
  analytics are the same analytics, further down the same page, at the same URL.
  **No route, form field or query parameter changed.** *(Superseded later in
  this release: the one scrolling column described here is a tab strip, and the
  eight sections are seven tabs with recent activity folded into Analytics. The
  order above is the order the tabs are in; what changed is that you no longer
  scroll past one section to reach the next.)*

- **The click limit says what it is a limit on.** It read *Click limit (empty =
  none)* with *"416 used so far"* in a separate line beneath it, and an owner
  setting a limit could not tell whether the box wanted the extra clicks or the
  total. It wanted the total. It now says so in one sentence, naming both
  numbers: *"A total and not an allowance on top of what has gone before: 416 of
  the 466 are already spent, and past the limit the link answers 410."* The gate
  is unchanged — a limit has always been absolute, and making it anything else
  would silently redefine every limit already set.

### Fixed

- **The analytics world map drew two grey bands across its full width** — one
  along the top, one just below the equator. Fiji and Russia cross the
  antimeridian, and the map generator emitted each of their outlines as a
  single ring wrapping from one edge of the frame to the other, so the fill
  swept the map at that latitude. The generator now splits any
  antimeridian-crossing ring at ±180° before projecting; Fiji, Russia and
  Wrangel Island render as their shapes, every other country's geometry is
  unchanged, and a Russia with clicks shades its own outline rather than a
  stripe through Scandinavia. The bands were faint at typical laptop widths
  and obvious on wide screens, which is how they went unnoticed.

- **Every dashboard page loaded with a Content-Security-Policy violation in the
  browser console.** htmx injects a stylesheet for its request-indicator feature
  at load, and the dashboard's `style-src 'self'` blocked it — on every page, in
  every browser. Nothing visible was wrong, because no page uses that feature;
  the costs were a console that could never be clean, and that the first
  template to adopt `hx-indicator` would have gotten a loading state that
  silently did nothing. The layout now tells htmx not to inject
  (`includeIndicatorStyles: false`); a template that starts using indicators
  ships the rules in the stylesheet instead. The policy itself is unchanged.

- **A link's country map and country list now follow the data instead of the
  GeoIP setting.** An instance that had accumulated country history and then
  removed — or had never configured — a GeoIP database was shown *"Geographic
  data is unavailable: no GeoIP database is configured"* over rows that were
  present and correct, on both surfaces, because each was suppressed on the
  setting rather than on whether anything had resolved. 0.2.0's note that the
  map is not drawn at all without a database was true of 0.2.0 and is not true
  now.

  The sentence is reached only when nothing resolved **for that link, in the
  window on screen** and nothing can resolve. The test is per link and per
  window rather than per instance: a link whose countries all fall outside the
  selected window meets the sentence until the window is widened. Configuration
  still counts on its own, so a database with no clicks yet keeps the ordinary
  *no data yet* rather than a claim about the instance. **A geographic routing rule is deliberately
  unchanged**: whether one can ever match is a genuine question about the
  database, and the rule form still says so.

- **An unreachable mail relay no longer keeps the whole server dark at every
  boot.** With `LINKCTRL_SMTP_HOST` set to something that does not answer, the
  reachability probe ran before the HTTP listener bound, so nothing was
  served — redirects included — for the whole of `LINKCTRL_SMTP_TIMEOUT`.
  Measured at **10.05 seconds** on the shipped default; at a raised timeout the
  container never became healthy at all and `docker compose up --wait` gave up.
  The code's own comment said "the relay being down is not a reason for a link
  shortener to stop serving redirects", and it was not true.

  The probe now runs in a goroutine. Nothing between it and the listener ever
  read its result, and the outbox retries regardless of what it finds, so
  waiting bought only the order of two log lines: `smtp relay reachable` now
  arrives after `http server listening` rather than before it. A rolling deploy
  is where this bit hardest — every new replica was unready for the timeout at
  every start, and a health check with a shorter start period failed it
  outright.

- **No dashboard page scrolls sideways on a phone.** At 360px, sixteen of the
  twenty-three pages did: the header could not fit the workspace switcher on one
  line and dragged every page with it, and six tables were as wide as their
  columns with nothing to scroll them. The header is two lines below the `sm`
  breakpoint and every table now scrolls inside its own box. Measured in Chromium
  at 360, 640 and 1280px, before and after, and held by a test that renders every
  page and fails any `<table>` or `<pre>` that nothing scrolls.

- **Erasing an account now reaches four places it did not.** The hourly pass
  already replaced the actor's name everywhere it was snapshotted. It now also
  removes the address from the **detail** of audit records that name the erased
  person as the subject of somebody else's action — the invitation lifecycle, the
  three membership actions and the two instance-level ones — from the **list** of
  outgoing principals on an instance-principal move, which is the one record that
  stored addresses as an array rather than a single value; blanks the address on
  the invitations that account **redeemed**; and clears it from the
  **notification the inviter received** when the account accepted their
  invitation, in the sentence as well as in the detail. The invitation one
  mattered most: `/invites` lists every invitation an organization ever issued,
  redeemed included, so an account deleted, erased and tombstoned everywhere else
  was still named in full on an ordinary dashboard page, behind no special
  permission and expired by no setting. The notification is the same shape one
  table over — it belongs to the person who sent the invitation, so removing the
  erased account's own notifications never touched it, and nothing expires it. An
  **outstanding** invitation to the same address is deliberately left alone — it
  is an offer to an address, which became reusable the moment the account was
  deleted.

- **A key cut out of an organization is no longer told about it, and its owner
  now is.** Revoking an account-wide key's reach into one organization stopped it
  *acting* there and not *reading* about it: `GET /api/v1/workspaces` went on
  listing that organization's name, slug and workspace ids to the very credential
  it had been barred from. It no longer does. And the key's owner, who could
  previously watch a credential stop working in one tenant with no page saying
  why — the audit record explaining it is written in an organization they may not
  be able to read — now sees which organizations a key has been cut out of, on
  their own key list. **The API responses for a key gained
  `revoked_organizations`**, which is additive.

- **A key cut out of an organization can no longer rotate its way back in.**
  Reach revocations were keyed to the key's id and nothing carried them to a
  successor, so the holder of a barred credential could call the rotation
  endpoint — which authenticates with the key's own token and needs nobody signed
  in — and get a replacement that reached the tenant they had been cut out of,
  acting and reading. Bars now travel with the rotation, inside the same
  transaction, keeping the date and the administrator who set them rather than
  being restamped by whoever rotated. The rotation's own response reports them —
  `revoked_organizations` on `POST /api/v1/api-keys/rotate` is the successor's
  bars, not an empty array — so a caller learns what its new credential cannot
  reach from the call that minted it. **This was found while fixing the two rows
  above and was not one of them**; it is named here because an administrator who
  cut a key out before upgrading was relying on something that did not hold.

- **The domains page no longer says a hostname is verified and unverified in one
  response.** Pressing *Check DNS* on a domain inside its grace window answered
  *"is not verified"* directly above a badge reading *"Verified — links are
  served here"*. Both were describing something real — the check had failed and
  the hostname was still being served — and the page now says that instead: the
  check failed, links are still served, and here is when they stop. A second,
  narrower case is gone with it: between grace expiry and the next hourly pass
  the page read *"stop being served at"* a time in the past.

- **A verification recorded once is recorded once.** Two replicas running the
  domains pass at the same moment could both write a `domain.verified` audit
  record for one verification. Reproduced five runs of five, now zero. The same
  was true of the *Verify* button on the Domains page — two administrators
  pressing it at once, or one double-click — and both paths now decide from what
  the write returned rather than from a row read a DNS round trip earlier. The
  hostname was always correctly verified; what was wrong was the count.

- **Every bar chart said its peak was the top of its axis.** The dashboard read
  *peak 5,000/day* on a series whose true maximum was 2,351 — the axis is rounded
  up to a readable number, and that rounded number was being presented as an
  observation. It now reports the reading. The gridlines also carry their values
  now, which is what the space under every chart in this product has been
  reserved for since it was written.

- **Granting somebody a role they already outrank says so.** The confirmation
  read *"Access granted. It adds to whatever they already had"* even when the
  added role's permissions were a subset of what the person already held, so
  nothing was added. The grant still happens — an org editor who is also a
  workspace admin is a real and useful arrangement — but the message now
  distinguishes the two, and the form warns before you press it rather than
  after.

- **Neither form that hands out access starts on the most powerful role.** The
  invitation form and *give somebody access* both rendered their roles strongest
  first with none marked, so a browser selected the first one: filling in an
  address and pressing the button, without touching the select, sent an **owner**
  invitation. Both now start on the lowest role. Nothing about what an actor may
  offer has changed; the least deliberate path through the form is no longer the
  most powerful one.

- **The password-reset page no longer promises mail it is about to refuse.** On
  an instance with no relay it read *"We send a link to your address"* directly
  above *"This instance cannot send mail"*.

- **A refused QR style leaves you where you were typing.** Opening the QR panel
  at its own address and entering a size out of range answered with the link page
  instead of the panel, so the reader had to find their way back to the form they
  were using. The refusal is unchanged — same message, same status, nothing
  stored — and it now arrives on the page it was typed on.

- **Uploading a logo no longer breaks a code's PNG download.** A logo forces
  error correction to level H, which makes the symbol bigger; the stored size was
  carried over unchanged, so a code already near the raster ceiling was pushed
  past it and its PNG started refusing. The size is now re-fitted to the larger
  symbol, so the picture stays exactly the size you asked for — unless the bigger
  symbol will not fit inside that size at all, and then the code grows rather
  than being drawn with a margin nothing can read. The payload does not change,
  so a code already printed still scans.

- **`/account/mfa` no longer scrolls sideways on a phone.** The enrolment QR was
  emitted at a fixed pixel width and overflowed a 360px viewport by 174px — the
  one dashboard page that still did.

### Notes for operators

- **One new secret, and it is optional.** `LINKCTRL_MFA_SECRET_KEY`, at least 32
  bytes, encrypts each account's TOTP secret at rest. **Unset means this instance
  has no second factor at all** — nobody can enrol and the account page does not
  offer it, which is exactly what every instance before this release was, so
  omitting it is a supported configuration rather than a broken one. It is not
  the API-key pepper and must not be the same value. Losing it locks every
  enrolled account out of its authenticator and no further: recovery codes are
  SHA-256 hashes and this key is not involved in them, so the route back is a
  recovery code, then turn the second factor off and enrol again.
- **The update check is on by default, and it does nothing until somebody is
  asked and says yes.** A fresh instance is asked on the setup form. **An
  instance upgrading into 0.3.0 has no first run to be asked at, so it is asked
  on the dashboard at the first sign-in by an account holding `instance.admin`,
  once.** Until that is answered no request is made. `LINKCTRL_UPDATE_CHECK=false`
  refuses it outright and wins over the answer; so does never answering. What
  leaves is a daily `GET` to GitHub's releases API carrying this server's source
  address and the running version in the `User-Agent` and nothing else. This is
  the fifth thing that leaves a LinkCtrl instance and `docs/SECURITY.md`
  enumerates all five.
- **An API key with no pin is now account-wide, and that is a behaviour change
  for one request shape.** `api_keys.organization_id` becomes nullable: NULL is
  account-wide, non-NULL is pinned. **A caller sending `org_wide: true` and
  nothing else got an organization-scoped key in 0.2.0 and gets an account-wide
  one in 0.3.0.** No issued key changed reach — dropping NOT NULL writes no rows,
  and every key that exists today keeps the organization it was issued for. An
  administrator can now cut *their own* organization out of somebody's
  account-wide key without destroying a credential that is not theirs; that is a
  separate action from a revoke, separately audited.
- **Deleting an account is immediate; erasing it takes up to an hour.** Access
  ends inside the deleting transaction. The hourly housekeeping pass then scrubs
  the identifying fields and **keeps the row**, so foreign keys and audit records
  go on pointing at something and the actor's name becomes a constant tombstone.
  The surviving `user_id` is pseudonymous rather than absent. Logged as
  `deleted accounts erased` with a count and no identifier; restarting the app
  runs the pass immediately rather than waiting.
- **Account recovery needs `SMTP_HOST` and says so out loud.** With no relay a
  reset request answers `503 no-mailer` from the API and states the operator's
  route on the page. It does not queue and there is nothing else to switch on.
- **`/readyz` is now a contract you can configure a load balancer against.**
  `503` means take this replica out of rotation, `200` means keep it, and
  `degraded` is a `200` — the word is diagnostic and the code is the instruction.
  If you run several replicas, `LINKCTRL_SHUTDOWN_DRAIN_DELAY` (default `5s`)
  must outlast your balancer's check interval × threshold, and it is the number
  that decides whether a deploy costs nothing or costs retries.
- **A single container is still tested on every push.** Nothing in the
  high-availability work is required to run one, and `make single-instance`
  drives the whole surface on Postgres alone to keep that true.
- **Eight additive migrations**, run at boot as usual: several QR codes per link,
  the QR logo column, password-reset tokens, account erasure, the second factor,
  the API key's account reach, the instance's update-check setting, and the QR
  default-code flag. No destructive step and no new permission. **One of them
  writes data**: the default-code flag is set on the row that already held that
  role, links carrying more than one code have their default named, and a link
  with codes but no row for its default gains one at the style it was already
  being drawn at. Nothing already printed changes what it counts as, and no
  recorded click is touched.
- **`LINKCTRL_UPLOAD_RATE_PER_MIN`** (default 30) is a new bucket for the one
  endpoint that now accepts a file. It is *on top of* `API_RATE_PER_MIN` rather
  than instead of it, and shared through Redis like the others.
- **One log line changed order.** The SMTP relay probe moved into a goroutine, so
  `smtp relay reachable` now arrives after `http server listening` rather than
  before it. Nothing between the two ever read its result; what this buys is that
  an unreachable relay no longer keeps the whole server dark for the length of
  `SMTP_TIMEOUT` at every boot.

## [0.2.0] - 2026-08-06

### Added

- **`lctl instance principal` — see who administers this instance, and hand that
  over when the account holding it cannot be reached.** The principal is
  conferred once, on the account that completes `/setup`, and deliberately by
  nothing else: an operation that could confer it would let one administrator
  mint another, and the whole point of the permission is that the set of people
  who may appoint dispute reviewers cannot grow. That left one situation with no
  answer at all — the founding colleague has left, or its password is lost and
  this product has no password reset — and the only route was editing the
  database by hand.

  ```sh
  $ lctl instance principal show
  $ lctl instance principal move --to you@example.com
  ```

  **It moves the principal and cannot add one.** Exactly one account holds it
  afterwards, checked before the change commits, so the bound survives the
  repair; reviewers already appointed keep the dispute queue, and nothing inside
  any organization changes. It refuses under `APP_ENV=production` without
  `--force`, as `lctl seed` and `lctl demo` do, and it writes an instance-wide
  audit record with the actor recorded as `system` — nobody signed in to make it.

  Anybody who can run this could already have written to the database directly,
  which is why it is a command rather than a page: the authority is filesystem
  access to the box, the same claim `/setup` itself rests on. What is new is that
  the change is recorded and cannot leave two principals behind.

- **An instance-level principal: somebody who administers the box rather than a
  tenant in it.** Until now every instance-wide reach in this product was guarded
  by a permission granted to the **owner** role — and `owner` is
  per-organization, with registration provisioning every self-registered account
  an organization it owns. On an instance running `LINKCTRL_SIGNUP_MODE=open`,
  that made the moderation of every organization's destinations one registration
  away.

  **The account that claimed the instance through `POST /api/v1/auth/setup` is
  now the principal.** It reads the dispute queue, decides what is in it, and
  reads the audit log of acts that belong to no organization. It may confer
  instance-level review on other accounts — at `/disputes`, or through
  `POST /api/v1/instance/reviewers` — and **somebody it appoints cannot appoint
  anybody else**, so the set of people who may delegate cannot grow.

  Its reach is enumerated and nothing inherits from it. The dispute queue, the
  blocklist entries those decisions lift, and the instance-wide audit surface;
  nothing else. `domains.write` is unchanged and still reaches every
  organization's owner and admin.

  **On upgrade the migration moves the grant rather than duplicating it.**
  `destinations.review` is removed from the owner role, and the instance
  permissions are conferred on the earliest surviving account — which on any
  instance that went through setup is the setup account. Nobody gains reach; the
  set of people holding it shrinks from "every owner on the instance" to one. If
  that account is no longer the operator's, see `docs/operations.md` for how to
  move it.

- **`GET /api/v1/instance/audit`**, the audit log of acts that belong to no
  organization, behind the new `audit.read.instance`. The default domain's root
  redirect and bot policy, every dispute decision, and every change to who
  reviews them all govern every organization on the instance — and each was
  previously filed under whichever organization the person happened to be acting
  in, where the tenants it changed could not see it. Those records are no longer
  returned by `GET /api/v1/audit`; they are here instead. Records written by
  earlier versions keep the organization they were written with.

- **An API key can now replace itself, unattended.** `POST /api/v1/api-keys/rotate`,
  sent with the key's own token, returns a successor and the only copy of its
  secret that will ever exist. There is no id in the URL because there is no other
  key it could rotate: the credential in the `Authorization` header is the one
  being replaced. A signed-in session gets a `403` — a session that wants another
  key mints one, as it always could.

  **The successor is identical or narrower.** Its scopes are this key's, or a
  subset you name; a scope the key does not already hold is refused rather than
  quietly dropped. The workspace binding is copied unchanged. Nothing widens, and
  `apikeys.read`/`apikeys.write` remain permissions no key can ever hold — which
  is what makes it safe to leave rotation in a credential's hands at all. The
  successor inherits the predecessor's *lifetime*: a key created to live 30 days
  rotates into another that lives 30 days, and a key with no expiry rotates into
  one with none.

  **The old key keeps working for a bounded window and then stops.** An hour by
  default, adjustable per rotation between five minutes and a day via
  `grace_seconds`, so a rolling deployment can hold both. When the window closes
  the old key is refused on every replica immediately — the check is part of
  authenticating, not a scheduled job — and housekeeping then marks it revoked so
  the key list agrees. **A key rotates once**; asking again returns `409` rather
  than minting a second successor. Every rotation is an audit event naming the
  prefix it came from and the prefix it became.

  **What this costs, stated plainly: a leaked key can survive a rotation.**
  Whoever holds the secret can rotate it, so revoking the key you know about may
  leave somebody holding a successor you never issued. Every generation shows up
  in your key list with its own prefix, every rotation is in the audit log, and
  each old key's window is capped — that bounds it; it does not remove it. If you
  believe a key has leaked, check the list for keys you did not create before you
  revoke, and rotate nothing.

  One more thing worth knowing when you use rotation: `last_used_at` is written on
  a 30-second cadence, so a key that reads as idle may have been used up to 30
  seconds ago. That is why the grace window cannot be set below five minutes.

  Nothing needs to be done on upgrade. Existing keys are unaffected until
  something rotates one.

- **API keys can now be issued for a whole organization instead of one
  workspace.** The *Reach* choice on the key form, `"org_wide": true` on
  `POST /api/v1/api-keys`, or `--org-wide` on `lctl apikey create`. A key issued
  this way follows its owner into every workspace of the organization instead of
  acting only where it was created.

  **The default has not changed**, and every key you already have is still bound
  to one workspace. The wider option needs `apikeys.write` through an
  *organization-wide* membership: a role you hold in a single workspace issues
  keys for that workspace, which is the same rule that already governs re-roling
  members. The key list, `lctl apikey list` and the API all report which of the
  two a key is.

- **Automation rules: standing instructions the scheduler carries out.** Write a
  rule at `/automation` or through `/api/v1/automation` — *when this happens in
  this workspace, do these things*. Three triggers: a link reaching its expiry, a
  link's click budget running out, and somebody in the workspace being refused a
  destination. Three actions: an in-app notification to the organization's owners,
  an `automation.fired` webhook to whoever subscribed to it, and archiving the
  links that matched. A rule may hold at most three actions and a workspace at
  most twenty rules.

  **Evaluation runs on the scheduler under the leader lock and never on a
  request.** There is no endpoint that evaluates a rule and no button that does;
  a link write, a redirect and an API call all leave rules alone until the next
  scheduler tick, which is every minute. That is enforced by the import graph
  rather than promised: no package on a request path can reach the evaluator.

  **A rule acts on what happens after it exists.** Creating one arms it at that
  instant and resuming a paused one re-arms it, so a rule written this afternoon
  does not fire for every link that expired last year, and a rule paused for a
  month does not deliver a month of backlog when you switch it back on.
  `last_fired_at` is what does this: it is the point a rule has already seen up
  to, so a rule sees each subject exactly once and cannot re-fire on its own
  effect. The page calls it "last fired or armed", because it is both.

  **A rule cannot set another rule off.** No action produces anything any trigger
  watches for, and that is asserted rather than assumed — which is why the webhook
  action always emits `automation.fired` rather than an event you choose, and why
  archiving a link never touches its expiry.

  A threshold is available on every trigger: *fire after N*, with nothing
  discarded while the count is unmet. One run considers at most 100 rules and 25
  subjects per rule, logs when either cap bites, and defers the remainder to the
  next run rather than dropping it; `/api/v1/automation` reports all of it.

  **Which 100 rotates.** An instance may hold more enabled rules than one run
  considers, and the run takes the ones it has looked at least recently — not the
  ones that fired least recently, which is a different set and one that does not
  drain: a rule whose trigger has not matched has not fired, so ordering on
  firings would park every idle rule at the front and leave anything past the
  hundredth unevaluated for good. The scheduler keeps its own timestamp for this,
  separate from the `last_fired_at` a rule shows you.

  Every rule change is an audit event, and so is every firing — the firing record
  names the rule rather than a person, which is what makes an automated archive
  answerable. `linkctrl_automation_firings_total{trigger,outcome}` counts them.

  **Two new permissions**, `automation.read` and `automation.write`, granted to
  the owner and admin roles. `automation.write` cannot be held by an API key: a
  rule keeps acting after the credential that created it is revoked, and it can
  archive links, so a key that could write one would have reach that survives its
  own revocation.

  Nothing needs to be done on upgrade, and there is nothing to configure. No rule
  exists until somebody writes one, and an instance where nobody does pays one
  indexed query a minute that returns nothing.

- **Webhooks: a workspace can have this instance POST its events somewhere.**
  Register a URL at `/webhooks` or through `/api/v1/webhooks`, choose from seven
  events — `link.created`, `link.updated`, `link.archived`, `link.restored`,
  `link.deleted` and `destination.blocked` — and each one arrives as a signed
  JSON POST. The vocabulary is closed: an unknown event name is refused rather
  than ignored, so a subscription that would never fire is not something you can
  create by typo.

  **Payloads are signed with HMAC-SHA256** using a per-webhook secret that is
  shown exactly once, on the response that mints it, and is not stored anywhere it
  can be read back. The scheme is written out for receivers in `docs/usage.md`,
  worked example included; the short version is that the key is the secret string
  as displayed and the message is the timestamp, a dot, and the raw body. Rotating
  a secret has no overlap window — the old one stops verifying immediately, which
  is what somebody rotating a leaked secret actually wants.

  **Delivery is a Postgres queue, not a call on the request path.** A link write
  queues one row per subscribed webhook and returns; the scheduler drains it every
  thirty seconds under the same leader lock the mail outbox uses. A failure is
  retried with a doubling backoff — 1m, 2m, 4m, 8m, 16m, 30m — for seven attempts
  spanning 61 minutes, then abandoned. Every attempt is recorded with its status,
  attempt count and response code, and that log is on the page behind each
  registration's **Deliveries** button and at
  `GET /api/v1/webhooks/{id}/deliveries`. Finished rows are pruned by age;
  `LINKCTRL_WEBHOOK_RETENTION_DAYS` is the window and there is no "keep forever"
  setting for it, because this table grows by a row per link write per webhook.

  **Your endpoint has to be publicly routable, and this is enforced twice.** The
  URL goes through the same destination checks a link's does when you register it,
  and the address the hostname actually resolves to is checked again at the moment
  the connection is opened — so a name that later points at a private address is
  not delivered to either. No redirect is followed at all: a receiver answering
  `302` is pointing this server at a URL nobody registered, and the `3xx` is
  recorded instead of chased. This is deliberately stricter than where a *link*
  may point, because a link sends a visitor's browser somewhere and a webhook
  sends the server; `docs/SECURITY.md` says so at length.

  **Two new permissions**, `webhooks.read` and `webhooks.write`, granted to the
  owner and admin roles. `webhooks.write` cannot be held by an API key: a webhook
  keeps delivering after the credential that created it is revoked, so creating
  one takes a signed-in person. Reading and inspecting deliveries work with a key.
  Up to twenty webhooks fit in a workspace.

  Nothing needs to be done on upgrade. No webhook exists until somebody registers
  one, and an instance where nobody does never opens an outbound connection for
  this feature. `LINKCTRL_WEBHOOK_TIMEOUT` and `LINKCTRL_WEBHOOK_RETENTION_DAYS`
  both have working defaults.

- **Every link now has a QR code, and scanning one is counted.** `GET
  /api/v1/links/{id}/qr.svg` draws it; `GET /api/v1/links/{id}/qr` says what it
  encodes and how it is drawn; `PUT` the same path stores a style — foreground,
  background, error-correction level, quiet zone and module size — and `DELETE`
  puts it back to plain black on white. The link's own page shows the code inline
  with the same controls and a download beside it.

  **SVG only**, by decision: the output is vector text, so no image encoder is in
  this program's dependency set and nothing rasterises on a request. A code
  prints at any size. There is no PNG download and no way to have two codes for
  one link.

  **The code paints its own white background and stays black on white in both
  themes.** A QR code inverted onto a dark field is refused by a large share of
  scanners, and a transparent one inverts itself the moment somebody switches to
  dark mode — so the picture does not follow the theme and the frame around it
  does. The style form will accept a low-contrast pair if that is what your brand
  wants; it refuses the same colour twice and anything that is not a hex colour.

  **A scan is an ordinary click, labelled `qr`.** The code encodes your short URL
  with `?src=qr` on it, because a camera sends no referrer and there is nowhere
  else the fact could come from. Scans then appear in the Referrers breakdown as
  `qr`, beside the `direct` you already had, with no new table, column or
  dimension — so they are deduplicated by visitor, filtered for bots and broken
  down by device and country like everything else. Two things follow and are
  worth knowing: anybody who types `?src=qr` by hand is counted as a scan, and
  two printed codes for one link cannot be told apart. **Any other `?src=` value
  is ignored**, deliberately — the analytics table keys on that value, so an open
  parameter would let anybody grow it without bound.

  `?src=qr` is **forwarded** to your destination when a link has query forwarding
  on, unlike the signature parameters, which are stripped. A signature is a
  credential; a source label is not.

  Nothing to do on upgrade. Codes are generated on demand and no link needs a row
  until somebody restyles its code.

- **Campaigns: label your links with the work they belong to, and filter by it.**
  `GET|POST /api/v1/campaigns`, `GET|PATCH|DELETE /api/v1/campaigns/{id}`, and a
  Campaigns page beside Folders. A link carries a campaign through `campaign_id`
  on create and update, and `GET /api/v1/links?campaign={id}` — or
  `?campaign=none` — filters the list, exactly as the folder filter does.

  A slug is derived from the name if you do not give one and is unique per
  workspace, because it is what a filter URL names. Start and end dates
  **describe** the campaign and enforce nothing: a link in a campaign that ended
  last month redirects exactly as it did before, because expiry is a property of
  the link. **Deleting a campaign keeps every link it held** — they stop carrying
  it, and none is deleted, archived or moved.

  A link can carry a folder and a campaign at the same time; they answer
  different questions. Campaigns are guarded by the link permissions you already
  have rather than by new ones, so a viewer sees and filters by them and an
  editor manages them.

  **There is no per-campaign analytics** — no click total, chart or export. That
  is a later phase, because computing one means a new pass over every click
  event, stacked on the rollup job this release has just rewritten.

- **A workspace can serve its short links on a hostname of its own, once it has
  proved it controls the name.** Registering a hostname is not enough and never
  becomes enough: until a DNS TXT record published in your zone has been read
  back by this instance, a request arriving for the hostname gets the same `404`
  it got before you registered it. That gap is the point of the feature rather
  than a step on the way to it — without it, anybody who pointed a hostname at
  this address could serve short links on it.

  The Domains page shows the record to publish and checks it on demand.
  `POST /api/v1/domains/{id}/verify` is the same check. Once it passes, links can
  be created on the hostname (`domain_id` on create, or leave it out and get your
  workspace's own hostname), `short_url` is built from it, and the bare hostname
  can redirect wherever you like via
  `PUT /api/v1/domains/{id}/root-redirect`. The links list gained a hostname
  filter, and `GET /api/v1/links?domain=` is its API form.

  **Every registered hostname is re-checked hourly, and the check has teeth.**
  The first failure notifies the owning workspace and changes nothing else — one
  failed DNS poll is weak evidence, and taking a customer's links down over it
  would make an availability feature into an availability incident. After **24
  hours** of continuous failure the hostname stops being served on every replica
  at once, with a second notification and an audit record. Both numbers are
  operator configuration (`LINKCTRL_DOMAIN_VERIFY_INTERVAL`,
  `LINKCTRL_DOMAIN_VERIFY_GRACE`); the runbook in `docs/deployment.md` states
  them and what changing them trades. **Renaming a hostname un-verifies it**: the
  record you published proves control of the old name — and a rename that lands
  while a check is running makes that check verify nothing and say so, because it
  proved control of a name the row no longer carries.

  Two bounds keep that stop reachable, and both are about the same thing: the
  hourly pass is the only mechanism that ever takes a lapsed hostname out of
  service, so nothing anybody can create at will may be allowed to delay it. A
  pass checks the hostnames you are **serving** before the ones merely registered.
  And **a workspace may register at most twenty-five hostnames** — every
  registration is a recurring DNS lookup this instance owes to a nameserver
  somebody else runs, whether or not the name resolves.

  **TLS stays your reverse proxy's.** This application never speaks ACME — no
  certificate authority is contacted and no account key is held. It answers
  Caddy's on-demand `ask` at `/tls-check`, and answers it **only** for hostnames
  that have verified; a wider answer would make the instance an unauthenticated
  certificate-issuance trigger for any name on the internet. The Caddyfile block
  is in `docs/deployment.md`. Keep the endpoint on the loopback address.

  Two operational notes for anyone already running this. The links a workspace
  created before verifying a hostname **do not move** — nothing rewrites a URL
  somebody has already published — so a workspace ends up with links on two
  hostnames, which is what the new filter is for. And `/tls-check` is now a
  reserved alias: an existing link with that alias is unaffected, but a new one
  cannot take the name.

- **A workspace can register a domain of its own.** Registering records that a
  hostname belongs to your workspace; the paragraph above is what makes it serve
  anything.

  A **Domains** page in the header's identity menu registers, renames and removes
  them, behind the `domains.write` permission that owner and admin already hold.
  The same operations are on the API: `GET/POST /api/v1/domains`, `PATCH` and
  `DELETE /api/v1/domains/{id}`. The existing `/api/v1/domain` — singular, the
  instance default's root redirect and bot policy — is unchanged and is a
  different resource.

  **A hostname belongs to exactly one workspace, instance-wide.** A hostname is
  one alias namespace, so it cannot be shared: registering a name somebody else
  already registered is refused, and the refusal names the hostname rather than
  its owner. `domains.write` now checks ownership as well as permission — an
  admin manages their own workspace's hostnames and receives `403` on another
  workspace's, which is also absent from their list. The instance's default
  domain stays the operator's and is not renamed or removed from this page.

  Registering, renaming and removing a domain are written to the audit log.

  Nothing to do on upgrade. The migration adds one nullable column and a
  constraint; no existing row changes, and an instance where nobody registers a
  hostname behaves exactly as it did.

- **Folders.** Links can be filed into a tree of folders, up to eight levels
  deep, with a page at **Folders** — linked from the top of the links list — that
  creates, renames, moves and deletes them. The links list gains a folder filter,
  including a **No folder** option for everything that was never filed, and a
  link's own page gains a folder select. Everything the page does is also on the
  API: `GET/POST /api/v1/folders`, `PATCH` and `DELETE /api/v1/folders/{id}`,
  `POST /api/v1/folders/{id}/move`, `folder_id` on a link, and `?folder=` on the
  link list.

  **Deleting a folder never deletes a link.** It removes the folder and the
  folders inside it; every link anywhere in that branch stays exactly where it
  was and becomes unfiled, which the **No folder** filter then finds. There is no
  undo for the folder itself — what is lost is a name and a shape.

  Moving is two clicks rather than a drag: **Move** on a folder, then **Move
  here** on the destination. Only destinations that would actually be accepted
  offer the button, and a folder can never be moved into itself or into anything
  inside it. The page works with JavaScript switched off and is operable from a
  keyboard; there is no drag-and-drop, deliberately.

  Two folders in the same place may not share a name, and the comparison ignores
  case. Nothing needs to be done on upgrade — the `folders` table has existed
  since 0.1.0 with nothing able to write to it, so every existing link starts
  unfiled and behaves exactly as it did.

- **A world map on a link's page, and rings on the breakdowns beside it.** The
  country breakdown is now also a choropleth: countries shaded by their share of
  the link's clicks, five bands, every shape carrying its exact figure. A toggle
  switches the shading to unique visitors, and when it does, the page repeats the
  sentence that says those are privacy-preserving estimates at daily resolution —
  the same one the API returns — because a map is a good deal more persuasive
  than a table and an estimate should not become a fact by being drawn.

  The ranked list is not replaced. It stays a click away from the map, and it is
  still the view that answers "exactly how many". The other breakdowns — devices,
  browsers, operating systems, referrers, languages — each gain a ring showing
  whether the traffic is one value or five, next to the numbers rather than
  instead of them.

  **With no GeoIP database configured the map is not drawn at all**, and says so.
  A world coloured entirely in the no-data shade is a picture of nothing that
  looks like a picture of something.

  No JavaScript, no CDN, no build step: the map is inline SVG computed on the
  server from country outlines compiled into the binary. Those outlines come from
  **Natural Earth**, which is public domain, packaged as world-atlas, which is
  ISC — vendored, version-pinned and checksummed like the two other third-party
  assets in the tree, with `make verify-assets` failing rather than silently
  repairing a mismatch. Nothing about the deployment changes and the Content
  Security Policy is untouched.

- **A staleness metric for the background rollups**,
  `linkctrl_rollup_staleness_seconds{job}` — seconds since each job last
  succeeded, read from the database rather than from process memory, so every
  replica reports the same number and a restart does not make a stalled job look
  healthy. Alert recipes are in `docs/operations.md`. The metric that existed
  before it, `linkctrl_job_last_success_timestamp_seconds`, is unchanged and is
  still the per-replica view; it is the wrong one to alert on and now says so.

- **A link can ask for something before it redirects.** Four gates, each off
  unless you switch it on, each set on the link's own page or over the API.
  Nothing changes for a link that uses none of them.
  - **A password.** The visitor gets a small challenge page and types it in;
    getting it right answers the redirect. It is stored as an argon2id hash with
    the same cost parameters an account password gets, and neither the password
    nor its hash is ever returned by the API. **Nothing is remembered in the
    visitor's browser** — no cookie, no token, no session — so coming back later
    means typing it again. That is deliberate rather than unfinished: it is what
    keeps the short-link host free of the session and CSRF machinery the
    dashboard needs, and it is why the challenge could be added to that host at
    all. Guesses are rate-limited per address *and* per link.
  - **A single use.** The first visit redirects, the second answers 410 Gone.
  - **A click ceiling.** The same thing with a bigger number. The count is exact
    and lives in the database — it is **not** the approximate click total shown
    on the link's page, which is written in batches and can lose one. A HEAD
    request never spends a click, so a link checker cannot use up a one-time
    link by asking whether it is alive — and it is refused all the same once
    there is nothing left to spend, so a spent link answers 410 to HEAD as well
    as to GET. Raising a ceiling on a link that has already stopped starts it
    working again.
  - **A signature.** The plain short URL is refused with 403 and only a signed,
    unexpired one works. Signed URLs are minted from the link's page or with
    `POST /api/v1/links/{id}/sign`, carry an expiry of up to thirty days, and the
    expiry is inside the signature so it cannot be extended by editing the URL.
    The signature parameters are stripped before a link's query forwarding
    passes anything to the destination, so whoever runs the destination never
    receives a URL they could replay. A signed URL names the hostname the link
    is published under — your own verified domain, when the link is on one — and
    works only there: the hostname is inside the signature, so the same alias on
    another of your hostnames is a different link and will not accept it.

- **A link can divide its traffic between several destinations.** A link now
  carries a split test: a set of *arms*, each one a destination of its own, and
  every visitor no routing rule claimed goes to one of them. Managed on the
  link's own page and over the API at `/api/v1/links/{id}/split`. A link with no
  arms is unchanged, which is every link that exists today.
  - **Weighted, for percentage splits.** Each arm has a weight and receives that
    share of the traffic. Weights are relative rather than percentages, so 60/40
    and 600/400 are the same test and adding a third arm does not mean editing
    the first two. The page shows each arm's computed share beside its weight.
  - **Sequential, for a strict rotation.** Arms are visited in turn — first
    visitor to the first arm, second to the second, and round again — and the
    order is **global**: it is kept in the database rather than in a process, so
    it holds across every replica and across restarts. That costs a database
    write on every visit to such a link, and only to such a link. "Approximately
    sequential" would have been free and would have been a support ticket.
  - **A fallback destination.** One per link, used when no rule matched and no
    arm was chosen. It stands in for the link's own destination without changing
    it, so switching the fallback off puts the link back exactly where it was.
  - **Switching an arm off is the feature flag.** One click, no deletion, and
    the remaining arms re-share its traffic on the next request. Clicks already
    recorded against the parked arm are kept. Setting an arm's weight to zero
    does the same thing while leaving it in the weighted list.
  - **There is no stickiness, deliberately.** Somebody following the same link
    twice may see two different arms. Each click is an independent trial, and
    which arm converted is answered by the per-destination breakdown rather than
    by remembering people — which would mean either a cookie this product does
    not set or a per-visitor lookup on the redirect path.
- **Clicks record which destination served them.** `click_events` gains a
  `destination_id`, and the link's page and `GET /api/v1/links/{id}/stats` gain a
  per-destination breakdown: clicks, unique visitors and share, beside each arm's
  configured weight. A split with no attribution is a coin flip with extra steps.
  The breakdown reads the same daily rollup every other breakdown does, so it
  costs nothing to a link that has none, and an unattributed click means the
  link's own destination. Clicks recorded against an arm that has since been
  deleted are still reported, as a destination that no longer exists — a running
  test's totals must not change because somebody tidied up.
- **A split arm's destination goes through the same checks as a link's.** The
  same tiers, the same refusals, the same audit record naming the split-variant
  surface. An arm receiving 5% of the traffic is still somewhere a browser is
  sent.
- **A link can send different visitors to different destinations.** A link now
  carries an ordered list of routing rules: each one has a condition set and a
  destination, they are checked from the lowest priority number upwards, and
  **the first rule that matches wins** — the rest are not consulted. A visitor
  matching none goes to the link's own destination, which is what every link
  does today and will keep doing. Rules are managed on the link's own page and
  over the API at `/api/v1/links/{id}/rules`, and a rule can be switched off
  without being deleted.
- **Twelve conditions, and any combination of them.** Country, region, city,
  language, browser, operating system, device, date and time, referrer host,
  query parameters, UTM parameters, and whether the visitor has been seen
  earlier today. Every condition set on a rule must hold and any listed value
  matches, so a rule narrows as you add to it. Comparison is
  case-insensitive, and a condition tested against something the request does
  not have — no referrer, no GeoIP database — does not match, so an
  unresolvable request falls through rather than being routed on a blank.
- **Date and time conditions are evaluated when the visitor arrives**, never
  when the link was cached, so a window opens and closes on time even on a hot
  link. A window is a set of weekdays, a `HH:MM`–`HH:MM` span, or both, in an
  IANA timezone — `Europe/London`, not an offset, because an offset is wrong for
  half the year. A span whose end is earlier than its start runs overnight. The
  timezone database is compiled into the binary, so a rule evaluates identically
  whatever base image the server was built on.
- **Region and city can be routed on, and are still never stored.**
  `LINKCTRL_GEOIP_MMDB_PATH` pointed at a MaxMind **City** database makes region
  and city conditions work; the country database that was enough for analytics
  carries neither. The values are resolved for the length of one redirect and
  discarded — `click_events.region` and `click_events.city` stay null, asserted
  by test, and no page shows them. A geographic condition on an instance with no
  database simply never matches.
- **"Returning visitor" means seen earlier today, and the day ends at midnight
  UTC.** A visitor from yesterday is new again. That is the whole feature rather
  than an approximation of a longer-lived one: a durable answer needs a cookie or
  a per-person identifier kept across days, and this product keeps neither. It is
  computed from the same daily-salted, address-free visitor hash the analytics
  already use, held in Redis, maintained by the click pipeline rather than on the
  redirect path, and expiring with the day. **With no Redis configured, every
  visitor reads as new.**
- **A rule's destination goes through the same checks as a link's.** Private and
  loopback addresses, the cloud metadata endpoint, obfuscated numeric hosts,
  schemes outside `http`/`https`, the operator's blocklist and any enabled
  reputation feed all apply, and a refusal is recorded in the audit log naming
  the routing-rule surface. A rule reached only by mobile visitors in one country
  is still somewhere a browser is sent.
- **There is no cookies condition, and that is a decision rather than an
  omission.** The redirect path sets no cookie and reads none; adding one would
  mean storing a per-visitor identifier the rest of the product deliberately does
  not keep. Sending one to the API is refused with the code
  `cookies_not_supported`, and the rule-list endpoint advertises the refusal
  beside the twelve conditions that are supported.
- **A link without rules costs exactly what it did before.** Its cached value is
  byte-for-byte what it was, rule evaluation never begins, and no geographic or
  Redis lookup happens. For a link *with* rules, evaluation reads only the
  request and that cached value — no database query per request, and the one
  network call the returning-visitor condition needs is reached only by a
  request that has already satisfied everything else on the rule. Re-measured on
  the built image; see [docs/slo.md](docs/slo.md).
- **`lctl demo` fills an instance with the whole product, not just its links.**
  It was written when a workspace and twenty links were all there was, and it
  had not grown since. It now seeds a second workspace with links of its own, two
  more accounts and their memberships, an outstanding invitation and two
  redeemed ones, an inbox with unread items, an audit trail spanning eight
  actions and two people, three refused destinations with a dispute about each —
  one open, one allowed, one upheld — and bot blocking switched on for exactly
  one link. Everything is created through the same service calls the dashboard
  and the API make, so nothing in it describes a state the product could not
  reach.
- **The demo seeder needs no mailer, enables no reputation feed, and does not
  touch `LINKCTRL_SIGNUP_MODE`.** The outstanding invitation is delivered by the
  copyable link, which is how a default instance delivers one; no destination
  reaches a third-party feed; and the extra accounts are written directly, so a
  `closed` instance stays closed while it runs. All three are asserted by test
  rather than documented and hoped for. The dataset does register **webhooks**,
  because a page with none shows nothing — so a link created on a demo instance
  queues a delivery carrying its destination, to a `.example` hostname that never
  resolves. The seeder itself queues nothing, which is asserted too.
- **`lctl demo --reset` puts an instance back the way it found it**, seeded
  accounts and organizations included, so running it twice produces the same
  demo. It always writes into the owning account's oldest workspace rather than
  into whichever one the workspace switcher was last left on, so what a reset
  removes and where the next run writes are the same answer — on a long-lived
  demo instance they were not, and a reset could remove the accounts and the
  click history while leaving the links it could not see. A full run takes a
  little under two seconds against a local Postgres.
- **A link can forward the path a visitor arrived with, not just the query
  string.** `forward_path` is per link and off by default, in the API and as a
  checkbox on the link's edit page. With it on, `/{alias}/api/quickstart`
  reaches the destination's own `/api/quickstart`, so one short link can stand
  in for a whole documentation tree. Both halves may be on at once: the path is
  joined first, then the query is merged onto the result.
- **With path forwarding off, anything after the alias is a `404`.** Not a
  redirect to the bare destination — that would make every link on the instance
  answer every URL beneath itself. It is the custom 404 page, and it spends the
  same 404-probe allowance an unknown alias does, so the refusal cannot be used
  to find out which aliases exist.
- **Forwarding cannot move the destination's origin.** The visitor's segments
  are appended to the destination's path and nothing touches its scheme, host,
  query or fragment; an encoded `?` or `#` stays a path byte rather than
  becoming a separator; and `..` segments are refused rather than resolved, in
  every spelling the URL standard treats as one. A property test asserts those
  invariants over generated input. Measured on the built image with forwarding
  on for all 100,000 seeded links and every request carrying extra path
  segments: 100% of 240,002 requests under 20ms at 2,000 rps, generator p99
  163µs, in [docs/slo.md](docs/slo.md).
- **Automated clients can be refused, per link or for the whole link domain.**
  A link's setting is *inherit*, *block* or *allow*; *inherit* is the default and
  takes the domain's answer, which on a fresh instance is allow — so nothing
  changes for anybody who does not switch it on. An operator with `domains.write`
  can turn it on for the domain, and can additionally **enforce** it, which
  overrules links set to allow. A link-level *allow* under an enforcing domain is
  refused at the API and the control is disabled in the dashboard, rather than
  being stored and quietly ignored. Both settings are on
  `PATCH /api/v1/domain` and `PATCH /api/v1/links/{id}`, and both changes are
  audited.
- **A blocked client gets `403` and learns nothing.** The body is a fixed page
  built into the binary, naming no alias and no destination, and the refusal
  happens before the link's state is looked at — so a live link, an expired one
  and an archived one return byte-identical responses. Being blocked reveals no
  more than the `404` an unknown alias already returns.
- **A refusal is counted, not logged.** It is a click event with `is_bot` already
  true, and increments `linkctrl_redirects_total{outcome="blocked_bot"}`. It is
  deliberately kept out of the audit log: a crawler hitting one blocked link
  thousands of times a day would otherwise fill the table whose growth this
  product alerts on.
- **The redirect path costs nothing extra for it.** The decision reads two fields
  the cached snapshot already carries, so a cached redirect still issues zero
  queries — asserted by a test that counts what reaches Postgres. Measured on the
  built image with blocking switched on for all 100,000 seeded links: 100% of
  240,001 requests under 20ms at 2,000 rps, generator p99 1.5ms, in
  [docs/slo.md](docs/slo.md).
- **Detection is a heuristic and there is no way past it.** The same user-agent
  classifier the analytics use decides — it treats a missing user agent as
  automated and matches substrings including `preview`, `monitor` and `checker`,
  and its false-positive rate has never been measured. A person it misjudges gets
  the 403 with no challenge and no appeal, and the link's owner is not told. That
  is why the default is off; a bypass is a separate, later piece of work.
- **A hostile destination is refused before a link is made, in three tiers that
  differ in what it costs to overrule them.** A refusal's `422` carries a reason
  code naming both — `unappealable.private_address`,
  `high_confidence.embedded_host`, `low_confidence.punycode_homograph` — so a
  caller can tell "this will never be allowed" from "ask the instance owner".
  Non-`http(s)` schemes and private, loopback, link-local, carrier-NAT and
  cloud-metadata addresses are the unappealable tier and have no off switch of
  any kind. A curated list compiled into the binary is the middle tier; changing
  it costs a rebuild, on purpose. Heuristics and a `blocked_destinations` table
  are the bottom tier, meant to be changed without one. The host is canonicalized
  once, before any tier sees it: lowercased, a trailing dot folded away, and a
  host written outside ASCII converted to the same `xn--` form a browser resolves
  it to. So `169.254.169.254.`, `169。254。169。254` with ideographic full stops,
  `１６９.２５４.１６９.２５４` in fullwidth digits and `169.254.169.254` are one address to
  all three tiers, and what gets stored is what they judged. **Both foldings
  convert rather than refuse**, because a trailing dot is a fully qualified name
  and `müller.de` is an ordinary one: `https://example.com./x` is stored as
  `https://example.com/x`, and `https://müller.de/preise` as
  `https://xn--mller-kva.de/preise`. **This changes what is stored for a
  destination whose host is not ASCII** — links created before this version keep
  the spelling they were created with, since accepted destinations are never
  re-checked, and re-saving one converts it. A host that is not a usable name in
  any spelling — a right-to-left override, a broken `xn--` label — is refused as
  malformed, with the untiered code `invalid`.
- **Three low-confidence rules, all of them appealable.** A host spelled to
  imitate another in punycode (`аpple.com` with a Cyrillic а), credentials before
  the host (`https://paypal.com@evil.example/`), and a destination that is itself
  a public short link. Each will sometimes be wrong, which is why they are the
  tier the instance owner overrules. The first two are computed; the third is a
  list of about twenty known shortener hosts that the schema seeds into
  `blocked_destinations`, so **removing one is a `DELETE` and not a rebuild**,
  and it stays removed.
- **The check runs everywhere a destination is written**: creating a link,
  editing one, and setting the link domain's root redirect. It does **not** run
  on the redirect path — a link accepted before its host was listed keeps
  working, because re-checking accepted links is a separate job and reading a
  blocklist on the hot path is not something this product does.
- **Every refusal is in the audit log**, as `destination.blocked`, with the tier,
  the rule, which surface it came from, an `ip_prefix` — never an address — and
  the attempted URL kept as evidence. That URL is stored defanged
  (`https[:]//evil[.]example/...`, anything HTML-active percent-escaped), so
  nothing that displays the log can render it as markup or as a link somebody
  clicks. Note that anyone who can create a link can now add audit rows;
  `LINKCTRL_AUDIT_RETENTION_DAYS` is what bounds that.
- **A low-confidence refusal can be appealed, and the instance owner decides.**
  Somebody who was refused asks for a review — from the link form, or
  `POST /api/v1/disputes` — and the request appears in a queue at `/disputes`
  with a notification to the owner. Allowing it deletes the blocklist entry and
  the destination becomes usable immediately; upholding it leaves the refusal
  standing. Either way the person who asked is notified, and either way it is an
  audit event (`dispute.allowed`, `dispute.upheld`).
- **The other two tiers have no appeal path at all.** A private address, a
  forbidden scheme or a host on the curated compiled list answers `422` with
  `not_disputable` and never reaches the queue. The party those refusals protect
  is the visitor, not the person appealing, and an owner who could approve
  `169.254.169.254` on request would have turned the queue into the SSRF the
  validator exists to prevent.
- **The queue is built as an attack surface, because it is one.** It shows an
  instance owner a URL a stranger chose. Every destination in it is defanged in
  the database and in the API, is never rendered as a link or in anything a
  browser resolves, and **the server never fetches it** — no preview, no
  screenshot, no favicon, no liveness check. A test parses the feature's source
  and fails on any outbound-HTTP symbol, so that stays true.
- **`destinations.review` and `destinations.decide`**, two new permissions,
  **held instance-wide by named people and by no organization role**. Reading
  the queue and acting on one are separate grants because they are different
  risks: a key may hold the first — the queue discloses who filed a dispute and
  a defanged host, and escalates nothing — and may never hold the second,
  because allowing a destination would let that same key point links there. Who
  holds them is below, under the instance-level principal.
- Two refusals `allow` gives instead of pretending to work: a punycode or
  credentials refusal has no blocklist row to delete (the queue marks it and
  offers only *Uphold*), and an entry that came from
  `LINKCTRL_DESTINATION_BLOCKLIST` would come back at the next restart — take it
  out of the environment instead.
- **An optional third-party reputation feed, off by default, and the disclosure
  that comes with it.** Every other check LinkCtrl makes on a destination is
  local. Setting `LINKCTRL_FEED_URL` adds one that is not: the destination
  somebody typed is **sent to a server you name** each time a link is created or
  edited, the root redirect is set, or a refusal is disputed. Nothing is sent on
  the redirect path and existing links are never re-checked. The destination is
  the entire payload — no account, no address, no workspace, no name for your
  instance. `LINKCTRL_FEED_NAME` is required alongside the URL, because an
  instance may not send destinations somewhere it cannot name, and the endpoint
  must be `https`.
- **The instance discloses it, at `/feeds` and `GET /api/v1/feeds`**, to every
  signed-in account rather than to administrators only — what it describes is
  what happens to their own destinations. It names the feed, states what is sent
  and when, and says that only the operator can change it; with no feed
  configured it says plainly that no destination reaches one. It answers for
  outbound webhooks too, which are the other way a destination leaves and are a
  workspace's rather than an operator's — see *Fixed*, where that half is
  described, because it was not there when this feature shipped. The page is
  read-only and accepts no `POST`, asserted by test: this product has no
  instance-level principal, so instance-wide settings are not changed from the
  dashboard.
- **A feed is a low-confidence signal and never a dependency.** It is asked
  **last**, only about destinations every built-in check already accepted — so a
  destination your own rules refuse never reaches the feed, and those rules
  answer identically with a feed on, off, or failing. That is a bound on the
  feed and not on the instance: the refusal is itself a `destination.blocked`
  event, and a workspace that has subscribed a webhook to it receives the
  refused destination, defanged. A refusal it produces is
  `low_confidence.feed_reputation`, appealable like any other, and an owner
  allowing it also stops that host being sent again. A feed that times out,
  errors, or answers something unreadable accepts the destination and increments
  `linkctrl_destination_feed_checks_total{result="error"}` — alert on it, because
  a feed that quietly stopped working is otherwise indistinguishable from no
  feed.
- **Dispute outcomes are emailed** when a mailer is configured. The dashboard
  notification is unchanged and does not depend on one; the email is the
  addition, and it is the only message this product sends to somebody who did not
  choose to be an administrator.

### Changed

- **`GET /api/v1/audit` now returns `workspace_id`.** The column has been stored
  and indexed since the audit log shipped and was dropped on the way out, so a
  reader could not tell which workspace a link-scoped action such as
  `link.bot_blocking_changed` came from — those records name the link and
  nothing else. It is absent on organization-level actions, which is most of the
  invitation and membership vocabulary. **The log is still not filtered by
  workspace**, deliberately: one that could be narrowed to wherever the reader
  happens to be standing would hide exactly the actions worth reviewing.

- **Dashboard pages are no longer counted as short links in the HTTP metrics.**
  `linkctrl_http_requests_total{surface="redirect"}` and its duration histogram
  mixed genuine short-link traffic with dashboard page loads for every route
  added since 0.1.0 — nine of them, including `/notifications`, `/disputes`,
  `/organizations` and `/campaigns` — because the classifier carried a
  hand-written list that stopped being updated. The surface is now derived from
  the routes the application actually registers. **Redirect-surface figures on
  an instance with dashboard traffic will drop when you upgrade**, and the new
  ones are the true ones. The SLO series `linkctrl_redirect_duration_seconds` is
  unaffected and always was — the redirect handler records it directly.

- **`linkctrl_rate_limit_fallback_total` is new**, and it is the series that
  says a shared rate limit has stopped being shared. The tracked-keys gauge
  cannot: a healthy shared limiter never writes its local table, so it reads zero
  whether the limit is working or the instance is idle. Alert on the rate — the
  counter is monotonic, so a threshold on its value latches forever after one
  blip. `docs/operations.md` carries the expression.

- **A mail relay's rejection no longer puts the recipient's address in the
  process log.** A bounce echoes the address it refused, and that line was logged
  at ERROR from the first failed attempt — moving an address out of the database,
  which is access-controlled and retention-bounded, into a log stream that is
  usually shipped elsewhere with neither. The address is replaced in the log line
  and kept in `mail_outbox.last_error`, where the row already carries it.

- **Two dark-theme hover colours meet WCAG AA.** White text on the accent and
  danger hover surfaces was at 4.47:1 and 3.67:1, below the 4.5:1 the theme
  claims for every token pair. Both hovers now darken rather than lighten, which
  is what the light theme already does.

- **A 6to4 address is anonymised as the IPv4 address it carries.** `2002::/16`
  embeds a client's IPv4 address in the bytes a `/48` prefix keeps, so a session
  or audit record from such a client stored all four octets where every other
  address stored a network. It is now folded and masked to `/24` like any other
  IPv4. No other IPv4-in-IPv6 scheme is affected — Teredo, NAT64 and ISATAP all
  embed in bits the prefix already discards.

- **The analytics salt cache no longer holds a salt the purge has deleted.** The
  in-memory copy was evicted one day later than the database row, so for a day
  the process held the salt whose deletion is the de-identification step. It now
  uses the same rule the delete statement does, and trims on every lookup rather
  than only when a new day's salt is created — which is what a replica that is
  not the job leader relies on.

- **A Redis outage now costs one probe per cooldown, as documented.** The shared
  rate limiter's circuit breaker let every waiting request through the moment the
  cooldown lapsed, each paying `REDIS_READ_TIMEOUT` against a server that was
  still down. At the default 50ms this was wasteful; if you have raised that
  timeout, it was the stall the breaker exists to prevent.

- **The dispute queue no longer offers Allow on a refusal it cannot lift.** A
  destination refused by an entry from `LINKCTRL_DESTINATION_BLOCKLIST` drew the
  same **Allow** button as any other, and pressing it answered `409` — the entry
  is rewritten from the environment at every boot, so removing it would last
  until the next restart. That was the most likely dispute on any instance whose
  operator configured a blocklist. The button is now drawn only where an allow
  can actually do something, and the guidance to take the host out of the
  environment is where the button used to be.

- **Switching workspace from a link's page lands on your links, not on a
  `404`.** The switcher returns you to the page you were on, and on a link's
  detail page that page names a link belonging to the workspace you just left.

- **The notification bell is no longer shown to an account that belongs to no
  organization.** Its **View all** went to a page that redirects such an account
  straight back — the one control in the header that led nowhere, offered to the
  account most likely to have unread notifications.

- **An invitation email to a closed instance no longer promises the form will
  create an account.** With `LINKCTRL_SIGNUP_MODE=closed` an invitation may only
  admit somebody who already has one, which the redemption page has always said
  and the email contradicted.

- **A `HEAD` request to a sequentially split link no longer advances the
  rotation.** Link checkers and unfurlers probe with `HEAD`, and every probe used
  to move the counter that decides which destination the next visitor gets —
  re-phasing the test with no click recorded to explain why the arms were
  uneven. A `HEAD` now reports the destination the next `GET` would be given,
  which is what a checker needs, without changing it.

- **A correct link password no longer spends the link's own rate limit.** The
  per-link limb of `LINKCTRL_LINK_PASSWORD_RATE_LIMIT` is charged before the
  password is checked, deliberately — that ordering is what stops timing
  revealing which limb refused — so more legitimate visitors than the limit
  opening the same link at once could exhaust it between them, with nobody
  attacking anything. A correct password now hands that token back. Wrong
  guesses are charged exactly as before, and the per-address limb is unchanged.

- **A deep-link path is bounded.** With `forward_path` on, everything after the
  alias is visitor-supplied and was limited only by the 1 MiB request ceiling,
  while the joiner walks it several times. It is now capped at 4096 bytes and 64
  segments, and anything past that gets the same `404` a path the link cannot
  forward already got.

- **An expired or archived link now records the traffic it receives.** It
  recorded nothing before, unless bot blocking happened to be switched on for
  it — because the bot refusal is decided before the link's state is, so a
  blocked crawler was counted on a dead link while a browser meeting the same
  link's `410` was not. Whether identical traffic was counted therefore depended
  on a setting about responses. **Counts on expired and archived links will
  start moving**, most visibly under crawler traffic; `links.click_count`
  includes bots, and the Clicks tile on the dashboard reads the human-only
  rollup, which is why the two numbers differ.

- **The notification inbox is now scoped to the workspace you are acting in.**
  A notification produced by a workspace — an automation rule firing, a custom
  domain failing its verification check — appears while you are in that
  workspace and not while you are in another. Anything belonging to the
  organization rather than to one workspace, such as a dispute decision or an
  audit-growth warning, is shown wherever you are. The bell's count, its
  preview and the notifications page all agree, because they share the
  predicate.

  The column recording which workspace produced a notification has been written
  since custom domains shipped and read by nothing, while the code comments
  beside it said this was already how it worked. **If you rely on seeing every
  workspace's notifications at once, switch workspace to see theirs** — there is
  no combined view.

- **The instance default domain's root redirect and bot policy are now the
  instance principal's.** They needed `domains.write`, which the owner and admin
  *roles* hold — so on an instance with more than one organization, every
  organization's owner and admin could repoint the hostname all of their links
  are served on and change the bot policy applied to them. With
  `LINKCTRL_SIGNUP_MODE=open` that was one registration away, since a registrant
  is provisioned as the owner of their own organization. They now need
  `domains.write.instance`, which reaches a person only through the instance
  principal.

  **On upgrade the migration confers it on whoever already holds the
  principal**, so an operator who has claimed their instance keeps working
  exactly as before. It does not re-derive who that is, which means a principal
  moved with `lctl instance principal move` stays moved. **Organization owners
  and admins lose access to these two settings** — `domains.write` itself is
  unchanged, and a workspace goes on registering and administering its own
  hostnames.

- **A redirect no longer pays the Redis read timeout twice while Redis is
  stalled.** A cache miss used to spend `REDIS_READ_TIMEOUT` on the lookup that
  never answered and then spend it again on the write that would have
  repopulated the cache — measured at 108ms for a cold redirect against a
  stalled server, past the 100ms uncached target. A failed lookup now suppresses
  that write for one resolve, because a server that will not answer a read will
  not usefully answer the write either. Nothing changes while Redis is healthy,
  and the in-process cache is still populated either way. There is one case
  where a redirect answered from memory still talks to Redis: a link carrying a
  *returning visitor* routing condition, which is now written down in
  `docs/configuration.md` where the opposite used to be.

- **Registering an address that already has an account now answers `202` and
  sends mail, where it used to answer `409`.** On an instance with `open`
  sign-ups that status code was an unauthenticated way to ask whether an address
  is registered here, which is a question a leaked address list can be tested
  against. Both answers now cost the same and return the same body, and the
  owner of the address gets a message saying somebody tried to register it —
  which reaches the person concerned rather than whoever typed the address in.
  **If you have an integration that treated `409` as "already registered", it
  will now see `202`**; nothing was created, exactly as the body says.

- **An email address that this product would accept and then fail to send to is
  now refused when it is typed.** Nine forms — `a<b@c.de` and `a,b@c.de` among
  them — passed the address pattern and were rejected by the mail parser, so a
  registration committed a row and then answered `500`. They now answer `422`
  like any other invalid address, on registration and on invitations alike.

- **`GET /api/v1/workspaces` answered with an API key now lists only the
  organization that key was issued for.** It used to list every workspace the
  key's owner belongs to, in every organization — names, slugs and identifiers
  for tenancies the key cannot act in, since a key is bound to the organization
  it was created in and always has been. A signed-in browser is unaffected and
  still sees all of them, because moving between organizations is exactly what
  the workspace switcher is for. **If an integration was relying on that
  endpoint to enumerate its owner's other organizations, it will now see one**;
  issue a key in each organization you need to reach.

- **A disputed destination now notifies the people who review disputes, instead
  of every organization owner on the instance.** Filing a dispute costs only the
  permission that would have let you create the link, and a refusal computed from
  the URL — credentials before the host, for example — is bounded by the string
  typed rather than by any blocklist row, so there is no useful limit on how many
  one account can file. The limit that matters is what each one costs, and it
  used to be one inbox row per organization owner: on an instance running
  `SIGNUP_MODE=open` that is a number a stranger grows by registering, aimed at
  people who cannot act on the queue anyway. It is now the holders of
  `destinations.review`, which is the set you chose. **Nothing else about filing
  or deciding changes**, and if you have not appointed anybody, that set is the
  account that claimed the instance.

- **The audit log is now bounded by the reader's own authority, not only by
  their organization.** A membership scoped to one workspace used to read every
  record in the organization, including workspaces where its holder had no
  membership at all — a per-action timeline and a network prefix tied to a named
  actor, for workspaces they do not administer. It now reads the records of the
  workspaces its own scope covers. **An organization-wide membership is
  unaffected** and still reads the whole organization, which is what the log is
  for; only the workspace-scoped case narrows. Nothing needs to be done on
  upgrade.

- **Analytics breakdowns are recomputed every fifteen minutes instead of every
  minute.** A link's click count still updates every sixty seconds. What moved is
  the expensive half — the per-country, per-device, per-browser and
  per-destination breakdowns — whose cost grows with the number of distinct
  values your traffic produces rather than with the number of links you have. At
  five and a half million clicks it was using between a sixth and a third of
  every minute; it now uses under one percent of every fifteen.

  **The visible consequence: a breakdown on a link's page can be up to fifteen
  minutes behind the click count above it.** Nothing on the page distinguishes
  the two, which is why the staleness metric above exists and why
  `docs/operations.md` carries an alert for it. This is what makes it safe to
  draw a map from those numbers at all — the alternative was a visualization
  reading a job that would eventually stop finishing inside its own interval.

  Nothing needs to be done about this on upgrade. The existing job keeps its name
  and its position, so its history carries forward; the new one starts from the
  usual two-day window.

- **Upgrading to this version empties the redirect cache.** The cached value a
  short link resolves through now carries its routing rules and its split-test
  arms, and an entry written by the previous version does not — so the cache key
  moved from `v1` to `v3` and every old entry is abandoned rather than read. The
  first request for each live alias after the upgrade reads the database;
  concurrent requests for the same one are collapsed into a single query, so a
  popular link does not become a stampede. Nothing needs to be done about this,
  and nothing is lost; expect a brief rise in database reads while traffic warms
  the cache again.

  The version moved twice inside this unreleased series — once for routing rules
  and once for split testing — but they ship together, so an upgrade from 0.1.0
  pays for one cold cache and not two. Both moved it for the same reason, and it
  is the reason the earlier additions to that value deliberately avoided one: an
  entry written by the older build carries no rules and no arms, so a link whose
  owner has since divided its traffic would go on sending all of it to one place
  for up to `REDIRECT_TTL`. That is a control the owner configured being silently
  absent, which is exactly what a cold cache is for.

  **On a multi-replica rolling upgrade there is one more consequence, and it is
  deliberate.** Cache invalidations are broadcast on a channel versioned with the
  cache key, so while old and new replicas are both running they do not hear each
  other's invalidations. Each still invalidates its own caches correctly; what
  they lose is the cross-replica broadcast, so an edit can take up to
  `REDIRECT_TTL` to reach a replica on the other version. That is the same
  degradation a Redis outage produces, it lasts only as long as the rollout, and
  it is preferred to one version applying another version's messages under
  different rules.

- **`LINKCTRL_REDIRECT_DEFAULT_STATUS` accepts `303`, and never applies to a
  password submission.** A correct link password is now answered `303` whatever
  the instance is set to. On a `307` instance it used to be answered `307`, and
  `307` forbids the browser from changing the method — so the browser re-sent the
  password it had just submitted, as a POST body, to the link's destination: a
  third-party host the link's owner chose and the operator does not control.
  `303` is the one redirect status that mandates a `GET`, so the body stops here.
  Nothing to do on upgrade, and `302` instances — the default — never had the
  problem.

- **A webhook batch is now sent all at once, so one unresponsive receiver no
  longer holds up the rest of the instance.** Deliveries were sent one after
  another on the same goroutine that runs every other scheduled job. At the
  default `LINKCTRL_WEBHOOK_TIMEOUT` of ten seconds a full batch of twenty
  therefore took up to two hundred seconds — and for that whole time nothing else
  ran: invitation and verification mail sat in the outbox, automation rules did
  not fire on their advertised minute, custom-domain re-verification did not
  happen, and the analytics rollups went stale. It needed no misconfiguration and
  no attack, only one webhook pointed at a host that accepts nothing and answers
  nothing, which any member who can register a webhook could do by accident.

  A drain now costs one attempt rather than twenty, whatever the backlog is.
  **If you operate a receiver, the visible change is that it can see up to twenty
  concurrent requests from this instance rather than a steady trickle, and they
  no longer arrive in queue order** — deduplicate on the `X-LinkCtrl-Delivery`
  header, as `docs/usage.md` has always said to. Nothing to configure, and the
  retry schedule, the attempt count and the batch size are all unchanged.


- **`LINKCTRL_DESTINATION_BLOCKLIST` now seeds a database table** instead of
  being held in memory. It keeps working and still matches on a label boundary.
  What changed is that its entries gained a reason code and an audit trail, and
  that they are *reconciled* at every boot: a host you remove from the variable
  is retired on the next restart. Entries the instance owner adds directly are
  never touched by that reconciliation.
- **Destination validation error codes are now `<tier>.<rule>`.** `private_address`
  became `unappealable.private_address`, and the old `host_blocked` became
  `low_confidence.operator_blocklist`. **A client branching on these codes needs
  updating.** Codes for malformed input — `required`, `too_long`, `invalid`,
  `no_scheme`, `no_host` — are unchanged.
- **`LINKCTRL_DESTINATION_SCHEMES` may only narrow the allowlist.** Naming
  anything other than `http` or `https` is now refused at startup. Setting it to
  `https` alone still works and is the reason the variable exists.

### Removed

- **`qr_codes.scan_count`.** A dormant column that nothing has ever incremented
  since it was created, dropped rather than wired: incrementing it would have
  cost a database write on the redirect path, and the number it produced would
  have disagreed with the click figures beside it — those exclude bots and
  deduplicate visitors, and a raw counter does neither. A QR scan is now counted
  as a click labelled `qr`, which is strictly more than the counter would have
  said. No instance has a non-zero value in it, so nothing is lost on upgrade.

  This is one of the two non-additive schema changes in this release, and both
  are stated in this file rather than left to be found in a migration. The other
  is the mail outbox erasing the bodies of already-delivered messages, under
  *Fixed*.


- **`LINKCTRL_DESTINATION_BLOCK_PRIVATE_IPS`.** Private, loopback, link-local,
  carrier-NAT and cloud-metadata addresses are refused unconditionally. The
  variable was an off switch on a protection whose beneficiary is the visitor
  whose browser would do the fetching, and the visitor was not the one turning it
  off. A stale line in your `.env` is reported at startup and otherwise ignored.
  **If you relied on it to point links at an intranet, use a hostname that
  resolves there** — hostnames are not checked against these ranges.

- **Anybody can sign themselves up, if the operator switches it on.** There is a
  `/signup` page, and `POST /api/v1/auth/register` honours the same setting.
  `LINKCTRL_SIGNUP_MODE` chooses between closed, invitation-only and open, and it
  is the only way to choose: there is no toggle in the dashboard and no endpoint
  that changes it.
- **A self-registered account gets an organization and workspace of its own**,
  and is their owner. It does not join yours — an invitation is what does that,
  and it gives membership and nothing else. The sign-up form says so in words,
  because switching sign-ups on to add a colleague and getting a stranger with a
  private instance-within-the-instance is exactly the surprise this ordering
  exists to avoid.
- **Open registration confirms the address before the account exists.** The form
  answers `202`, writes a pending row and mails a single-use link that lapses
  after a day; the user, organization and workspace are created when the link is
  followed. So `open` requires a configured mailer, and with none the instance
  stays invitation-only — `/signup` refuses on the way in rather than after a
  password has been typed. Registering the same address again replaces the
  outstanding link, which is what to do when the mail does not arrive.
- **You can manage the people already in your organization.** *Members*, in the
  menu behind your address in the header, lists every membership with its role
  and lets you change or end one. `GET /api/v1/members`, `PATCH` and `DELETE
  /api/v1/members/{id}` do the same. The list needs `members.read` and changing
  anything needs `members.write` — both held by owners and admins, and this is
  the first enforcement of `members.read` anywhere.
- **You can only manage roles below your own.** An admin manages editors and
  viewers, and never another admin — nor themselves, so an admin who wants to
  step down asks an owner. Owners manage every role including each other,
  because an owner already holds everything and refusing would only mean a
  departed co-owner could be removed by nothing but SQL. **The last owner of an
  organization cannot be removed or demoted**, by anybody, including themselves.
  Separately, nobody can hand out a role above their own — the same ceiling an
  invitation carries. The cost is worth knowing before you rely on it: one owner
  and a few admins, with the owner away, means the admins cannot be changed at
  all.
- **You can give somebody a role in one workspace.** It **adds** that role there
  on top of whatever they already hold, and takes nothing away anywhere. There
  is no way to *restrict* somebody to a workspace — "org admin, viewer in
  finance" is not expressible — so the feature can only ever grant more than you
  expected, never less. `POST /api/v1/members` issues one; removing it is the
  same call that removes any membership.
- **A role given in one workspace reaches that workspace only.** Somebody who is
  an admin in one workspace manages the memberships scoped to it, and cannot
  re-role or remove an organization-wide membership, grant themselves into a
  second workspace, issue an invitation, or delete the organization — each of
  those is authorized against a membership that covers the whole organization.
  So owning one workspace is not owning the organization, however completely you
  own the workspace. The member list draws only the controls that will work, and
  the workspace picker on it offers only the workspaces you may grant in.
- **Workspaces can be created, renamed and deleted.** *Workspaces*, in the same
  menu, and `POST /api/v1/workspaces`, `PATCH` and `DELETE
  /api/v1/workspaces/{id}`. Until now registration provisioned one and there was
  no way to add another.
- **Deleting a workspace is refused while it still holds any link**, archived
  ones included. Links, tags and folders all cascade from a workspace and there
  is no trash to restore them from, so deleting one with links in it would be a
  redirect outage for every alias it held, and an archived link keeps its alias
  and its click history. The cost is stated rather than hidden: there is no bulk
  delete and no way to move a link between workspaces, so emptying a workspace
  is one link at a time. An organization's **last** workspace is refused too —
  everybody in an organization has to resolve into one to sign in at all.
- **You can create an organization of your own**, provisioned with a workspace
  and an owner membership in one transaction — the same path registration takes,
  not a second implementation of it. `POST /api/v1/organizations`, and a form on
  the workspaces page that moves you into the new organization once it exists.
- **A new `orgs.create` permission**, granted to the `owner` role and to nothing
  else. On a default instance — signup closed, everybody else arriving by
  invitation — that means the account from the setup form and nobody else, until
  an owner deliberately makes somebody else an owner. It is a role grant rather
  than a check on how an account was created, so there is one authorization axis
  rather than two. It is delegable to an API key: a key's scopes are intersected
  with its owner's role on every request, so an organization made through one
  leaves that key holding exactly what it was minted with.
- **An organization can be deleted.** `org.delete` has existed since the first
  release, held by owners and gating nothing; this is the operation behind it.
  *Workspaces*, in the menu behind your address, carries the control, and
  `DELETE /api/v1/organizations/{id}` does the same. It removes every workspace,
  membership, outstanding invitation and API key in the organization, in one
  transaction. **There is no undo, no trash and no export**, and the id in the
  path has to be the organization you are acting in — anything else is a `404`,
  so a mistyped id deletes nothing. Unlike every other permission added this
  cycle, `org.delete` is not grantable to an API key: an irreversible action
  belongs behind an interactive sign-in.
- **Deleting an organization is refused while it still holds any link**, archived
  ones included, and while it is the only organization on the instance. The first
  is the workspace rule one level up — without it, deleting an organization would
  be a way around the refusal that protects a workspace's links — and it carries
  the same cost: no bulk delete, so emptying a large organization is a link at a
  time. The second is the same reasoning that refuses the last owner and the last
  workspace.
- **A deletion still reserves the aliases of trashed links that had traffic.**
  Neither refusal counts links already in the trash — one you have deleted is one
  you cannot delete again, so counting them would make a workspace undeletable
  until the 30-day window ran out, with nothing you could do about it. Those
  links are destroyed with the workspace or organization, and any alias among
  them that ever received a click is reserved permanently, exactly as the purge
  job and a rename already reserve one. An alias that never received a click is
  released and can be registered again. Without this, tidying up inside the trash
  window handed a live audience's alias back to the whole instance.
- **Belonging to no organization is now a state the product handles.** Deleting
  an organization is *not* refused because somebody would be left with no other
  one. Their account survives untouched, they can still sign in, and they are
  offered an organization of their own instead of an error — until they take it,
  every other page redirects them to that offer. This was the alternative to
  refusing a deletion whenever anyone would be orphaned, which would make the
  first organization on a default instance effectively permanent, and to deleting
  those accounts, which would make one click destroy people.
- **Eight more audit events**: a member added to a workspace, removed or
  re-roled; a workspace created, renamed or deleted; an organization created or
  deleted. A workspace or organization deletion records the name, because the row
  it names is gone and the record is the only trace left. **An organization's
  audit trail survives the organization**, deletion record included.
- **You can invite somebody into your organization.** *Invitations*, in the menu
  behind your address in the header, issues a single-use link that adds one
  person as one role. Emailed when a mailer is configured, and copyable either
  way — on an instance with no relay, which is the default, that link is the
  whole delivery mechanism. `POST`, `GET` and `DELETE /api/v1/invitations` do the
  same, behind the `members.write` permission that owners and admins hold. This
  is that permission's first enforcement.
- **An invitation is tied to the address it names, not to whoever holds it.**
  Accepting requires entering the address it was sent to, so a link forwarded
  into a group chat cannot add somebody nobody chose. The cost is stated plainly:
  pasting one into a channel for "whoever needs this" is now a thing the product
  refuses, and joining under a different address needs a new invitation.
- **The role an invitation carries is capped at the issuer's own.** An owner can
  invite an owner; an admin can invite an admin, editor or viewer, and never an
  owner. `members.write` is grantable to an API key, so automation can bring
  collaborators in — but **an invitation issued with a key carries `editor` or
  `viewer` and nothing above**, whatever rank created the key. Redeeming one
  produces an interactive account that revoking the key does not revoke, so
  without that second bound a key holding one delegable scope could mint an
  account holding the scopes no key may hold. Automation that issues `admin`
  invitations through a key now gets a `403` when it asks, rather than an
  account with more authority than the key that made it.
- `LINKCTRL_INVITE_TTL`, defaulting to `168h`. The clock starts when the
  invitation is created rather than when the mail leaves, because delivery is
  asynchronous through the outbox — so a slow relay spends the window, which is
  why it is tunable. Expiry cannot be switched off; revoking is how an invitation
  ends early.
- **Accepting an invitation adds a membership and nothing else.** No personal
  organization or workspace is provisioned for the person joining: they are a
  colleague in an organization that already exists, not a tenant of their own.
  The account is otherwise ordinary. This is also the first way an account can
  hold two memberships, so it is the first time the workspace switcher in the
  header has anything to switch between.
- **`LINKCTRL_SIGNUP_MODE=closed` now means closed to invitations too.** Under
  `closed` an invitation may only add somebody who already has an account;
  `invite` and `open` let it create one. `invite` previously behaved exactly as
  `closed`, and the configuration reference said so — that sentence is gone
  because it is no longer true.
- **Every way accepting can fail answers identically.** An unknown token, an
  expired or revoked or already-used one, the wrong address, the wrong password,
  somebody who is already a member, or an address with no account on a closed
  instance: one status code, one body, and the same password-hashing cost spent
  on each. Anything else would let whoever holds a link ask the server whether a
  given address has an account here.
- Issued, revoked and accepted are audit events, and accepting notifies whoever
  sent the invitation in their dashboard inbox.

- **The dashboard header has an identity menu and a notification bell.** Your
  email address was inert text with Account and Sign out scattered around it, and
  Notifications was a nav link whose badge could say how many but never what.
  The address is now the control: Account and Sign out live under it, and the top
  level is down to Dashboard, Links and API keys. The bell keeps the unread count
  and, opened, previews the newest few unread notifications with a **View all**
  link. `/notifications` is unchanged and is still the full surface.
- Both menus are built on the Popover API. They open from a keyboard, close on
  Escape or a click outside, and need no JavaScript — no page in the dashboard
  runs an inline handler. They are **not** ARIA menus: a screen reader announces
  a button and a group rather than the `role="menu"` pattern, which is the
  honest trade for needing no script.
- **The dashboard now needs a browser from mid-2023 or later**: Chrome 114,
  Safari 17, Firefox 125. That is what the Popover API costs. In anything older
  the two panels render as ordinary blocks in the header — untidy, but Account,
  Sign out and the notifications link all stay on the page.
- **The bell costs no extra database work.** The header already ran one query per
  page render for the unread count; it now returns the count and the preview
  together, so a page still issues exactly one — asserted by a test that counts
  what reaches Postgres.

- **The audit log has behavior.** The table shipped in 0.1.0 with nothing writing
  to it; there is now a writer, a read API, and a retention policy of its own.
  Changing the link domain's root redirect is the first recorded action — the
  setting that sends every stray visitor somewhere, and the one 0.1.0 said would
  become an audit event once there was an audit log to put it in.
- `GET /api/v1/audit` lists an organization's records newest-first with keyset
  pagination, gated by a new `audit.read` permission granted to owners and
  admins. **It cannot be granted to an API key.** Reading the log is the one
  place a network prefix is tied to a named person, so it requires a signed-in
  session; a key requesting the scope is refused when it is minted.
- `LINKCTRL_AUDIT_RETENTION_DAYS`, **defaulting to `0` — keep forever.**
  Deliberately not the analytics default: an upgrade must never silently start
  deleting history an operator assumed permanent. `audit_logs` partitions are
  now dropped under this window and never under the analytics one.
- `linkctrl_audit_log_bytes`, the on-disk size of every `audit_logs` partition,
  refreshed hourly on every replica. Keeping everything forever is only a safe
  default if the growth it permits is visible; the Prometheus alert recipe is in
  [docs/operations.md](docs/operations.md#audit-log-growth).
- **The dashboard has a dark theme.** With nothing chosen it follows
  `prefers-color-scheme`, with no cookie, no account and no JavaScript involved.
  An **Appearance** control in account settings overrides that with System,
  Light or Dark, and the sign-in page carries the same control — the preference
  is per browser, so it has to be settable before you have an account to sign
  in to.
- The choice is stored per browser rather than on the account, so it works
  before you sign in and two browsers on one account may disagree. Deliberate.
- **There is no flash of the wrong theme.** The server reads the cookie and
  renders the theme onto `<html>`, so the first response is already correct.
  Nothing corrects it afterwards because there is nothing to correct.
- **Two light-theme colours changed.** The quietest text — timestamps,
  "(optional)" hints, empty states — was `slate-400` and `slate-300`, which
  measure 2.56:1 and 1.48:1 against white and fail WCAG AA. Both are now darker.
  An accessibility claim that exempted the theme already shipped would not have
  been one; the contrast figures for every token pair, in both themes, are
  recorded beside the definitions in `internal/ui/static/css/input.css`.
- Not themed, deliberately: `/docs`, whose Swagger UI is vendored and
  checksum-pinned.
- **Credential and API rate limits are shared across replicas.** They are
  enforced in Redis, so the configured rate is the instance's rate rather than
  each replica's — an attacker spreading a credential-stuffing run across
  replicas no longer gets the limit multiplied by however many are behind the
  load balancer.
- On any Redis error the limiter falls back to the per-replica bucket it always
  had. It still limits, just once per replica, and it never starts refusing
  requests because Redis is unwell: a limiter is abuse mitigation, not an
  authorization boundary. The fallback is logged once when it begins.
- **The 404-probe limiter is deliberately not shared**, and will not be. A Redis
  round trip on the redirect path would put an optional dependency inside the
  20ms budget.
- **Cache invalidation now crosses replicas.** Editing a link on one instance
  clears every instance's cache, over a Redis pub/sub channel, instead of only
  the one that handled the edit. Running more than one app replica no longer
  means an edit takes up to `REDIRECT_TTL` to become visible everywhere — the
  limitation 0.1.0 shipped with, and the reason it told you to run one instance.
- With Redis down this degrades rather than breaks: redirects still resolve from
  Postgres, edits still apply and still clear the replica that made them, and
  the other replicas fall back to the TTL staleness they had before. A
  subscriber that stops hearing invalidations **flushes its in-process caches
  then, and again when it reconnects**, because Redis pub/sub does not replay
  and a replica cannot know which invalidations it missed. The cost is a cold
  cache after a Redis blip; the alternative is serving a destination the owner
  already changed.
- **A Redis that stalls counts as one that stopped hearing.** A connection held
  open with nothing coming down it looks exactly like a channel nobody has
  published on, so `LINKCTRL_REDIS_SUBSCRIBER_READ_TIMEOUT` (30s) bounds how
  long a replica accepts silence before it pings and waits for the reply — the
  part a stalled connection cannot produce. Unanswered, it says so in the log
  and drops what it can no longer vouch for. Without this a wedged Redis, proxy
  or sidecar left a replica serving pre-edit destinations for the whole
  `REDIRECT_TTL` with nothing reporting a problem.
- **Notifications, in the dashboard.** An unread count in the header, a
  notifications page and `GET /api/v1/notifications` with mark-read and
  mark-all-read. (The count began as a nav link and is now the bell described
  above; both shipped unreleased, so this describes where it ended up.) Your own
  inbox
  only — there is no permission for reading somebody else's, because there is no
  reason for one — and no endpoint that creates a notification: they record what
  the system observed, not what a caller asserts.
- The first thing that raises one is the audit-log size threshold above.
  `LINKCTRL_AUDIT_SIZE_WARN_BYTES` defaults to 5 GiB and is **on by default**,
  which is deliberately the opposite of the retention default: keeping
  everything forever is only safe if the instance nobody configured is the one
  that gets warned. Owners are told, at most once a week each, and only owners —
  nobody else can change the setting.
- **A workspace switcher, in the dashboard and at
  `GET /api/v1/workspaces`.** Which workspace a request acts in is now resolved
  in one place for every credential, with a switch that moves the browser that
  asked and is remembered for the next sign-in. `POST
  /api/v1/workspaces/{id}/switch` and `PUT /api/v1/workspaces/default` are the
  API half; both need a signed-in session: switching moves the calling session,
  which a key has not got, and remembering the choice must not repoint where the
  key's owner lands. It does not follow that a key is unaffected by a switch —
  the organization-wide keys added later in this series resolve a workspace per
  request, so one made in a browser moves theirs too unless a default is pinned.
- **Where a new sign-in starts** follows the workspace you used last. An
  *Account* setting pins one instead, and offers **Last-Used** as its first
  option — the derived behaviour stays the default, and the override is there
  for anyone it annoys. The pin applies to sessions started after it; switching
  from the header still moves the browser you are in.
- The header control **draws nothing while you have one workspace**, which is
  every account on every instance today: nothing creates a second membership
  yet. This release is the groundwork that lands before invitations do, so the
  identity resolution underneath is settled before a feature starts producing
  memberships.
- **An optional SMTP mailer, off by default.** Set `LINKCTRL_SMTP_HOST` and the
  instance can send mail; leave it empty — which is the default — and nothing
  changes at all: no outbound connection, nothing queued, and every feature that
  could email behaves exactly as it does today. It lands before the features
  that need it so email is part of each one from its first commit rather than
  retrofitted.
- **Sends never happen on the request path.** A message is written to a new
  `mail_outbox` table and delivered by a job on the scheduler that already
  maintains partitions, so mail queued before a restart is still delivered
  after it. Retry is bounded — five attempts backing off 1m to 16m — and a
  message that never got through is kept, with the relay's error, rather than
  disappearing.
- **The audit-growth warning is the first thing that emails.** Owners already
  got it in the dashboard; with a mailer configured they get it by email too,
  under the same once-a-week guard. The in-app notification remains the
  baseline and does not depend on the mailer.
- Messages are **plain text only**. No HTML part means no remote image that
  reports when a message was opened and no anchor text that disagrees with its
  link, and every value put into a message has its control and
  bidirectional-formatting characters stripped first — so nothing anyone types
  can inject a header or forge a second message.
- The supported configuration is deliberately narrow: STARTTLS, implicit TLS, or
  an unencrypted local relay, with PLAIN authentication over an encrypted
  connection. LOGIN, CRAM-MD5, XOAUTH2 and client certificates are not
  supported, and `SMTP_TLS=starttls` **refuses to send** rather than falling back
  to plaintext against a relay that does not offer it.
- `LINKCTRL_SMTP_PASSWORD` **exists again.** It was accepted and ignored before
  0.1.0, then removed because there was no mail feature to authenticate to.
  There is one now.

### Fixed

- **A failed custom-domain check could write its refusal onto a hostname it never
  checked.** Verification reads a row, asks a nameserver about the hostname it
  read — seconds, against a server the domain's owner runs — and writes the result
  back. Renaming the domain in that gap is something its owner is entitled to do
  at any moment, and the successful path already refused to land on a row that had
  changed. The failing path did not: the new hostname acquired an error message
  about the old one, shown on the Domains page, and a "last checked" timestamp it
  had not earned — which pushed it behind every genuinely unchecked registration
  in the re-verification queue. Both paths now write only onto the row whose name
  and token were actually proved, and the on-demand check answers *this changed
  while it was being checked, try again* instead. **No hostname's serving status
  is affected in either direction**; what was wrong was what the page said and
  whose turn came next.

- **A timeout the service set on a Redis call did nothing; `REDIS_READ_TIMEOUT`
  was the only thing bounding a stalled cache.** The client was not configured to
  apply a caller's deadline to the socket, so against a Redis that accepts a
  connection and never answers, a call asking for 50ms still took the full read
  timeout — measured at 401ms with the read timeout at 400ms, and 50ms once the
  option is set. **No default moved and no knob changed meaning**:
  `REDIS_READ_TIMEOUT` is still the ceiling on every Redis command, and a caller
  asking for longer — the readiness probe, the returning-visitor batch — is still
  held to it. What changes is a caller asking for *less*, which on the redirect
  path means a lookup starting late in the request budget no longer spends time
  the request does not have. Both hand-built defences around this are unchanged
  and still needed: the invalidation budget bounds a three-attempt loop, and the
  shared rate limiter bounds what the request waits rather than what one command
  costs.

- **`docs/configuration.md` said the IPv6 rate limit is keyed by /64 and left
  out what that means for a subscriber holding more than one.** A single host is
  routinely handed a whole /64, which is why the key is not the address — but a
  site delegated a /56 or a /48 holds 256 or 65536 distinct /64s and gets a
  bucket for each, so the number you configure applies that many times over to
  one customer. The document now says so, and says why the key is not made
  coarser: it would let one abusive host throttle its neighbours. The keying
  itself is unchanged.

- **`docs/cli.md` described `--org-wide` as issuing a key "valid in every
  workspace of the organization", which reads as a key acting in all of them at
  once.** There is no per-request workspace selector: such a key resolves a
  single workspace per request the way a sign-in does — your pinned default,
  otherwise the workspace you used last — bounded to the organization the key
  was issued in, which is what `docs/SECURITY.md` and `docs/usage.md` already
  said. So a key like this follows the switch you make in a browser. Behaviour
  is unchanged; the flag's description is.

- **`docs/SECURITY.md` said every invitation-redemption failure costs the same
  argon2 work, and four of them cost twice as much.** The property that matters
  is intact — nothing tells you whether an address has an account here, because
  every failure a stranger can reach costs exactly one hash. The four that cost
  two are reached only after a correct password for the invited address was
  verified, or by the losing half of two simultaneous redemptions, and anybody
  in that position can sign in as the account and read its memberships anyway.
  The claim now says which is which rather than overstating it.

- **The review queue now names the blocklist entry Allow deletes, and the entry
  is fixed when the dispute is filed.** The runtime blocklist matches on label
  boundaries, so somebody refused at `login.evil.example` was refused by the row
  that says `evil.example` — and Allow deleted that row, for every workspace on
  the instance, while the queue displayed only the host that was typed. Nothing
  on the page or in the API said which entry would go. Worse, the entry was
  worked out again at the moment of the click rather than at filing: a more
  specific row added while the dispute waited silently retargeted a decision the
  owner believed they were making about what they had been shown.

  Each dispute now carries the entry it is about, `blocked_host_defanged` in the
  API, rendered beside the host on `/disputes`. Existing disputes carry no entry
  and Allow refuses them rather than guessing — uphold and file again. Upgrading
  needs nothing.

- **One blocked host is now one dispute, not one per subdomain of it.** The
  "already waiting for review" bound counted the host as typed, so a single
  listed entry admitted a fresh dispute — and a fresh notification to every
  organization owner on the instance — for `login.`, `mail.`, `a.`, and any
  other prefix somebody cared to type. It is counted per blocklist entry now. A
  refusal with no entry behind it — a punycode homograph, credentials before the
  host, a reputation-feed verdict — is still bounded by the host alone, because
  there is no row to count instead.

- **An A/B test on a password-protected link no longer sends everybody to the
  same arm.** A sequential split chooses its arm before the password is asked
  for, and the challenge page and the form that answers it are two requests — so
  every visit advanced the rotation twice and served the second position. At an
  even number of arms half of them received no traffic at all, and at two arms
  the first arm was never served to anybody. The click history recorded the arm
  that was actually served, so the reports agreed with the skew rather than
  exposing it. A request that is about to be shown the password page now chooses
  no arm, and one visit advances the rotation once. Weighted splits, and
  sequential splits on links without a password, are unaffected; a *wrong*
  password still spends a position, as any refusal does.

- **The redirect path now has a database deadline.** Password verification,
  signature-key reads, click-budget writes and a sequential split's rotation all
  ran with no time limit of any kind: a query that could not proceed held a
  connection for as long as PostgreSQL would let it, while other requests queued
  behind it. Each of those calls is now bounded by `REDIRECT_TIMEOUT`, the same
  setting that already bounded the redirect's own lookup — no new configuration,
  and no change on any instance where these queries were already fast. The bound
  follows the visitor: somebody who closes the tab mid-request does not spend a
  one-time link's only click.

- **A hostname spelled with a trailing dot, or with an explicit port, now reaches
  the hostname it names.** `Host: go.example.com.` is the fully qualified
  spelling of `go.example.com` and `Host: go.example.com:8080` names the same
  host on a non-default port; neither matched a verified custom domain. On a
  deployment serving the dashboard and short links on one hostname the
  consequence was serious: the request fell through to the instance's own tree,
  so a customer's verified hostname answered with the dashboard, the API, and
  short links belonging to the instance's default domain. On a split-hostname
  deployment the same spelling of a *configured* hostname was answered with the
  operational 404. Both are folded now. A non-default port is still significant
  when matching the instance's own hostnames, because a deployment may serve the
  dashboard and short links on one name and two ports.

- **Bot blocking now applies to links on custom domains.** Turning blocking on —
  even *enforced* — wrote the setting to the instance's default domain alone,
  while a link is served under the policy of the domain it is on. Since a
  workspace's own verified hostname becomes the default for its new links, every
  link created there was unprotected, and neither surface said so. The setting
  now reaches every registered hostname, and a hostname registered afterwards
  inherits it, which is what "instance-wide" has always meant on this page. There
  is still no per-hostname bot policy, deliberately: `domains.write` is not
  narrow enough to hold one.

  A link's own page also read the **default** domain's policy for every link, so
  a link on a custom hostname could have its bot control disabled by another
  domain's setting, with the explanation naming a hostname the link is not served
  on — while the API would have accepted the change the page refused. The page
  now reads the domain the link is actually served on.

- **`/feeds` no longer tells you nothing leaves when your own webhooks are
  receiving your destinations.** With no reputation feed configured, that page
  put *"No destination leaves this instance"* in a green panel for every
  signed-in account, and added *"Nothing you point a link at is sent anywhere"*.
  A webhook makes both false: the five link-lifecycle events carry the
  destination exactly as it was typed, `destination.blocked` carries the refused
  attempt defanged, and no operator setting turns any of it off — registering one
  is an owner's or an administrator's decision inside a workspace. The page was
  reading the feed setting and nothing else, so there was no state in which it
  could have been right about the second channel.

  It now reads your workspace's registrations as well, and answers for both
  channels in every combination. The reputation feed is the operator's and its
  answer is instance-wide; a webhook is your workspace's and its answer is about
  your workspace alone — neither is stated as the other, so an instance where the
  strong claim is true can still make it. `GET /api/v1/feeds` gained a
  `webhooks` object saying whether anything enabled there is subscribed to an
  event that carries a destination, and how many are: **an addition, so every
  field that response already carried is unmoved.** It carries a count and never
  an address, because reading who a workspace posts to needs `webhooks.read`
  while this disclosure needs no permission at all.

  If your instance has no feed and no webhooks, the page says what it always
  said, in different words. If it has webhooks, it now says so — which is the
  point, and may be the first time anybody there is told.

- **`lctl demo`'s own description said no destination left the instance, and its
  dataset registers webhooks.** The demo seeds two registrations because a page
  with none shows nothing, one of them enabled and subscribed to four
  link-lifecycle events, so every link created on a demo instance queues a
  delivery carrying that link's destination. Both hostnames are `.example`,
  which never resolves, so nothing reaches anybody's server — but *no destination
  leaves this instance* was the wrong sentence for it, and `docs/cli.md` repeated
  it. Both now say what is true: no destination reaches a third-party feed, and
  the webhooks are there.

- **`docs/SECURITY.md` counted two outbound connections and there are four.**
  The *Egress* row named an SMTP relay and a reputation feed, said both were off
  until an operator configured them, and added that nothing else in the product
  opens a socket outwards. Webhook delivery was the third — shipped, documented
  two rows above, and with no operator setting anywhere in its path — and the
  hourly DNS `TXT` query custom-domain re-verification makes is the fourth. The
  row now enumerates all four rather than counting them, says who decides each,
  names what it excludes and why (Postgres, Redis, and the health check dialling
  this process's own listener), and states plainly that the DNS query is the
  weakest member and is counted anyway.

- **Signing in no longer says whether an address has an account here.** Five
  wrong passwords against a registered address answered `429` with problem type
  `account-locked`; five against an unregistered one answered `401`
  `invalid-credentials`. Five attempts fit inside `LINKCTRL_LOGIN_RATE_PER_MIN`,
  so nothing masked the difference, and the sign-in form said the same thing in
  prose — *"the account is locked briefly"* against *"the email or password is
  incorrect"*. Anybody with a list of addresses could sort it into accounts and
  non-accounts, unauthenticated, on an instance with `SIGNUP_MODE=closed` where
  the registration endpoint refuses before it looks anything up — and asking cost
  each of them a fifteen-minute lockout.

  Every sign-in failure is now one answer on both surfaces: same status, same
  problem type, same body, same words on the page. The refusal also costs the
  same, which was the second half of it — a locked account used to refuse without
  hashing anything, so the question was answerable with a stopwatch even once the
  status codes matched.

  **If you have a client that branches on `account-locked`, it will not see that
  type again**; a locked account is a `401 invalid-credentials` like every other
  refusal. `rate-limited` is now the only problem type this API answers `429`
  with. The lockout itself is unchanged — five failures, fifteen minutes, and
  `LINKCTRL_LOGIN_LOCKOUT_THRESHOLD=0` still turns it off. The refusal names it in
  words whether or not one is in force, so somebody sitting out their own lockout
  is still told why waiting helps; an operator who needs to know which account is
  locked reads `users.locked_until`, and `docs/operations.md` says how.

- **A stalled SMTP relay no longer holds up every other background job.**
  Queued mail was sent one message after another on the single goroutine that
  runs every scheduled job, so at the default `LINKCTRL_SMTP_TIMEOUT` of ten
  seconds a full batch of twenty took up to two hundred seconds — and for that
  whole time nothing else ran: webhook deliveries did not go out, automation
  rules missed the minute they advertise, custom domains were not re-verified and
  the analytics rollups went stale. It needed no attack and nothing a tenant
  could do, only a relay that accepts connections and then says nothing, which is
  what a firewalled SMTP host looks like from here. This is the same defect fixed
  for webhook delivery earlier in this release, in the package immediately beside
  it.

  A drain now costs one attempt rather than twenty, whatever the backlog is.
  **The visible change is that this instance can open up to twenty connections to
  your relay at once rather than one at a time.** If your relay caps concurrent
  connections below that it will refuse the extra ones; a refused message is a
  spent attempt that retries with backoff, so nothing is lost, but an outbox held
  continuously above that cap for five attempts will abandon the overflow.
  Nothing to configure, and the batch size, retry schedule and attempt count are
  all unchanged.

- **A delivered invitation left its token readable in the database.** Mail is
  queued rendered, and an invitation's message contains the single-use link — so
  while the `invitations` row stored only a hash of the token, the outbox row
  beside it held the token itself, in clear, for the thirty days a finished
  message is kept. Anyone who could read the database could take a token out of
  `mail_outbox.body` and redeem it, at whatever role the invitation carried,
  including `owner`. The same applied to address-verification mail under open
  sign-ups.

  A message now loses its body in the same statement that marks it sent or
  failed, and the database refuses to hold a finished row that still has one.
  What an operator reads when somebody says the mail never arrived — recipient,
  subject, kind, attempts and the last error — is untouched. **On upgrade, the
  bodies of messages already delivered are erased**; if you were reading them for
  anything, that stops working. Nothing else changes and no message in flight is
  affected.

  What remains, and it is in `docs/SECURITY.md` rather than implied: a message
  that has **not** been delivered still holds its token, because it is the
  message about to be sent. On an instance whose relay is down, that lasts as
  long as the invitation is redeemable — `LINKCTRL_INVITE_TTL`, seven days by
  default.

- **A feed's API key was written to the log on every feed failure.** A reputation
  feed that timed out, refused the connection or failed DNS was logged at `WARN`
  with the whole configured URL in the message — including the credential in its
  query string, and on `FEED_METHOD=GET` including the destination the user had
  just typed. A two-second timeout is the ordinary case, so this reached the log
  stream on a routine error path, which is where logs get shipped elsewhere and
  pasted into tickets. The message now names the same endpoint `/feeds` shows —
  scheme, host and path — and the cause, so an operator can still tell which feed
  stopped answering. Nothing to do on upgrade beyond rotating a feed credential
  that has been through a log you do not control.

- **`/feeds` printed a feed credential written into the URL.** The disclosure
  page strips a query string, but `https://apikey:secret@feed.example/` was shown
  verbatim to every signed-in account — and under open sign-ups that is anybody
  who registers. Go sends URL userinfo as Basic auth, so the credential worked
  and nothing warned about it. Two changes: the endpoint on `/feeds` and on
  `GET /api/v1/feeds` is now built from the scheme, host and path rather than by
  removing the parts known to be secret; and **`LINKCTRL_FEED_URL` with a
  username or password in it is refused at startup**, pointing at
  `LINKCTRL_FEED_AUTH_HEADER` and `LINKCTRL_FEED_AUTH_TOKEN`, which are redacted,
  unset from the environment after parsing and readable from a mounted file.

  **This stops an instance booting if it had one.** Move the credential to
  `FEED_AUTH_TOKEN` before upgrading, and rotate it — it has been on a page every
  signed-in user could read. A key written into the **path** of `FEED_URL` is not
  detectable and is still printed; `docs/configuration.md` says so.

- **A workspace-scoped role reached the whole organization's invitations.**
  Somebody granted `admin` in one workspace could list every pending invitation
  in the organization — each invitee's address, the role they were offered and
  who offered it — and revoke any of them. Revocation cannot be undone and that
  actor could not issue a replacement, so it was a way to stop an owner staffing
  their own organization. Issuing an invitation already required an
  organization-wide membership; listing, revoking and the role list now do too.
  Nothing needs to be done on upgrade. If you granted somebody a role scoped to
  one workspace, they will now get a permission error on `/invites` where the
  page used to open.

- **A workspace-scoped owner was told about every other workspace in the
  organization.** Custom-domain warnings carry the hostname and automation
  notifications carry the link aliases a rule matched, and both went to everybody
  holding `owner` anywhere in the organization — including workspaces the
  recipient holds no membership in and cannot open. Recipients are now the
  organization-wide owners plus the owners of the workspace the news is about, so
  a workspace-scoped owner still hears everything about their own workspace and
  nothing about anyone else's. Notifications already delivered are untouched.

- **An API key kept working after its owner was removed from the organization.**
  The credential still authenticated, still carried the organization's tenancy,
  could create an organization it had no scope for, and could rotate itself into
  a fresh key indefinitely — so an administrator who removed somebody could not
  actually stop the credentials they left behind. A key whose owner holds no
  membership covering it now fails authentication outright, and rotation re-checks
  the same thing under its own lock. **This can break a working integration**: a
  key whose owner has since left, or whose owner's role was narrowed to a
  different workspace, stops authenticating on upgrade rather than continuing with
  no permissions. Reissue it from an account that is still a member.

- **Nobody but a key's own owner could revoke it.** `DELETE /api/v1/api-keys/{id}`
  and the revoke button only ever matched your own keys, so a credential that had
  to be stopped — leaked, or belonging to somebody unreachable — had no answer but
  its owner's cooperation. `apikeys.write` held through an organization-wide
  membership now revokes any key issued into that organization, and writes an
  `apikey.revoked` audit record naming whose key it was. Revoking your own key is
  unchanged and still writes no record. A key you may not act on still answers
  404 rather than 403.

- **An API key holding `members.write` could promote an existing member to
  `admin`.** The cap that stops a key *inviting* somebody at `admin` did not
  cover assigning a role to somebody already in the organization, so the same key
  reached the same outcome through the members page instead of through an
  invitation — and the resulting account holds `apikeys.read`, `apikeys.write`
  and `audit.read`, none of which a key may hold, in a principal that revoking
  the key does not take back. A role assigned with an API key is now capped at
  `editor`, on both the change-role and the workspace-grant paths, and the role
  list a key is offered matches. A signed-in administrator promotes exactly as
  before.

- **The webhooks and automation pages returned "Link not found".** Both are
  linked from the sidebar, both are documented in `docs/usage.md`, and both
  answered 404 for everybody on every deployment shape, as did all nine of their
  forms. The handlers were there and the API worked throughout; what was missing
  was the entry that attaches those paths to the dashboard, so the requests fell
  through to the short-link handler and were answered as if somebody had followed
  a link that does not exist. Nothing needs to be done on upgrade, and no data
  was affected — the pages simply start working.

  The list those entries lived in is gone. The dashboard's paths are now produced
  by registering the pages themselves, so a page cannot be added and left
  unreachable, and the test that guards the reserved-word list reads the same
  registrations instead of reading that list back to itself.

- **Two organizations with the same name, created within about a minute of each
  other, failed with an internal error.** The suffix that makes an organization
  slug unique was taken from the front of its UUIDv7 — which is the timestamp,
  and whose leading characters are identical for everything created inside the
  same 65-second window. It now comes from the random half of the id. This was
  reachable before only by two accounts registering with the same display name;
  creating organizations by hand is what makes it ordinary.

- **`lctl demo --reset` claimed running it twice produced the same demo, and
  produced a different amount of click history each time.** The month of traffic
  it backfills is anchored to the clock, and the clicks generated for the part of
  today that has not happened yet are discarded — but discarding one used to
  consume less of the seeder's random number stream than keeping it, so a single
  click landing on the far side of that line re-rolled the traffic for every
  remaining link and every remaining day. Two runs a few seconds apart could
  differ by hundreds of clicks, in either direction. Two changes: the discard no
  longer moves the stream, and the history now ends at the top of the current
  hour rather than at the instant the command ran, so every run inside one hour
  writes the same history. The demo loses its newest hour of traffic out of
  thirty days, and gains being reproducible.

### Notes for operators

- **`LINKCTRL_SIGNUP_MODE` keeps its meaning and gains a browser.** It was read
  only by `POST /api/v1/auth/register`; it now governs the `/signup` page as
  well. It is still set in the environment and nowhere else — no runtime toggle
  was added, deliberately, because an instance-wide setting held on the owner
  role would be held by everyone who signed up on an open instance.
- **`open` needs `LINKCTRL_SMTP_HOST`.** Public registration confirms the address
  before creating the account, so with no relay the effective mode is
  invitation-only whatever the variable says. `/signup` answers 403 and the
  server states the derivation once at boot.
- **`POST /api/v1/auth/register` answers `202`, not `201`,** and no longer
  returns a user id. Nothing exists when it returns; the account is created by
  the emailed link. A client that expected `201` and a `user_id` needs changing.
- One additive table, `pending_registrations` — the waiting room for an address
  that has not been proven yet. Lapsed and spent rows are swept hourly. No new
  permission.
- Every record stores a network prefix — /24 for IPv4, /48 for IPv6 — and never
  an address, matching what sessions already did. The actor's label is
  snapshotted when the event is written, so a record stays readable after the
  account it names is deleted.
- Nothing is recorded retroactively. The log starts at the upgrade.
- Notifications added no database columns. The table shipped in 0.1.0 and
  per-kind detail goes in its `data` jsonb, so this upgrade is additive in the
  ordinary way and needs no backfill.
- There is no push, and no per-event preference machinery. In-app is the
  baseline; email is an addition an operator switches on.
- The switcher adds three nullable columns —
  `users.default_workspace_id`, `users.last_workspace_id` and
  `sessions.workspace_id` — all NULL after the migration, which is why
  resolution falls through to the workspace every account already resolved to.
  Additive, no backfill.
- Nothing about tenancy has changed yet: one personal organization and one
  workspace are still provisioned per account, and no product surface creates a
  second membership. What changed is that when one exists, it will be reachable.
- The mailer adds one table, `mail_outbox`, and changes nothing else. An
  instance that never sets `LINKCTRL_SMTP_HOST` will never write a row to it.
- **If you configure a mailer, own the sending domain's DNS.** SPF, DKIM and
  DMARC are set where the domain lives, not here. Nothing signs outbound mail.
- Queued messages are stored rendered, so anyone who can read the database can
  read them. Sent and failed rows are deleted after 30 days; pending rows are
  never deleted by age.
- **An edit made while Redis has stopped answering now has a stated bound.**
  `LINKCTRL_REDIS_INVALIDATE_BUDGET`, default `250ms`, is the most an edit will
  wait for the cache to be told a link changed — the total across all three
  attempts rather than each. Previously each attempt got its own
  `REDIS_READ_TIMEOUT`, so raising that knob multiplied it by three into the
  latency of a form submission. Nothing about a healthy or briefly stalled cache
  changes: every retry that used to succeed still fits inside the budget.
- The bound covers a Redis that accepts a connection and never answers, and one
  that answered and then went quiet mid-command. Both are bounded because the
  caller stops waiting rather than because any single command gives up: three
  attempts each entitled to `REDIS_READ_TIMEOUT` is three times that knob, which
  is what made this worth fixing rather than tuning.
- **A bounded failure is still a failure to invalidate.** The edit is committed
  either way, the failure is logged, and the stale cache entry expires with
  `REDIRECT_TTL` exactly as before. Redirects are untouched, and were measured
  rather than assumed: nothing on that path retries, so a stalled cache costs a
  redirect one `REDIS_READ_TIMEOUT` per call and then Postgres answers it. A
  redirect served from memory costs nothing; a cold one measured 108ms, since it
  pays the timeout on the lookup and again on the write that would have
  refilled the cache.

## [0.1.0] - 2026-07-31

First release, and all of Phase 1's twenty-one milestones: a self-hostable link
manager where a short link is an editable, measurable, scriptable resource.

### Links

- Create, edit, archive and soft-delete links. A trashed link holds its alias for
  30 days; the hourly purge then deletes it, permanently reserving any alias that
  ever received traffic. There is no trash view in this release, so recovery
  inside that window is a database operation and the interface says so rather
  than implying a button. Editing a destination never changes the short URL —
  the reason redirects are always 302.
- **Renaming a link reserves its old alias** on the same rule the purge uses: if
  it ever received a click it can never be reissued, to anyone. Abandoning an
  alias is not the same as freeing it — the old one is still on printed material
  and in other people's bookmarks, and handing it to a different destination is
  a redirect hijack.
- Custom or generated aliases, lowercase-canonical and case-insensitive. Dots are
  refused outright, which removes the "is `logo.png` an alias or an asset?" class
  of problem rather than pattern-matching for it.
- Tags, titles, descriptions and expiry. An expired link answers `410 Gone`, not
  `404`, so crawlers and link checkers stop retrying, and it reports as expired
  in the dashboard and the API too — the status is derived from the expiry
  wherever it is shown or filtered, never written to a column that would be stale
  between the deadline passing and something noticing.
- Per-link query forwarding, off by default: the visitor's query string is merged
  into the destination, whose own parameters win on conflict.
- Full-text and substring search, filtering by status, sorting, and cursor
  pagination. Offsets are not offered: they re-scan skipped rows and silently
  duplicate or drop entries when links are created mid-page.
- An alias that has ever received traffic is never reissued after purge.

### Redirects

- In-process cache, then Redis, then Postgres, with negative caching for the
  unknown aliases a public shortener is mostly asked for.
- Redis is optional at runtime. Losing it makes redirects slower, never wrong.
- A dedicated connection pool for the redirect path, so a slow analytics query
  cannot leave a redirect waiting for a connection.
- **Measured**: 100% of 240,001 cached redirects answered under 20ms at a sustained
  2,000 rps, with 100k links and 5.7M click events in the database and the
  analytics rollup running throughout. See [docs/slo.md](docs/slo.md).

### Hostnames

- **The dashboard and short links can be served on separate hostnames**, via
  `LINKCTRL_APP_BASE_URL` and `LINKCTRL_LINK_BASE_URL`. Both default to
  `LINKCTRL_BASE_URL`, so a single-host deployment needs no configuration at all.

  Set to different hosts, each answers only its own paths: the dashboard host
  stops resolving aliases, the link host stops serving the dashboard, the API and
  the static assets. A request to the wrong host is `404`, never a redirect to
  the right one — a cross-host redirect reachable through the alias namespace
  would be an open redirector for anyone able to create a link.

  The point is the session cookie. It carries the `__Host-` prefix, which forbids
  a `Domain` attribute, so once the hosts differ a browser will not send it to
  the host serving short links — the half of the product that gets pasted into
  forums and probed by strangers, and the half that needs no credentials at all.

  `/healthz` and `/readyz` answer on every hostname, including ones never
  configured: probes come from load balancers and container runtimes that do not
  know the operator's names. Still one listener and one process.

- **The link domain's root can be pointed somewhere**, for the visitor who trims
  a short link back to the bare domain. Unset it answers `404`, and there is no
  default page — an instance that says nothing about itself is a legitimate
  choice. Setting it needs the `domains.write` permission, held by owner and
  admin, because this is not one link but where every stray visitor to the whole
  domain ends up. The destination is validated exactly as a link's is, which
  matters most here: reaching it needs no link and no alias. Cached and
  invalidated on change, so it costs no query per request and takes effect
  immediately.

### Analytics

- Clicks, estimated unique visitors, bots, device, browser, OS, language and
  referrer host, from daily rollups rather than raw events.
- Country, with an operator-supplied MaxMind database. Region and city are
  resolvable from the same file and deliberately not stored.
- Retention enforced by dropping whole monthly partitions, which is instant and
  reclaims the space. Rollups survive, so charts outlive the raw events.
- **No IP address is stored in any column.** A visitor is
  `HMAC(daily salt, ip ‖ user-agent ‖ workspace)`, and the salts are deleted after
  two days — that deletion is the de-identification step, not housekeeping.
  Referrers are reduced to a host at ingest.
- Unique-visitor figures are therefore estimates at daily resolution, and every
  API response carrying them says so.

### Authentication and authorization

- Email and password with argon2id, server-side sessions in `__Host-` cookies,
  and per-account lockout.
- Real RBAC with four built-in roles and a working permission evaluator, not a
  hardcoded owner check.
- API keys as `lk_live_…` bearer tokens, scoped to permissions the owner holds and
  intersected with their current role on every request. Only the HMAC is stored.
- Keys can never hold `apikeys.*` or `org.delete`: a key that can mint keys makes
  revoking a leaked one meaningless.

### Abuse limits

- Per-address rate limits on the credential endpoints and on `/api/v1`, answering
  `429` with `Retry-After`. Added alongside the per-account lockout rather than
  instead of it — one address guessing across a leaked list never trips a
  per-account counter.
- 404 probe limiting on the redirect path, charging misses only. A working link is
  never throttled by anyone's scanning, paths that could not be an alias are
  refused on shape without a lookup, and a throttled address still resolves links
  already in the cache.
- Destination validation by allowlist: `http` and `https` only, with private,
  loopback, link-local, carrier-NAT and cloud-metadata addresses refused.

### Interfaces

- Server-rendered dashboard with htmx. Works without JavaScript; no build step at
  runtime and no Node in the image.
- REST API with RFC 9457 problem responses, a hand-maintained OpenAPI 3 document,
  and Swagger UI at `/docs`. A contract test replays every documented operation
  against a live server, so the document cannot drift from the implementation.
- `lctl` for configuration checks, migrations, partitions, API keys and load-test
  seeding — including minting the first key on a headless host.
- `lctl demo` fills an instance with a workspace worth looking at: around twenty
  links with titles, tags and destinations, a month of click history with weekday
  seasonality and a launch spike, and every status the dashboard can render. Its
  links are created through the same service call the REST API uses, so the
  dataset cannot describe a state the product could not reach.

### Operations

- `/healthz` and `/readyz`, the latter distinguishing degradation from failure:
  Redis down is `degraded`, because the service still works.
- Prometheus metrics on a separate, unpublished listener.
- Structured JSON logs. Secrets are a type that refuses to print itself, so a
  config dump or a formatted panic cannot leak the database password.
- Graceful shutdown that fails readiness first, drains, then flushes buffered
  clicks.
- Migrations run in-process at boot, serialized across replicas by a Postgres
  session lock, and disableable for change-controlled deployments.

### Known limitations

Stated because they are the things worth knowing before deploying, and they are
all in [Plan.md](Plan.md#known-limitations) with their consequences:

- **Run one application instance.** Cache invalidation reaches the replica that
  served the edit; others wait out the TTL. Phase 2 adds pub/sub.
- **Rate limits are per instance** and reset on restart.
- **The analytics dimension rollup is expensive** and gets worse with traffic: 16-21
  seconds per 60-second run at 5.7M events. Redirects are unaffected; dashboards go
  stale if it falls behind.
- **Behind a reverse proxy, `LINKCTRL_TRUSTED_PROXIES` must be set**, or every
  request appears to come from the proxy and all traffic shares one rate-limit
  bucket.
- **`LINKCTRL_API_KEY_PEPPER` cannot be rotated in place.** Changing it invalidates
  every existing key.
- No audit log behaviour, folders API, per-workspace custom domains, QR codes, or
  password/one-time links. The tables exist; the features are Phase 2. The
  `visitors` table and `click_events.is_first_visit` are dormant in the same way:
  nothing writes or reads them, and both stay under partition maintenance and
  retention so the guarantees apply the day something does.
- No signup page. `LINKCTRL_SIGNUP_MODE=open` is honoured by the JSON API only,
  and a registration creates a new isolated workspace rather than adding a member
  to yours. Invitations, and a signup form worth having, are Phase 2.

[Unreleased]: https://github.com/DevOfPie/LinkCtrl/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/DevOfPie/LinkCtrl/releases/tag/v0.3.0
[0.2.0]: https://github.com/DevOfPie/LinkCtrl/releases/tag/v0.2.0
[0.1.0]: https://github.com/DevOfPie/LinkCtrl/releases/tag/v0.1.0
