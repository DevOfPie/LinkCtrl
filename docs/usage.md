# Using LinkCtrl

The dashboard and the API are two skins over the same service layer, so anything
one can do the other can too. This walks through both.

Live API reference: **`/docs`** on your instance (Swagger UI, with a working
try-it-out console). The document itself is at `/api/v1/openapi.json` and
`/api/v1/openapi.yaml`.

## The dashboard

| Page | What it does |
| --- | --- |
| `/dashboard` | 30-day totals, clicks-per-day chart, your five newest links. |
| `/links` | Create a link; search, filter by status, sort; page through with a cursor. The search box filters as you type and updates the address bar, so a reload or a shared URL shows the same view. |
| `/links/{id}` | Everything about one link: edit destination, alias, title, description, expiry and tags; per-window analytics (7/30/90 days) with device, browser, OS, referrer, language and country breakdowns, each with a share ring, plus a world choropleth over the country figures; recent activity; archive, restore and delete. |
| `/keys` | Mint, list and revoke API keys, and choose whether a new one reaches one workspace or the organization. Rotation is not here: it replaces the credential that made the request, and a browser session is not one. |
| `/notifications` | Things the instance wanted you to know about, and mark-read. |
| `/disputes` | The review queue: destinations somebody was refused and has asked you to look at. Needs `destinations.review`, which is held instance-wide rather than by a role. The account that claimed the instance also appoints other reviewers here. |
| `/feeds` | Whether this instance sends the destinations you type to a third party, and to whom. Read-only, and readable by everybody — what it describes is what happens to your own data. |
| `/account` | Your profile, password and appearance. |

It works without JavaScript. htmx makes search and filtering swap a fragment
instead of reloading, and that is the only thing it is used for.

### The header

Three destinations at the top level — Dashboard, Links, API keys — and on the
right, in order: the workspace switcher, a notification bell and your email
address.

The **bell** carries the unread count and, opened, shows the newest few unread
notifications with a **View all** link to `/notifications`, which is still the
full surface: everything, paged, with mark-read.

**Since 0.2.0 the inbox is scoped to the workspace you are acting in.** A
notification a workspace produced — an automation rule firing, a custom domain
failing verification — is there while you are in that workspace and not while
you are in another; switch workspace to see its news. Anything that belongs to
the organization rather than to one workspace, a dispute decision or an
audit-growth warning, follows you everywhere. The count, the preview and the
page all agree, because they ask the same question. There is no combined view.

The preview is deliberately
short, and nothing is only reachable through it.

Your **email address** opens a menu holding the administrative surfaces —
**Members**, **Invitations**, **Workspaces** and **Blocked destinations**, each
shown only if you hold the permission its page needs — plus **Reputation
feeds**, **Account** and **Sign out**. They live here
rather than at the top level because each is visited when something *changes*,
where the three top-level destinations are where work happens.

**Reputation feeds** is the one entry gated on no permission at all, and
deliberately so: it says whether the destinations *you* type leave this instance
— by the operator's reputation feed, or by a webhook an administrator of your own
workspace registered — so an editor is exactly the reader it is for.

Both are popovers, which the browser opens and closes on its own: they work with
a keyboard, close on **Escape** or a click anywhere outside, and open only one at
a time. No JavaScript is involved in any of that. They are not ARIA menus — a
screen reader announces a button and a group rather than the `role="menu"`
pattern, and that is the trade for needing no script.

That sets a floor on the browser: **Chrome 114, Safari 17 or Firefox 125**, all
from mid-2023. Something older ignores the panels' popover behaviour and draws
them as plain blocks in the header; it looks wrong, and everything in them is
still reachable.

On a narrow screen the address itself is hidden and the menu is reached by its
icon. The rest of the bar does not reflow — a responsive nav has not been built.

### Light and dark

Every page follows your operating system's setting unless you say otherwise.
The **Appearance** control on `/account` overrides it with System, Light or
Dark. The sign-in page carries the same control, because the choice is stored
per browser and has to be settable before you have signed in.

The choice is stored per browser, in a cookie, not on your account. Two browsers
signed into the same account may disagree, which is deliberate: the person
sitting at each of them chose. It is also why it works before you sign in.

There is no flash of the wrong theme while a page loads. The server reads the
cookie and renders the theme into the page it sends, so the first paint is
already correct — there is no script to run and nothing to correct.

### Creating a link

Paste a destination; leave the alias blank for a generated one. Aliases are
lowercase-canonical, so `/GitHub` and `/github` are the same link, and dots are
refused outright — which removes the whole "is `logo.png` an alias or an asset?"
class of confusion.

Destinations must be `http` or `https`, and may not point at private, loopback,
link-local, carrier-NAT or cloud-metadata addresses. The last one matters: a
short link to `169.254.169.254` turns a shortener into a way to make someone
else's browser probe their own network.

### Editing, archiving, deleting

- **Editing the destination** is the point of the product: the short URL does not
  change. Redirects are never permanent — 302 by default — precisely so an edit
  takes effect.
- **Changing the alias** breaks anything already pointing at the old one, and
  frees it for reuse. The form says so.
- **Archiving** stops redirecting but keeps the alias reserved and the analytics
  readable.
- **Deleting** is soft, with a 30-day window. It stops redirecting immediately,
  and the alias stays reserved for the whole window — nobody can register it
  while the link is restorable. After the purge, an alias that ever received
  traffic is never reissued — it exists on printed material and in other
  people's bookmarks, and handing it to someone else would redirect their
  audience. An alias that never received a click is released.

There is no trash view in Phase 1: recovery inside the 30 days is a database
operation, not a button.

### Folders

A folder is a place to put a link and nothing else. It carries no settings, gives
nobody access to anything, and changes nothing about where a link sends somebody.

**Folders** at the top of the links list opens the tree. From there you can
create a folder — at the top level or inside another, up to eight levels deep —
rename one, move one, or delete one. Two folders in the same place cannot share a
name, and the comparison ignores case, so `Campaigns` and `campaigns` are one
name.

**Moving is two clicks, not a drag.** Press **Move** on a folder and the page
asks where it goes; every destination that would actually be accepted grows a
**Move here** button, and **Move to the top level** sits in the banner. Cancel
leaves it alone. A folder can never be moved into itself or into any folder
inside it, and a branch cannot be moved somewhere that would push part of it past
eight levels — those destinations simply offer no button. The whole thing works
with JavaScript switched off and is operable from a keyboard; there is no
drag-and-drop.

**Deleting a folder never deletes a link.** It removes the folder and the folders
inside it, and every link filed anywhere in that branch stays exactly where it
was — it simply stops being in a folder, and the **No folder** filter on the
links list finds it. There is no undo for the folder itself.

To file a link, use the **Folder** select on the link's own page; the first
option, *No folder*, is how a link comes back out again. Over the API it is
`folder_id` on create and update, where an empty string unfiles:

```sh
curl -sS -X PATCH "$BASE/api/v1/links/$LINK_ID" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"folder_id": "'"$FOLDER_ID"'"}'
```

The links list filters by folder with `?folder=<id>`, and `?folder=none` returns
everything in no folder. **It is one folder, not its subtree**: a parent does not
gather up its children's links, so the number returned is the same number shown
beside that folder on the tree page.

Folders need no permission of their own. Seeing the tree and filtering by it is
`links.read`; creating is `links.create`, renaming and moving is `links.update`,
deleting is `links.delete`. A viewer can therefore read the tree, and an editor
can organise it.

### Campaigns

A campaign is a label saying what body of work a link belongs to. It is not a
folder and it does not replace one: a folder is *where a link lives*, a campaign
is *what it is for*, so a launch link filed under Product can belong to Summer
2026 at the same time.

**Campaigns** at the top of the links list opens the page. Create one with a name;
the slug is derived from it unless you type one, and it is folded to lowercase
letters, digits and hyphens because it is what a filter URL names. Two campaigns
in one workspace cannot share a slug, ignoring case.

**The dates describe the campaign and enforce nothing.** A link in a campaign
that ended last month redirects exactly as it did before it ended. Expiry is a
property of the link, and putting a second, weaker one on the campaign would give
two answers to "why did this stop working" — and would put a second table on the
redirect path to find out. The page shows the schedule and says so.

**Deleting a campaign keeps every link it held.** They stop carrying it; none is
deleted, archived or moved. The **No campaign** filter finds them afterwards.

