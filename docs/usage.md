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
| `/links/{id}` | Everything about one link: edit destination, alias, title, description, expiry and tags; per-window analytics (7/30/90 days) with device, browser, OS, referrer, language and country breakdowns; recent activity; archive, restore and delete. |
| `/keys` | Mint, list and revoke API keys. |
| `/notifications` | Things the instance wanted you to know about, and mark-read. |
| `/account` | Your profile, password and appearance. |

It works without JavaScript. htmx makes search and filtering swap a fragment
instead of reloading, and that is the only thing it is used for.

### The header

Three destinations at the top level — Dashboard, Links, API keys — and on the
right, in order: the workspace switcher, a notification bell and your email
address.

The **bell** carries the unread count and, opened, shows the newest few unread
notifications with a **View all** link to `/notifications`, which is still the
full surface: everything, paged, with mark-read. The preview is deliberately
short, and nothing is only reachable through it.

Your **email address** opens a menu holding **Account** and **Sign out**.

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
  change. Redirects are 302 precisely so an edit takes effect.
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

Country breakdowns need a GeoIP database, which cannot be shipped in the image —
see [deployment.md](deployment.md#optional-geographic-analytics). Without one the page says the data is
unavailable rather than drawing a blank chart. Region and city are not stored even
with a database configured: nothing shows them, and city plus a timestamp is close
to a location history.

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

### Getting a key

Mint one in the dashboard at `/keys`, or on a headless host with
[`lctl`](cli.md#apikey). The token appears exactly once — only its HMAC is
stored, so it cannot be recovered afterwards. Lose it, revoke it, mint another.

Scopes are permission slugs you already hold, checked again on every request
against your current role. Demote the owner and their keys weaken immediately.

```
links.read   links.create   links.update   links.delete
tags.read    tags.write     analytics.read domains.write
members.read members.write  workspace.read workspace.write
```

`apikeys.read`, `apikeys.write` and `org.delete` are never grantable to a key —
a key that can mint keys makes revoking a leaked one meaningless, and an
irreversible action should need an interactive sign-in.

The `members.*` and `workspace.write` scopes are grantable but gate no endpoint
yet; member management is Phase 2. Granting them changes nothing today.

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
    { "field": "url", "code": "private_address",
      "message": "destination must not be a private, loopback or link-local address" }
  ]
}
```

| Status | Means |
| --- | --- |
| `401` | No valid credential — or the credential itself is being rejected. Invalid, revoked and expired keys are indistinguishable on purpose. |
| `403` | Authenticated but not permitted. The detail names the missing permission, which is useful rather than a disclosure. |
| `404` | Does not exist, or belongs to a workspace you cannot see. Someone else's resource is never a `403`, so ids cannot be probed. |
| `409` | Alias already taken. |
| `422` | Validation failed; `errors` names each field. Also how Phase 2 fields (`password`, `max_clicks`, `one_time`) are refused — loudly, rather than accepted and ignored. |
| `429` | Two different things, told apart by `type`: `account-locked` after repeated failed logins for one account, or `rate-limited` when your address is going too fast. The second carries `Retry-After`; the first is a fixed 15 minutes. Retrying the second is fine — retrying the first just extends it. |
| `504` | The request exceeded the server's deadline. Retry; narrow the window if it is an analytics query. |

Unknown JSON fields are rejected rather than ignored: a misspelled field silently
dropped means you believe you set something you did not.

### Redirect behaviour

| Situation | Response |
| --- | --- |
| Active link | `302` with `Location`, `Cache-Control: private, no-store` |
| Expired | `410 Gone` — distinct from 404 so crawlers and link checkers stop retrying |
| Unknown, archived or disabled | `404` |
| Too many misses from one address | `429` with `Retry-After` — see [configuration.md](configuration.md#rate-limits). Links already in the cache keep resolving, and paths that could never be an alias are not counted. |
| The server could not resolve it | `503` with `Retry-After: 1`. Deliberately not a `404`: that would claim the link does not exist, and a crawler or link checker believing it drops a live link. |

Query forwarding is per-link and off by default: set `forward_query` (a checkbox
on the link's edit form, a boolean in the API) and the visitor's query string is
merged into the destination, with the destination's own parameters winning on
conflict. Deep-link path forwarding is Phase 2. `HEAD` works and does not record
a click.

## Roles

| Role | Can |
| --- | --- |
| Owner | Everything, including deleting the organization. |
| Admin | Everything except deleting the organization. |
| Editor | Create, edit and delete links and tags; read analytics. **Cannot mint API keys** — an editor who could would be able to grant themselves scopes beyond their own role. |
| Viewer | Read links, tags and analytics. |

Phase 1 auto-provisions one personal organization and workspace per user, and
the account that claims the instance is its owner. Invitations and shared
workspaces are Phase 2, so in practice everyone is an owner of their own
workspace today — but the evaluator is real, and changing a role changes
behaviour immediately, including for existing API keys.

## Which workspace you are in

Every request acts in exactly one workspace. With one membership — which is
every account until invitations ship — there is nothing to choose and the
dashboard shows no switcher at all.

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
nothing carries it while the account follows last-used. Switching
(`POST /api/v1/workspaces/{id}/switch`) and pinning
(`PUT /api/v1/workspaces/default`, with `null` to go back to last-used) require
a signed-in session and answer `403` for an API key: a key acts in the workspace
its own row names, so switching would change nothing about its own requests
while repointing where you land.

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

Three things worth knowing. It needs the **`domains.write`** permission, which
owner and admin hold and editor does not: this is not one link, it is where
every stray visitor to the whole domain ends up. The destination is validated
exactly as a link's is, so `http` and `https` only and no private, loopback or
cloud-metadata addresses — a root redirect is the easiest thing on the instance
to reach, needing no link and no alias. And the redirect is a `302` that
intermediaries are told not to cache, so changing it takes effect immediately.