To label a link, use the **Campaign** select on the link's own page; the first
option, *No campaign*, is how a link comes back out. Over the API it is
`campaign_id` on create and update, where an empty string removes the label —
the same idiom `folder_id` uses:

```sh
curl -sS -X PATCH "$BASE/api/v1/links/$LINK_ID" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"campaign_id": "'"$CAMPAIGN_ID"'"}'
```

The links list filters with `?campaign=<id>`, and `?campaign=none` returns
everything carrying no campaign.

**There is no campaign analytics.** No per-campaign click total, chart or export
exists, and none is planned for this phase: computing one means a new pass over
every click event, and the rollup job that would carry it has only just been
rewritten to fit inside its own interval. The filtered links list is what answers
"how is this campaign doing", one link at a time.

Campaigns need no permission of their own, exactly as folders do not. Reading the
list and filtering by it is `links.read`; creating is `links.create`, editing is
`links.update`, deleting is `links.delete`.

### QR codes

Every link has one. There is nothing to create and no row to make: open a link's
page and the code is drawn under **QR code**, with a **Download the SVG** link
beside it.

**SVG only.** The code is vector text, so it prints at any size, and no image
encoder is anywhere in this program. There is no PNG download, and there is no
way to have two codes for one link.

**The code encodes your short URL with `?src=qr` on it**, and that is what makes
a scan countable. A camera sends no `Referer` header, so without the parameter
every scan would arrive indistinguishable from somebody typing the URL by hand.
Scans show up in the link's **Referrers** breakdown as `qr`, beside `direct` —
counted, deduplicated by visitor and filtered for bots like every other click.
Two things follow: somebody who types `?src=qr` by hand is counted as a scan, and
two printed codes for one link cannot be told apart.

**Restyling changes the drawing and never the content.** The form takes a
foreground colour, a background colour, an error-correction level (`L`, `M`, `Q`,
`H` — higher survives more damage and packs the code tighter at the same printed
size), a quiet zone in modules, and a module size in pixels. **Back to black on
white** appears once a style is stored.

**The code does not follow your theme.** It paints its own background across its
quiet zone and defaults to black on white in both light and dark mode, because a
QR code inverted onto a dark field is refused by a large share of scanners and a
transparent one inverts itself the moment somebody switches theme. The frame
around the code is what the theme colours. The form will accept a low-contrast
pair if that is what your brand wants; it refuses the two that are certainly
broken — the same colour twice, and anything that is not a `#rgb` or `#rrggbb`
colour.

Over the API:

```sh
# The picture.
curl -sS "$BASE/api/v1/links/$LINK_ID/qr.svg" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" -o code.svg

# What it encodes, and how it is drawn.
curl -sS "$BASE/api/v1/links/$LINK_ID/qr" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY"

# Restyle it. An omitted field is its default, so {} is plain black on white.
curl -sS -X PUT "$BASE/api/v1/links/$LINK_ID/qr" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"style": {"foreground": "#123a6b", "level": "Q"}}'
```

Seeing a code is `links.read` and styling one is `links.update`: a QR code is a
picture of the link's own short URL, so anybody who can see the link can see its
code.

### Reading the analytics

Numbers come from daily rollups, not from raw events, which is what keeps them
fast once the click table is large. Two honest caveats travel with them:

- **Unique visitors are estimates at daily resolution.** Carrier-grade NAT
  collapses many people behind one address into one visitor; someone moving from
  WiFi to cellular counts as two.
- **A multi-day total is a sum of daily figures.** Someone visiting on three days
  counts three times. The exact figure is unrecoverable by design — the salts
  that would allow it are deleted after two days.

Every API response carrying these numbers includes that caveat as a `caveat`
field. Bots are recorded and counted separately, and excluded from the headline
figures.

**The Referrers breakdown holds two values that are not hostnames.** `direct` is
a click that sent no `Referer` header — a typed URL, a link in a native app, a
browser configured not to send one. `qr` is a scan of this link's QR code, which
carries `?src=qr` in the picture because a camera sends no referrer either. Both
sit beside the real hostnames because they answer the same question. `?src=`
accepts no other value: anything else is ignored and the click is attributed as
it would have been without the parameter.

Whether a QR code is scanned or its URL is typed cannot be distinguished — the
label travels in the URL, and anybody can type it.

Country breakdowns need a GeoIP database, which cannot be shipped in the image —
see [deployment.md](deployment.md#optional-geographic-analytics). Without one the page says the data is
unavailable rather than drawing a blank chart, and the map is not drawn at all: a
world coloured entirely "unknown" would be a picture of nothing that looks like a
picture of something. Region and city are not stored even with a database
configured: nothing shows them, and city plus a timestamp is close to a location
history.

The map shades each country by its share of the link's clicks across five bands,
and every shape carries its exact figure — hover it, or use the *Exact numbers*
link to the ranked list below, which is the view that answers "how many". A
toggle switches the shading to unique visitors; when it does, the page repeats
the estimate caveat above word for word, because shading a map by an estimate
without saying so would turn it into a fact. Two things the map cannot show are
said in words instead: a country outside the breakdown's top twenty values is not
drawn, and a territory the world map has no outline for — Hong Kong, Monaco,
small islands — is listed under "counted but not drawn" so the map and the list
cannot quietly disagree about a total.

The map is not free, and the number is worth knowing before you put a dashboard
behind a slow link: it is about **86 KB of inline SVG**, on every view of a link
that has geography, and nothing in front of it compresses responses by default.
A link page with a map measures 175 KB against 42 KB without one. That is the
price of a chart with no JavaScript, no CDN and no request of its own; put a
compressing reverse proxy in front of the instance and most of it goes away,
because path data compresses extremely well.

**Breakdowns are recomputed every fifteen minutes**, where the click count and
the daily series are recomputed every minute. So a breakdown — and the map drawn
from it — can be up to a quarter of an hour behind the totals at the top of the
same page. That is the deliberate trade for a rollup whose cost grows with the
number of distinct values your traffic produces; `docs/operations.md` has the
metric and the alert.

## The API

Base path `/api/v1`. JSON in, JSON out, RFC 9457 problem documents for errors.

### Authenticating

Two credentials work everywhere. If a request carries both, the bearer key wins.

**API key** — for scripts, CI and integrations:

```sh
curl -sS https://links.example.com/api/v1/links \
  -H "Authorization: Bearer lk_live_a1b2c3d4_…"
```

**Session cookie** — what the dashboard uses. Sign in, keep the cookie:

```sh
curl -sS -c jar.txt -X POST https://links.example.com/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"a-long-passphrase"}'

curl -sS -b jar.txt https://links.example.com/api/v1/me
```

Cookie-authenticated unsafe requests must be same-origin — cross-site `POST`s
are refused. Bearer-authenticated requests are unaffected, since a browser
cannot be tricked into attaching a header it does not have.

Two operations deliberately refuse an API key and require a session: **minting or
revoking keys**, and **changing a password**. A key that can mint keys makes
revocation meaningless, and a leaked key must not be able to lock its owner out.

There is exactly one thing a key may do to a key, and it is
[rotating itself](#rotating-a-key).

### Getting a key

Mint one in the dashboard at `/keys`, or on a headless host with
[`lctl`](cli.md#apikey). The token appears exactly once — only its HMAC is
stored, so it cannot be recovered afterwards. Lose it, revoke it, mint another.

**Reach.** A key is bound to the workspace you created it in and acts only there.
The alternative — not pinned to one, rather than acting in all of them at once —
is the *Reach* control on the form, `"org_wide": true` on the API, or
`--org-wide` on the CLI. It
needs `apikeys.write` held through an **organization-wide** membership: a role you
hold in one workspace issues keys for that workspace, which is the same rule that
stops a workspace-scoped admin re-roling an organization-wide member. The key
list, `lctl apikey list` and the API all say which of the two a key is.

An organization-wide key follows you into that organization's workspaces and no
further. If you belong to more than one organization, the key stays in the one it
was issued in whatever workspace you last used or pinned elsewhere — the pin is
about you, and the key is about a tenancy.

Scopes are permission slugs you already hold, checked again on every request
against your current role. Demote the owner and their keys weaken immediately.

```
links.read   links.create   links.update   links.delete
tags.read    tags.write     analytics.read domains.write
members.read members.write  workspace.read workspace.write
orgs.create
```

`apikeys.read`, `apikeys.write`, `org.delete`, `audit.read`,
`audit.read.instance`, `destinations.decide` and `instance.admin` are never
grantable to a key — a key that can mint keys makes revoking a leaked one
meaningless, an irreversible action should need an interactive sign-in, the audit
log ties a network prefix to a named person, a key that could allow a blocked
destination could then point links at it, and a key that could appoint a reviewer
would widen its reach by manufacturing somebody else's.

`destinations.review` **is** grantable, and the pair is the point: reading the
dispute queue discloses who filed a dispute and a defanged host, escalating
nothing, so an integration can watch the queue while acting on one stays with a
person. That is enforced by which scopes a key may hold, not by any check on what
kind of credential is calling.

Everything else is grantable, and each one has a reason it is safe to be.
`members.write` gates invitations and member management, and an invitation
issued with a key may carry **`editor` or `viewer` and nothing above**, whatever
rank created the key. Redeeming one produces an interactive account, not another
key — nothing revokes it along with the key — so what a key may hand out is
bounded separately from what it may hold.
`orgs.create` gates organization creation, and a key holding it gains nothing —
scopes are intersected with the owner's role on every request, so an
organization made through a key leaves that key holding exactly what it was
minted with.

### Rotating a key

A key replaces itself. Nobody has to be signed in, which is the point: a
deployment can retire an old credential at 3am without a human.

```sh
curl -sS -X POST https://links.example.com/api/v1/api-keys/rotate \
  -H "Authorization: Bearer $LINKCTRL_KEY"
```

`201` with the successor and the only copy of its token, plus a `predecessor`
block saying when the key you just used stops working.

There is no id in that URL, and that is deliberate: the key being rotated is the
one in the `Authorization` header, and there is no other it could reach. A
signed-in session gets `403` — a session that wants another key mints one.

The successor is **identical or narrower**. Same workspace, same reach, same name;
scopes are this key's unless you name a subset, and a scope this key does not hold
is refused rather than dropped. Its expiry is the predecessor's *lifetime* from
now, so a 30-day key rotates into another 30-day key and a key that never expires
rotates into one that never expires.

Both secrets verify for a grace window — an hour by default, five minutes to a day
via `grace_seconds` — so a rolling deployment can hold either. When it closes the
old key is refused immediately, on every replica; a background sweep then marks it
revoked so the key list agrees with what already happened.

```sh
# Narrower, and cut the old one off in ten minutes.
curl -sS -X POST https://links.example.com/api/v1/api-keys/rotate \
  -H "Authorization: Bearer $LINKCTRL_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"scopes":["links.read"],"grace_seconds":600}'
```

**A key rotates once.** Asking again answers `409`: the successor already exists,
and if its token was lost the way out is to revoke this key from a session and
mint a new one. Every rotation is an audit event naming the prefix it came from
and the prefix it became.

Two things to know before you rely on it:

- **`last_used_at` is written on a 30-second cadence.** Checking whether anything
  still uses the predecessor is the obvious way to confirm a rotation has landed,
  and the answer is up to half a minute stale. That is why the grace window has a
  five-minute floor.
- **A leaked key can rotate itself too.** Whoever holds a stolen secret can
  perform the call above, and they end up with a successor you never issued.
  Revoking the key you know about does not touch it. If you suspect a leak, read
  the key list first and look for prefixes you do not recognise —
  [SECURITY.md](SECURITY.md) says what to do about it.

### Worked examples

Create a link:

```sh
curl -sS -X POST https://links.example.com/api/v1/links \
  -H "Authorization: Bearer $LINKCTRL_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
        "url": "https://example.com/launch",
        "alias": "launch",
        "title": "Launch announcement",
        "tags": ["marketing"],
        "expires_at": "2026-12-31T23:59:59Z"
      }'
```

`201` with the link, and a `Location` header holding the public short URL.

List with search and a total:

```sh
curl -sS -G https://links.example.com/api/v1/links \
  -H "Authorization: Bearer $LINKCTRL_KEY" \
  --data-urlencode 'search=launch' \
  --data-urlencode 'sort=clicks' \
  --data-urlencode 'include_total=true'
```

Paging is by cursor, not offset — offsets re-scan skipped rows and silently
duplicate or drop entries when links are created mid-page. Follow
`next_cursor` while `has_more` is true:

```sh
curl -sS -G https://links.example.com/api/v1/links \
  -H "Authorization: Bearer $LINKCTRL_KEY" \
  --data-urlencode 'cursor=MjAyNi0wNy0zMFQwOTo…'
```

`include_total` is opt-in because counting costs a scan the common page load
should not pay for.

Edit without changing the URL:

```sh
curl -sS -X PATCH https://links.example.com/api/v1/links/$ID \
  -H "Authorization: Bearer $LINKCTRL_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"url": "https://example.com/launch-v2"}'
```

`PATCH` is partial: absent fields are untouched. `expires_at` is three-valued —
absent leaves the expiry alone, `""` clears it, a timestamp sets it.

Analytics:

```sh
# One link, explicit window
curl -sS -G https://links.example.com/api/v1/links/$ID/stats \
  -H "Authorization: Bearer $LINKCTRL_KEY" \
  --data-urlencode 'from=2026-07-01' --data-urlencode 'to=2026-07-30'

# Whole workspace
curl -sS https://links.example.com/api/v1/stats/overview \
  -H "Authorization: Bearer $LINKCTRL_KEY"

# Recent raw clicks, bounded
curl -sS https://links.example.com/api/v1/links/$ID/clicks \
  -H "Authorization: Bearer $LINKCTRL_KEY"
```

Windows default to the last 30 days and may not exceed 400. Dates are `YYYY-MM-DD`
and UTC.

Archive, restore, delete:

```sh
curl -sS -X POST   .../api/v1/links/$ID/archive -H "Authorization: Bearer $KEY"
curl -sS -X POST   .../api/v1/links/$ID/restore -H "Authorization: Bearer $KEY"
curl -sS -X DELETE .../api/v1/links/$ID         -H "Authorization: Bearer $KEY"
```

### Errors

Every failure is `application/problem+json`. Branch on `type`, never on prose:

```json
{
  "type": "https://linkctrl.dev/problems/validation-failed",
  "title": "Validation failed",
  "status": 422,
  "detail": "One or more fields are invalid.",
  "instance": "/api/v1/links",
  "errors": [
    { "field": "url", "code": "unappealable.private_address",
      "message": "destination must not be a private, loopback or link-local address" }
  ]
}
```

| Status | Means |
| --- | --- |
| `401` | No valid credential — or the credential itself is being rejected. Invalid, revoked and expired keys are indistinguishable on purpose, and so is every sign-in failure: a wrong password, an unregistered address, a suspended account and an account locked out by repeated failures are one answer, because telling them apart says whether an address has an account here. |
| `403` | Authenticated but not permitted. The detail names the missing permission, which is useful rather than a disclosure. |
| `404` | Does not exist, or belongs to a workspace you cannot see. Someone else's resource is never a `403`, so ids cannot be probed. |
| `409` | Alias already taken. |
| `422` | Validation failed; `errors` names each field. |
| `429` | `rate-limited`: your address is going too fast. `Retry-After` says how long to wait, and waiting works. This used to be two types — the other was `account-locked` — and it is one now, because which of them you got answered whether the address you named is registered. A locked account is a `401` like every other sign-in refusal; the lockout is fifteen minutes and further attempts extend it, so the `401` body says so whether or not one is in force. |
| `504` | The request exceeded the server's deadline. Retry; narrow the window if it is an analytics query. |

Unknown JSON fields are rejected rather than ignored: a misspelled field silently
dropped means you believe you set something you did not.

#### Refused destinations

A destination refused by the blocking tiers carries a `code` of
`<tier>.<rule>`, so one string says both how sure the refusal was and what it
matched. **This changed in 0.2.0**: the codes were previously bare rules
(`private_address`, `host_blocked`), and a client branching on them needs
updating.

| Code | Means | Can it be appealed |
| --- | --- | --- |
| `unappealable.scheme_not_allowed` | Not `http` or `https` | No |
| `unappealable.private_address` | A private, loopback, link-local, carrier-NAT or cloud-metadata address, in any spelling a browser resolves | No |
| `high_confidence.embedded_host` | An exact host on the list compiled into this build | Only by rebuilding the instance |
| `low_confidence.operator_blocklist` | A host the operator listed, or a child of one | Yes — and allowing it removes the entry |
| `low_confidence.shortener_chain` | The destination is itself a short link, on a host the instance ships as a known shortener, or a child of one | Yes — and allowing it removes the entry |
| `low_confidence.punycode_homograph` | The host is spelled to imitate a different name | It can be reviewed and upheld, but not allowed |
| `low_confidence.url_credentials` | Credentials before the host, which hide where the URL goes | Same |
| `low_confidence.feed_reputation` | A third-party reputation feed the operator configured says the destination is malicious. Only appears on an instance that has one — see `/feeds` | Yes — and allowing it also stops that host being sent to the feed again |

The codes that are not tiered — `required`, `too_long`, `invalid`, `no_scheme`,
`no_host` — are unchanged. Those are malformed input rather than a refusal, and
they are not recorded as blocked attempts.

A host is compared against every tier in the form a browser would resolve it to,
so an internationalized destination is **converted** and not refused:
`https://müller.de/preise` is accepted and stored as
`https://xn--mller-kva.de/preise`, which is the value the response returns and
the value a visitor is sent to. A host that is not a usable name in any
spelling — a right-to-left override in it, a broken `xn--` label — comes back as
`invalid`, in the untiered group above.

#### Appealing one

Only the `low_confidence.*` tier can be appealed. `POST /api/v1/disputes` with
the URL that was refused files one; there is no field naming the refusal,
because the server re-derives the tier from the URL and will not take your word
for it.

```sh
curl -sS -X POST "$BASE/api/v1/disputes" -b cookies.txt \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example-shortener.test/abc"}'
```

`422` with a code of `not_disputable` means the refusal was unappealable or came
from the compiled list, and no review changes either. `not_blocked` means nothing
refuses that destination any more. `409` means this decision is already queued —
either about the same host, or about the same **blocklist entry**, so once
somebody has appealed the row that says `evil.example`, `login.evil.example` is
the same appeal rather than a second one.

Whoever holds `destinations.review` reads the queue with
`GET /api/v1/disputes?open=true`, and whoever holds `destinations.decide` answers
with `POST /api/v1/disputes/{id}/allow` or `.../uphold`. Allowing deletes the
blocklist entry; upholding changes nothing. Both are audit events — recorded
against no organization, because what they change belongs to none — and both
notify the person who asked.

**Neither permission comes from an organization role.** The account that claimed
the instance holds both, plus `instance.admin`, and appoints other reviewers at
`/disputes` or with `POST /api/v1/instance/reviewers`. Somebody it appoints
reviews disputes and cannot appoint anybody else. An API key may hold the reading
half and never the deciding half, so a token can watch the queue and a person has
to answer it.

**The entry allowing deletes is `blocked_host_defanged`, not `host_defanged`.**
The two differ whenever somebody was refused by a subdomain of a listed host: a
refusal at `login.evil.example` comes from the row that says `evil.example`, and
that row is what refuses every workspace on the instance. It is recorded when the
dispute is filed, so an entry added while the dispute waits cannot change what
allowing it removes. A refusal with no row behind it carries no
`blocked_host_defanged` at all.

Three allows are refused with `409` rather than doing nothing: a
`punycode_homograph` or `url_credentials` refusal holds no entry to delete
(`liftable` is `false` on those); an entry that came from
`LINKCTRL_DESTINATION_BLOCKLIST` would come back at the next restart — take it
out of the environment instead; and a dispute filed before 0.2.0 carries no
recorded entry, so uphold it and file it again.

A `feed_reputation` refusal is `liftable` and deletes nothing, which is the one
allow that works that way. There is no blocklist row behind it — the verdict is
re-asked on every write — so the decision itself is the override: after it, that
host is neither refused nor sent to the feed again. It is scoped to the exact
host, so allowing `evil.example` says nothing about `login.evil.example`.

#### What happens to your destinations

`GET /api/v1/feeds` answers for **both** channels a destination can leave by. It
is readable with any credential, because what it describes is what happens to the
caller's own data, and it is the same disclosure the dashboard shows at `/feeds`.

```sh
curl -sS "$BASE/api/v1/feeds" -H "Authorization: Bearer $KEY"
```

```json
{
  "enabled": false,
  "webhooks": { "receiving": false, "count": 0 }
}
```

That is the answer on a fresh instance, and both halves are needed to read it.
`enabled` is the reputation feed: the **operator's**, instance-wide, unset in the
shipped default. `webhooks` is **your workspace's**: `receiving` is true when at
least one enabled registration there is subscribed to an event whose payload
carries a destination — the five link-lifecycle events, which carry the URL as
typed, or `destination.blocked`, which carries the refused attempt defanged. An
`automation.fired` subscription carries neither and does not count, nor does a
registration that is switched off.

`{"enabled": false}` alone was once treated as the whole answer, and it is not
one: no operator setting turns webhooks off, so a workspace with one registered
is sending destinations somewhere on an instance whose feed is unset. Read
neither field as a statement about the other — `enabled` says nothing about your
workspace, and `webhooks` says nothing about anybody else's.

The answer carries a count and never an address. Reading *who* a workspace posts
to needs `webhooks.read` (`GET /api/v1/webhooks`); being told that your
destinations leave needs nothing, so a key holding no webhook scope still gets a
true answer here.

There is no write counterpart. A feed is switched on in the instance's
configuration and nowhere else; a webhook is registered through
`/api/v1/webhooks`, which is a different operation behind a different permission.

**Every destination this API returns is defanged**, in `host_defanged` and
`destination_defanged`, and there is no way to obtain the original through it.
Nothing on the server fetches a disputed URL — no preview, no screenshot, no
liveness check — because a shortener that fetches a URL somebody submitted is the
SSRF the address refusals exist to prevent.

### Redirect behaviour

| Situation | Response |
| --- | --- |
| Active link | `302` with `Location`, `Cache-Control: private, no-store` |
| Expired | `410 Gone` — distinct from 404 so crawlers and link checkers stop retrying |
| Unknown, archived or disabled | `404` |
| Password-protected, not yet answered | `200` with a small challenge page, `no-store` and `noindex`. Submitting the right password to the same URL answers the redirect directly, as a `303` — the one status that mandates a `GET`, so the password body stops at this server whatever `LINKCTRL_REDIRECT_DEFAULT_STATUS` is set to. Not `401`: there is no authentication scheme being offered, and a `401` without one is a status no client can act on |
| Password-protected, wrong password | `200` with the same page and an error on it. Guesses are rate-limited per address **and** per link |
| One-time or click-limited, budget spent | `410 Gone`, for the same reason an expired link is |
| Signature required, and absent, wrong or expired | `403` with a fixed page. One answer for all four causes, so it cannot be asked which one applies |
| `POST` to an alias that is not password-protected | `405` with `Allow: GET, HEAD`. The short-link host accepts exactly one write — a password submission — and nothing else |
| Anything after the alias, on a link that does not forward paths | `404` — the same answer, so it cannot be used to find out which aliases exist |
| Too many misses from one address | `429` with `Retry-After` — see [configuration.md](configuration.md#rate-limits). Links already in the cache keep resolving, and paths that could never be an alias are not counted. |
| A sequential split test whose rotation could not be advanced | `503`. The order is strict by decision, so an arm is never chosen at random when the counter is unavailable — "approximately sequential" would be worse than briefly unavailable |
| The server could not resolve it | `503` with `Retry-After: 1`. Deliberately not a `404`: that would claim the link does not exist, and a crawler or link checker believing it drops a live link. |

Query forwarding is per-link and off by default: set `forward_query` (a checkbox
on the link's edit form, a boolean in the API) and the visitor's query string is
merged into the destination, with the destination's own parameters winning on
conflict.

**`src` is a reserved parameter**, read by this server on every redirect. It is
what a QR code carries — `?src=qr` — so a scan can be told apart from a typed
URL, and it is the only value the parameter accepts; anything else is ignored
entirely. Unlike the signature parameters below, **it is not stripped**: with
query forwarding on it reaches your destination like any other parameter, because
a source label is not a credential and a destination whose own analytics also see
it is better informed rather than compromised.

**Deep-link path forwarding** is the other half, and the same shape:
`forward_path`, per link, off by default. With it on, path segments after the
alias are appended to the destination's own path — `/{alias}/api/quickstart`
reaches `https://docs.example/product/api/quickstart` for a link pointing at
`https://docs.example/product`. Both may be on at once; the path is joined
first, then the query is merged onto the result.

Three things about it are worth knowing before switching it on:

- **The alias stops being one URL.** With forwarding on, that alias answers
  every path beneath itself. That is the feature, and it is also why it is off
  by default and per link rather than per instance.
- **With it off, anything after the alias is a `404`** — the custom page, and it
  spends the same 404-probe allowance an unknown alias does. There is no
  fallback to the bare destination, because that would make every link on the
  instance answer every URL under itself.
- **Nothing about the destination changes except its path.** Its scheme, host,
  query and fragment are untouched, encoded characters are passed through
  rather than re-encoded, and `..` segments are refused rather than resolved —
  a request for `/{alias}/%2e%2e/admin` is a `404`, not a redirect one directory
  up.

`HEAD` works, does not record a click, and **does not spend one either** — so a
link checker cannot use up a one-time link by asking whether it is alive. It is
still refused when there is nothing left to spend: a link whose budget is gone
answers `410` to `HEAD` exactly as it does to `GET`, because *not spending a
click* was never meant to mean *not checking whether there is one*.

### Gated links

Four things can be put in front of a link, each off unless you switch it on, each
on the link's own page and in the API. They restrict **the short link**, not the
destination: anybody who reaches the destination another way is unaffected, and
whoever passes a gate holds the destination URL afterwards.

- **A password.** Twelve characters minimum, hashed with argon2id, never
  readable back — not in the form, not in the API. Because nothing is stored in
  the visitor's browser, they type it on every visit. Removing one is explicit
  (an empty box means *leave it alone*, since nobody can retype what they cannot
  read).
- **One-time**, and **a click limit**, which are the same gate with different
  numbers. Past the limit the link answers `410`. The count is exact and is
  **not** the click total on the link's page, which is approximate. Raising a
  limit on a link that has stopped starts it again.
- **A signed link.** With `require_signature` on, the plain short URL is refused
  and only a signed, unexpired one works:

  ```sh
  curl -X POST -H "Authorization: Bearer $LINKCTRL_KEY" \
       -H 'Content-Type: application/json' -d '{"ttl_seconds": 3600}' \
       https://links.example.com/api/v1/links/$ID/sign
  # {"url":"https://links.example.com/abc123?exp=1785739578&sig=YTK8…",
  #  "expires_at":"2026-08-03T06:46:18Z"}
  ```

  Up to thirty days. The expiry is inside the signature, so editing the URL does
  not extend it, and both parameters are stripped before query forwarding
  reaches the destination. The URL is minted on **the hostname the link is
  published under**, which is the workspace's own verified hostname when it has
  one, and the signature verifies only there: the domain is inside the MAC, so
  the same alias on another hostname is a different link and does not accept it.
  There is no revoke button — signatures expire, and
  invalidating every outstanding one for a workspace means clearing
  `workspaces.signing_secret`, which [SECURITY.md](SECURITY.md) documents.

## Roles

| Role | Rank | Can |
| --- | --- | --- |
| Owner | 10 | Everything, including creating an organization and deleting this one. |
| Admin | 20 | Everything except creating or deleting an organization. |
| Editor | 30 | Create, edit and delete links and tags; read analytics. **Cannot mint API keys** — an editor who could would be able to grant themselves scopes beyond their own role. Cannot see the member list. |
| Viewer | 40 | Read links, tags and analytics. |

Rank counts *downward* in authority, and it is what every ceiling in the product
is expressed against: the role an invitation may carry, the role you may hand
out, and which memberships you may change. Lower binds tighter.

Registration auto-provisions one personal organization and workspace per user,
and the account that claims the instance is its owner. Accepting an invitation
is what puts somebody in a role that is not owner. The evaluator is real, and
changing a role changes behaviour immediately, including for existing API keys.

A membership can also name a single workspace, in which case its role applies
there **in addition to** whatever the person holds organization-wide. Permissions
are the union of every membership that matches the workspace being acted in.

## Inviting somebody

*Invitations*, in the menu behind your address in the header, is where you add
people to your organization. It needs the **`members.write`** permission, which
owner and admin hold.

An invitation is one grant of one membership, and four things are true of it:

- **It is tied to the address you send it to.** Whoever opens the link has to
  enter that address to redeem it, so forwarding it into a group chat does not
  let somebody else join. That is what makes the link safe to copy, which
  matters because on an instance with no mailer the link is the only way to
  deliver one.
- **It carries a role at or below your own.** An owner can invite an owner; an
  admin cannot. The form offers you exactly what you may issue.
- **It works once**, and stops working when you revoke it or when
  `LINKCTRL_INVITE_TTL` runs out — seven days by default, counted from when you
  created it rather than from when the mail left.
- **It is shown once.** Only a hash is stored, exactly as for an API key, so a
  lost link is re-issued by revoking and inviting again.

With a mailer configured the invitation is emailed; either way the link appears
on the page for you to copy. Accepting adds the organization to an account the
person already has, or creates one for them — **unless the instance's signup
mode is `closed`**, where no invitation may create an account and only somebody
who already has one can join. That mode is `LINKCTRL_SIGNUP_MODE` and the
operator sets it; see [Sign-ups](#sign-ups) below.

Somebody who joins by invitation gets a membership and nothing else: no personal
organization or workspace of their own, which is what would make them a separate
tenant rather than a colleague.

The same operations are in the API:

```sh
# Issue one. The response carries the link, and it appears nowhere else.
curl -sS -X POST .../api/v1/invitations \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"email":"colleague@example.com","role":"editor"}'

curl -sS .../api/v1/invitations -H "Authorization: Bearer $KEY"
curl -sS -X DELETE .../api/v1/invitations/$ID -H "Authorization: Bearer $KEY"

# Accepting takes no credential at all — it is how somebody gets their first.
curl -sS -X POST .../api/v1/invitations/redeem -H 'Content-Type: application/json' \
  -d '{"token":"…","email":"colleague@example.com","password":"…"}'
```

Issued with a key, `role` may be `editor` or `viewer` only; `admin` and `owner`
answer `403` however the key was minted. Issued from the dashboard it is your
own rank that bounds it.

Every way redemption can fail answers the same `404` with the same body: a
token that was never issued, one that expired, one already used, the wrong
address, the wrong password, or an address with no account on a closed instance.
That is deliberate. Anything else would let whoever holds a link ask the server
whether a given address has an account here.

Issuing, revoking and accepting are all in the audit log, and an accepted
invitation shows up in the inviter's notification inbox.

## Managing the people already here

*Members*, in the same menu, lists every membership in your organization with
the role it carries and how far it reaches. Reading it needs **`members.read`**
and changing anything needs **`members.write`**; owner and admin hold both.

**You manage only roles below your own.** An admin changes and removes editors
and viewers, and never another admin — nor themselves, so an admin who wants to
step down asks an owner. Owners are the exception: an owner manages every role
including another owner, because an owner already holds everything and there is
no authority left to escalate to. The page draws the controls you can actually
use and leaves the rest as plain text, and the service refuses again either way.

**The last owner cannot be removed or demoted**, by anybody, including
themselves. Make somebody else an owner first. There is no self-service way to
leave an organization; somebody who outranks you does it.

Separately from who you may act on, you cannot hand out a role above your own —
the same ceiling an invitation carries. So an admin may promote an editor to
admin, and will then find they can no longer manage them.

Removing somebody ends the membership and nothing else. Their account, their
password and the links they made all stay; inviting them again restores access.

### Giving somebody a role in one workspace

The same page grants a role scoped to a single workspace. **It adds and never
narrows.** Permissions are the union of every membership that matches a
workspace and the effective role is the strongest among them, so an
organization-wide editor granted `admin` in one workspace is an admin there and
an editor everywhere else.

There is no way round this: *organization admin, viewer in the finance
workspace* is not expressible. If you need somebody to see less, give them less
everywhere and add workspaces back.

**What it grants stops at that workspace.** The role somebody holds there lets
them administer that workspace's memberships and nothing wider: they cannot
re-role or remove an organization-wide membership — their own included — grant
themselves a role in a second workspace, send an invitation, or delete the
organization. Each of those is authorized against a membership covering the
whole organization, so somebody made an `owner` of one workspace owns that
workspace and not the organization. The member list draws only the controls that
will work, and the workspace picker on it offers only the workspaces you may
actually grant in.

Withdrawing the grant is the same *Remove* control — it removes that membership
row and leaves the organization-wide one alone.

```sh
curl -sS .../api/v1/members -H "Authorization: Bearer $KEY"

# Re-role or remove. The id is the *membership* id from the list above.
curl -sS -X PATCH .../api/v1/members/$ID \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"role":"editor"}'
curl -sS -X DELETE .../api/v1/members/$ID -H "Authorization: Bearer $KEY"

# Add a role in one workspace.
curl -sS -X POST .../api/v1/members \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"user_id":"…","workspace_id":"…","role":"admin"}'
```

Everything here is in the audit log: added, removed, re-roled.

## Adding workspaces, and organizations

*Workspaces*, in the same menu, lists the workspaces of your organization and
creates, renames and deletes them. It needs **`workspace.write`**, which owner
and admin hold — and for renaming or deleting, it needs that permission *in the
workspace being changed*, which can differ from the one you are acting in if you
hold a workspace-scoped role.

**Deleting a workspace is refused while it holds any link at all**, archived
ones included. Links, tags and folders all cascade from a workspace and there is
no trash to restore them from, so this refusal is the only guard there is; an
archived link keeps its alias and its click history, so archiving is not a way
round it. Delete the links first. There is no bulk delete and no way to move a
link to another workspace, so that is a link at a time.

An organization's **last** workspace cannot be deleted either. Everybody in an
organization resolves into one of its workspaces to act at all, so removing the
last one would leave every member unable to sign in.

The same page creates an **organization** of your own, if you hold the
**`orgs.create`** permission. That is granted to the owner role and to nothing
else, so on a default instance it is the account that claimed the instance and
nobody else — until an owner deliberately makes somebody else an owner. Creating
one provisions its first workspace and your owner membership in the same
transaction, and moves you into it.

### Deleting an organization

The same page deletes the organization you are in, if you hold **`org.delete`**
— the owner role and nothing else, and no API key, ever. It removes every
workspace in it and everything under them, every membership, every outstanding
invitation and every API key issued in it, in one transaction. **There is no
undo, no trash and no export.**

```sh
curl -sS -X DELETE .../api/v1/organizations/$ORG_ID \
  -H "Authorization: Bearer $KEY"   # 403: org.delete is never grantable to a key
```

The id in the path must be the organization you are acting in. Any other id
answers `404`, the same answer one that never existed gets, so a mistyped id
deletes nothing and no id can be probed. To delete a different organization,
switch into it first.

Two refusals, both `409` with the reason in `detail`:

- **While any workspace still holds a link**, archived ones included. This is
  the workspace rule one level up: without it, deleting the organization would be
  a way around the guard that protects a workspace's links. There is no bulk
  delete, so a large organization is a link at a time and there is no shortcut.
- **While it is the only organization on the instance.** An instance with none
  cannot be used or repaired from the dashboard.

**Members left belonging to nothing are not a refusal.** Somebody whose only
organization this was keeps their account, their password and their sessions.
They can sign in, and they are shown a page offering them an organization of
their own; until they take it, every other page redirects them back to that
offer — including *Account*, so a password cannot be changed from that state.
Creating an organization there is permitted despite the account holding no
permissions at all: it is the only operation with that exemption, and the moment
the account has any membership, `orgs.create` decides again.

**The audit trail survives.** `organization.deleted` is recorded with the name
and slug of what was removed, and every earlier record the organization wrote
stays in the table. It is not readable through the API afterwards — `GET
/api/v1/audit` is scoped to the organization you are in, and nobody can be in a
deleted one — so reading a deleted organization's history takes database access.

Nothing else is kept, and the link refusal is why: an organization that can be
deleted holds no links, so there are no aliases left to protect. Aliases that had
received traffic were already reserved when those links were purged, against the
instance domain, which belongs to no organization and is untouched.

```sh
curl -sS -X POST .../api/v1/workspaces \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"Marketing"}'
curl -sS -X PATCH .../api/v1/workspaces/$ID \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"Growth"}'
curl -sS -X DELETE .../api/v1/workspaces/$ID -H "Authorization: Bearer $KEY"

curl -sS -X POST .../api/v1/organizations \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name":"Acme"}'
```

Both refusals — links present, last workspace — answer `409` with the reason in
`detail`. The API does not move your session into a new organization; call
`POST /api/v1/workspaces/{id}/switch` with the `workspace_id` it returned.

## Sign-ups

Whether anybody may create an account here is **`LINKCTRL_SIGNUP_MODE`**, and
the operator is the only one who sets it. There is no toggle in the dashboard
and no endpoint that changes it: admitting accounts to an instance is not
something one organization's owner decides for everybody, so changing it is an
`.env` edit and a restart (decision D38).

Three modes, in increasing order of who is admitted:

| Mode | Who may get an account |
| --- | --- |
| `closed` | Nobody new, by any path. An invitation may only add somebody who already has an account here. |
| `invite` | Anybody an invitation names. There is no public form. |
| `open` | Anybody, at `/signup`, after confirming their address by email. |

The shipped default is `closed`.

**`open` needs a mailer.** Registration confirms the address before the account
exists, so with no `LINKCTRL_SMTP_HOST` configured the effective mode is
`invite`: `/signup` answers 403 rather than presenting a form that could not be
completed, and the server states the derivation once at boot.

### What a self-registered account gets

Its **own organization and workspace**, with the registrant as owner. It does not
join yours. Somebody who accepts an invitation gets the opposite — membership in
the inviting organization and nothing else — so if the thing you want is a
colleague, what you want is an invitation, not `SIGNUP_MODE=open`.

### What registering actually does

Nothing, until the address is proven. `POST /api/v1/auth/register` and the
`/signup` form both answer with a queued verification mail and a `202`; no user,
organization or workspace exists yet. Following the link and confirming is what
creates all three, and the link works once and lapses after a day. Registering
again replaces an outstanding link, which is what to do when the mail does not
arrive. Lowering the mode invalidates the links already sent: verification asks
the mode again, so a restart into `closed` strands them.

Registration shares the sign-in rate limit, per address, so alternating between
the two surfaces does not double anybody's budget. There is **no CAPTCHA**. On a
public instance, open sign-ups are the largest abuse surface there is.

## Which workspace you are in

Every request acts in exactly one workspace. With one membership — which is
every account that has not accepted an invitation — there is nothing to choose
and the dashboard shows no switcher at all.

Once there is more than one, a control appears in the header. Switching moves
*that browser*, immediately and for the rest of the session, so two windows can
sit in two workspaces. It is also remembered: the next time you sign in, you
start where you last were.

*Account* → **Default workspace** overrides that. **Last-Used** is the first
option and the one every account is on; picking a workspace instead pins it, and
new sessions start there however you were switching about. The pin applies to
sessions started after it, so the browser you set it from stays where it is.

The same three operations are in the API:

```sh
curl -H "Authorization: Bearer $LINKCTRL_KEY" https://links.example.com/api/v1/workspaces
```

`current` marks the one this request is acting in; `default` marks the pin, and
nothing carries it while the account follows last-used. **The list a key gets is
its own organization's**, where the list a browser gets spans every organization
you belong to: crossing them is what the switcher is for, and a key cannot act
outside the one it was issued for, so it is not told what the others are called.
Switching
(`POST /api/v1/workspaces/{id}/switch`) and pinning
(`PUT /api/v1/workspaces/default`, with `null` to go back to last-used) require
a signed-in session and answer `403` for an API key: switching moves the calling
session, which a key has not got, and remembering the choice would repoint where
*you* land next. It is not that a key would be unaffected — an organization-wide
one names no workspace and resolves one per request the way a sign-in does, so a
switch you make in a browser moves its requests too unless you have pinned a
default.

## The link domain

When short links have a hostname of their own — see
[configuration.md](configuration.md#two-hostnames) — that hostname's root is a
page in its own right. It answers `404` until you point it somewhere, which is
what a visitor gets if they trim a short link back to the bare domain.

The *Account* page has the setting when the hosts are separate; it is hidden
otherwise, because on a single host `/` is the dashboard and there is nothing to
repoint. The same setting is at `/api/v1/domain`:

```sh
curl -sS -X PATCH .../api/v1/domain \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"root_redirect_url":"https://example.com/"}'
```

Send `""` to clear it and go back to answering `404`.

Three things worth knowing. **Since 0.2.0 it needs the
`domains.write.instance` permission, which the instance principal holds and
nobody else does unless the operator says so** — this is not one link and it is
not one tenant's hostname either, it is where every stray visitor to the whole
domain ends up. It used to need `domains.write`, which owner and admin hold, so
on an instance with more than one organization every owner could repoint it. The destination is validated
exactly as a link's is, so `http` and `https` only and no private, loopback or
cloud-metadata addresses — a root redirect is the easiest thing on the instance
to reach, needing no link and no alias. And the redirect is a `302` that
intermediaries are told not to cache, so changing it takes effect immediately.

## Using a domain of your own

A workspace can register a hostname, prove it controls it, and serve its short
links there. **Registering is not the same as serving**, and the page says so
before the form: a registered hostname is stored unverified, and a hostname
pointed at this instance gets the same `404` it got before you registered it
until the DNS check passes.

*Domains*, in the header's identity menu, is the page. Registering, renaming and
removing **your workspace's own** hostnames needs the **`domains.write`**
permission — owner and admin hold it, editor does not. The instance default
domain is not administered here at all: it cannot be renamed or removed by
anybody, and its root redirect and bot policy are the instance principal's
(`domains.write.instance`).

```sh
curl -sS -X POST .../api/v1/domains \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"hostname":"go.example.com"}'
```

Type the hostname on its own: no `https://`, no path, no port. It is stored
lowercased with a trailing dot removed, so `Example.COM.` and `example.com` are
one name. A pasted URL, an IP address, a bare `localhost` and a numeric
top-level domain are each refused with a message saying which.

**A hostname belongs to exactly one workspace, across the whole instance.** It
is one alias namespace — every alias on it has to be unambiguous — so it cannot
be shared, and registering a name somebody else has already registered is
refused. The message names the hostname and not its owner.

`GET /api/v1/domains` lists what your workspace may use: the instance's default
domain, and whatever your workspace owns. Another workspace's hostname is not in
it. Each entry carries `manageable`, which says whether *you* may change that
one.

Changing and removing are the other two operations:

```sh
curl -sS -X PATCH .../api/v1/domains/$ID \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"hostname":"links.example.com"}'

curl -sS -X DELETE .../api/v1/domains/$ID -H "Authorization: Bearer $KEY"
```

The hostname is the only thing a registration has, which is why changing one is
the whole of the update. Removing frees the name for anybody on the instance to
register again.

Three refusals worth knowing before you meet them. Another workspace's hostname
answers **403** — you are being told it is not yours, rather than that it does
not exist. The instance's **default domain** is listed but is not administered
here: it is the hostname every workspace's links are on, and its settings are
the operator's, at `/api/v1/domain` above. And a hostname with links on it
cannot be removed, because every one of them would stop resolving.

Registering, renaming and removing are written to the audit log.

### Proving you control it

The row shows one DNS record. Publish it, point the hostname at this instance,
and press **Check DNS**:

```
_linkctrl-challenge.go.example.com.  IN  TXT  "b7f0…"
go.example.com.                      IN  CNAME  <the instance's link host>
```

```sh
curl -sS -X POST .../api/v1/domains/$ID/verify -H "Authorization: Bearer $KEY"
```

A failed check comes back as a **422** naming what it found — no record at all,
or a record whose value is not your token — because that is something you fix in
your DNS provider and try again. There is no waiting period in either direction:
the check runs the moment you ask for it.

**Until it passes, nothing is served on the hostname.** That gap is deliberate
and it is the reason this exists: without it, anybody who pointed a hostname at
this instance could serve short links on it.

Once it passes, this instance re-checks the record **every hour**. If it stops
being there, you are told at once and your links keep working for **24 hours**;
if the record has not come back by then, the hostname stops being served until
you publish it again and verify. (An operator can change both numbers — see
`docs/configuration.md`.) **Renaming a hostname un-verifies it**, because the
record you published proves you control the old name.

TLS is the operator's proxy, not this application: it never obtains a
certificate and never contacts a certificate authority. Where the proxy is Caddy
with on-demand TLS, a verified hostname gets a certificate on its first visit
with nothing further to do.

### Putting links on it

Name the domain when you create a link, or leave it out and get your workspace's
own hostname if it has a verified one:

```sh
curl -sS -X POST .../api/v1/links \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/spring","alias":"spring","domain_id":"'$ID'"}'
```

`short_url` comes back built from that hostname. Two things follow from aliases
being per hostname rather than per instance: the same alias can exist on two of
your hostnames and point at two different places, and an alias that is reserved —
`login`, `api`, `healthz` — is reserved on every hostname, including yours.

**Links already created do not move.** Nothing rewrites a URL somebody has
already published, so verifying a hostname changes where *new* links go and
leaves the existing ones where they are. The links list has a hostname filter for
exactly that reason.

`https://go.example.com/` — the bare hostname — answers `404` until you point it
somewhere:

```sh
curl -sS -X PUT .../api/v1/domains/$ID/root-redirect \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"root_redirect_url":"https://example.com"}'
```

Sending `""` removes it and restores the `404`. It is offered only on a verified
hostname, and the destination goes through the same refusals a link's does.
Changing it is audited.

## Webhooks

Where this server sends a workspace's events. Register a URL, choose the events,
and every one of them arrives as a signed JSON POST.

Needs `webhooks.read` to see and `webhooks.write` to change, which are the owner
and admin roles. `webhooks.write` cannot be held by an API key: a webhook keeps
delivering after the credential that created it is revoked, so creating one takes
a signed-in person. Reading and inspecting deliveries work with a key.

The dashboard page is `/webhooks`; the API is `/api/v1/webhooks`.

```sh
curl -sS -X POST .../api/v1/webhooks \
  -H "Cookie: $SESSION" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/hooks/linkctrl",
       "events":["link.created","link.updated"],
       "description":"Ops channel"}'
```

The response is the **only** place the signing secret appears. It is not stored
anywhere it can be read back; if you lose it, rotate:

```sh
curl -sS -X POST .../api/v1/webhooks/$ID/rotate -H "Cookie: $SESSION"
```

There is no overlap window. The previous secret stops verifying immediately,
because two valid secrets at once means a receiver that has been compromised
keeps verifying for as long as the window lasts.

### The events

Seven, and the vocabulary is closed — an unknown name is refused rather than
ignored, so a subscription that would never fire is not something you can
accidentally create.

| Event | When |
| --- | --- |
| `link.created` | After a link exists. Fetching its short URL on this event finds it working. |
| `link.updated` | Any successful edit, the destination included — **and an edit that changed nothing**, because the service does not diff and the dashboard form posts every field on every save. Make your handler idempotent rather than assuming something moved. |
| `link.archived` | The link was paused. |
| `link.restored` | It was un-paused. |
| `link.deleted` | Somebody deleted it, starting the 30-day recovery window. **Not** the purge at the end of that window — you would otherwise be told twice about one link. |
| `destination.blocked` | Somebody was refused a destination. The payload names the tier, the rule and the surface, and carries the attempted URL **defanged** — `https[:]//evil[.]example` — so nothing that renders it produces a live link to the thing that was refused. |
| `automation.fired` | An [automation rule](#automation-rules) ran. The payload names the rule, its trigger, how many subjects it matched and the first five of them. **This is the only event a workspace can cause this server to emit on purpose, and nothing triggers on it** — which is what stops a rule that emits a webhook being a rule that sets off another rule. |

### The payload

```json
{
  "event": "link.created",
  "occurred_at": "2026-08-03T09:41:22Z",
  "workspace_id": "0198c9c5-0000-7000-8000-000000000010",
  "data": {
    "id": "0198c9c5-0000-7000-8000-000000000001",
    "alias": "spring",
    "short_url": "https://lnk.example.com/spring",
    "url": "https://example.com/spring",
    "title": "Spring campaign",
    "status": "active"
  }
}
```

`data` differs by event; the three fields around it do not. A `destination.blocked`
payload carries `tier`, `rule`, `code`, `surface` and `url_defanged` instead.

Headers on every delivery:

| Header | What it is |
| --- | --- |
| `X-LinkCtrl-Event` | The event name, so you can route without parsing the body. |
| `X-LinkCtrl-Delivery` | A UUID. **This is your idempotency key** — every retry of one event carries the same value, and no two events share one. |
| `X-LinkCtrl-Timestamp` | Unix seconds, and part of what is signed. Reject a timestamp too far from your own clock if you want replay protection. |
| `X-LinkCtrl-Signature` | `v1=` followed by a lowercase hex digest. |

### Verifying the signature

```
signed  = "<X-LinkCtrl-Timestamp>" + "." + <the raw request body>
digest  = HMAC-SHA256(key = the secret string exactly as it was shown to you,
                      message = signed)
header  = "v1=" + lowercase hex of digest
```

Three things to get right, because each produces a signature that never matches
and no clue why:

- **The key is the secret string as displayed** — the 64 lowercase hex
  characters, used as-is. Do not hex-decode it first. LinkCtrl stores 32 random
  bytes and shows you their hex; making the visible string the key means there is
  no encoding step to get wrong.
- **The message is the raw body**, byte for byte as it arrived. Do not parse and
  re-serialize the JSON before verifying — key order and whitespace will not
  survive it.
- **Compare in constant time.** `hmac.compare_digest` in Python, `hash_equals` in
  PHP, `hmac.Equal` in Go.

```python
import hmac, hashlib

def verify(secret: str, timestamp: str, body: bytes, header: str) -> bool:
    version, _, digest = header.partition("=")
    if version != "v1":
        return False
    want = hmac.new(secret.encode(), f"{timestamp}.".encode() + body,
                    hashlib.sha256).hexdigest()
    return hmac.compare_digest(digest, want)
```

### Delivery, retries and the log

Nothing is delivered on the request path. A link write queues one row per
subscribed webhook and returns; the scheduler drains that queue every thirty
seconds, under a leader lock, in batches of twenty.

**A batch is sent all at once**, so your receiver can see up to twenty concurrent
requests from this instance and should not assume they arrive in order. Each one
carries its own delivery id, which is what you deduplicate on.

A delivery is a success on **2xx** and a failure on anything else. A redirect is a
failure too, and is not followed: a receiver answering `302` is pointing this
server at a URL nobody registered. The `3xx` is recorded so you can see it.

A failed delivery is retried with a doubling backoff — 1m, 2m, 4m, 8m, 16m, 30m —
for **seven attempts spanning 61 minutes**, then abandoned. Nothing recovers an
abandoned delivery; if your receiver was down for longer than an hour, anything
older than that is gone. The attempt count is not configurable, because changing
it changes what *your* receiver experiences rather than what the instance costs.

Every attempt is recorded: the status, how many attempts it took, what the
receiver answered, and the error where there was no answer at all.

```sh
curl -sS .../api/v1/webhooks/$ID/deliveries -H "Authorization: Bearer $KEY"
```

`response_code` is `null` when nothing answered — a refused connection, a
timeout, or this instance declining to open the socket. The same log is on the
`/webhooks` page behind each registration's **Deliveries** button.

Finished deliveries are pruned by age. The window is `WEBHOOK_RETENTION_DAYS`,
thirty days by default, and there is no "keep forever" setting for it — this table
grows by one row per link write per enabled webhook.

Pausing a webhook stops it queueing anything. Deliveries already queued still go
out, and deleting the webhook takes its delivery log with it.

### What your endpoint has to be

**Publicly routable.** A URL naming a private, loopback or link-local address is
refused when you register it, and the address the name actually resolves to is
checked again every time this server opens the connection — so a name that later
points somewhere private is not delivered to either. That is a deliberate
difference from where a *link* may point, and `docs/SECURITY.md` explains why the
two are not the same question.

Up to **20** webhooks fit in a workspace. Every enabled one turns each link write
into another outbound connection, which is what the ceiling is about.

### Redis is not involved

The queue is Postgres. Redis on this instance is a cache and nothing durable
depends on it, webhook delivery included: flushing the cache loses no event.


## Automation rules

A rule is a standing instruction: *when this happens in this workspace, do these
things*. Write one at `/automation`, or through `/api/v1/automation`.

They are managed by the people accountable for the workspace — owner and admin,
behind `automation.read` and `automation.write` — for the reason webhooks are, and
one turn further. A webhook is an instruction to *report*. A rule is an
instruction to *act*: it runs unattended, and it can archive links.

### When a rule runs

**On the scheduler, under a leader lock, and never on a request.** There is no
run-now button and no endpoint that evaluates a rule, deliberately: creating a
link, following a short URL and calling the API all leave rules alone until the
next tick, which is every minute. On a multi-replica deployment exactly one
replica evaluates.

That has a consequence worth knowing before you write a rule expecting a
stopwatch: a rule fires *within about a minute* of its subject appearing, not at
the instant it appears.

### What a rule can watch for

Three triggers, and the vocabulary is closed. It is small on purpose — a large
trigger set is one nobody can test exhaustively, and untested triggers are where
surprises live.

| Trigger | When |
| --- | --- |
| `link.expired` | A link's `expires_at` passed. The link itself is not changed by expiring; the redirect path reads the timestamp. |
| `link.max_clicks` | A gated link's durable click budget ran out — the counter the gate spends transactionally, not the approximate `click_count` the analytics write afterwards. |
| `destination.blocked` | Somebody in this workspace was refused a destination by any tier. |

### What a rule can do

Three actions. A rule may hold at most three, and they run in the order you
listed them.

| Action | What happens |
| --- | --- |
| `notify` | One in-app notification per firing to the organization's owners, naming the rule, the count and the first five subjects. One per **firing**, not one per subject: forty items in an inbox is how an inbox stops being read. |
| `webhook` | One `automation.fired` event to every webhook subscribed to it. You cannot choose which event it sends, and that is the loop guard rather than a limitation. |
| `archive_link` | Archives the links that matched. Only available on `link.expired` and `link.max_clicks`, because `destination.blocked` has no link to archive — a rule that pairs them is refused when you save it rather than saved and silently ineffective. |

**There is no "disable" action.** An archived link and a disabled one produce the
identical answer on the redirect path, and `disabled` has nothing that restores
it — a rule writing it would leave links in a state the dashboard cannot get them
out of. Restore an archived link from its page.

### Firing once, and not looping

`last_fired_at` is not a note about the past. It is the point a rule has already
seen up to, and it is what stops a rule going off forever on the same link.

- A rule sees only subjects whose event time is **after** it, and the moment a
  rule fires that point moves past the last subject it handled.
- **Creating a rule arms it**, so it acts on what happens from then on rather than
  on your back catalogue. A rule you write this afternoon does not archive every
  link that expired last year.
- **Resuming a paused rule re-arms it**, so a rule switched off for a month does
  not deliver a month of backlog when you switch it back on.

The page calls the column *last fired or armed*, because it is both: a rule that
has never matched anything and a rule that fired a moment ago carry the same kind
of value.

It is **not** a record of when the rule was last examined. A rule below its
threshold, or one whose trigger simply has not matched, is looked at every minute
and this value does not move — deliberately, since that is what lets matches
accumulate. What decides whose turn it is on a busy instance is a separate thing
the scheduler keeps for itself; see *What one run costs* below.

Rules also cannot set each other off. Nothing an action produces is anything a
trigger watches for — which is why the `webhook` action emits only
`automation.fired`, and why archiving a link never moves its expiry.

### Firing after several, rather than after one

Every rule has a threshold, *fire after N*. Below it nothing happens and **nothing
is discarded**: matches accumulate across runs until the count is reached. "Tell
me after five refusals" therefore works on an instance where refusals arrive one
at a time.

The threshold cannot exceed the number of subjects one run handles (25), because
a higher one would be a rule that saves and never fires.

### What one run costs, and what it will not do

| Bound | Value |
| --- | --- |
| How often the scheduler evaluates | every minute |
| Rules considered in one run, across the instance | 100, least recently looked at first |
| Subjects one rule handles in one run | 25 |
| Actions on one rule | 3 |
| Rules in one workspace | 20 |

A run that hits either cap says so in the log and **loses nothing**: the rules it
did not reach are the ones the next run starts with, and a rule that matched more
than 25 subjects picks up the remainder on the next run. All of it is in the
`evaluation` block of `GET /api/v1/automation`, so a client never has to guess.

The first of those turns on a fact worth stating plainly, because the obvious
reading of it is wrong. A rule's turn is decided by when the scheduler last
**looked at** it, not by when it last **fired** — two different things, kept in
two different places. A rule that matches nothing has not fired, and a queue
ordered by firings would leave every idle rule permanently at the front and
whatever sat past the hundredth never evaluated at all. So an instance holding
more than 100 enabled rules evaluates all of them, a hundred a minute, in
rotation — five workspaces at the twenty-rule cap is where that starts to matter.

### What it records

Creating, editing and removing a rule are audit events. So is every firing, as
`automation.fired` — and that record's actor is the *rule*, not a person, which is
what makes an automated archive answerable afterwards. `linkctrl_automation_firings_total`
counts firings by trigger and outcome.
