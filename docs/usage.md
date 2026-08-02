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
| `/disputes` | The review queue: destinations somebody was refused and has asked you to look at. Needs `destinations.review`. |
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
full surface: everything, paged, with mark-read. The preview is deliberately
short, and nothing is only reachable through it.

Your **email address** opens a menu holding the administrative surfaces —
**Members**, **Invitations**, **Workspaces** and **Blocked destinations**, each
shown only if you hold the permission its page needs — plus **Reputation
feeds**, **Account** and **Sign out**. They live here
rather than at the top level because each is visited when something *changes*,
where the three top-level destinations are where work happens.

**Reputation feeds** is the one entry gated on no permission at all, and
deliberately so: it says whether this instance sends the destinations *you* type
to a third party, so an editor is exactly the reader it is for.

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
orgs.create
```

`apikeys.read`, `apikeys.write`, `org.delete`, `audit.read` and
`destinations.review` are never grantable to a key — a key that can mint keys
makes revoking a leaked one meaningless, an irreversible action should need an
interactive sign-in, the audit log ties a network prefix to a named person, and a
key that could allow a blocked destination could then point links at it.

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
| `401` | No valid credential — or the credential itself is being rejected. Invalid, revoked and expired keys are indistinguishable on purpose. |
| `403` | Authenticated but not permitted. The detail names the missing permission, which is useful rather than a disclosure. |
| `404` | Does not exist, or belongs to a workspace you cannot see. Someone else's resource is never a `403`, so ids cannot be probed. |
| `409` | Alias already taken. |
| `422` | Validation failed; `errors` names each field. Also how Phase 2 fields (`password`, `max_clicks`, `one_time`) are refused — loudly, rather than accepted and ignored. |
| `429` | Two different things, told apart by `type`: `account-locked` after repeated failed logins for one account, or `rate-limited` when your address is going too fast. The second carries `Retry-After`; the first is a fixed 15 minutes. Retrying the second is fine — retrying the first just extends it. |
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
refuses that destination any more. `409` means the host is already waiting.

Whoever holds `destinations.review` — the **owner** role, and no API key — reads
the queue with `GET /api/v1/disputes?open=true` and answers with
`POST /api/v1/disputes/{id}/allow` or `.../uphold`. Allowing deletes the
blocklist entry; upholding changes nothing. Both are audit events and both notify
the person who asked.

Two allows are refused with `409` rather than doing nothing: a
`punycode_homograph` or `url_credentials` refusal holds no entry to delete
(`liftable` is `false` on those), and an entry that came from
`LINKCTRL_DESTINATION_BLOCKLIST` would come back at the next restart — take it
out of the environment instead.

A `feed_reputation` refusal is `liftable` and deletes nothing, which is the one
allow that works that way. There is no blocklist row behind it — the verdict is
re-asked on every write — so the decision itself is the override: after it, that
host is neither refused nor sent to the feed again. It is scoped to the exact
host, so allowing `evil.example` says nothing about `login.evil.example`.

#### What this instance does with your destinations

`GET /api/v1/feeds` answers whether destinations are being sent to a third party,
and to whom. On a default instance it is `{"enabled": false}` and that is the
whole answer: nothing leaves. It is readable with any credential, because what it
describes is what happens to the caller's own data, and it is the same disclosure
the dashboard shows at `/feeds`.

```sh
curl -sS "$BASE/api/v1/feeds" -H "Authorization: Bearer $KEY"
```

There is no write counterpart. A feed is switched on in the instance's
configuration and nowhere else.

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
| Anything after the alias, on a link that does not forward paths | `404` — the same answer, so it cannot be used to find out which aliases exist |
| Too many misses from one address | `429` with `Retry-After` — see [configuration.md](configuration.md#rate-limits). Links already in the cache keep resolving, and paths that could never be an alias are not counted. |
| The server could not resolve it | `503` with `Retry-After: 1`. Deliberately not a `404`: that would claim the link does not exist, and a crawler or link checker believing it drops a live link. |

Query forwarding is per-link and off by default: set `forward_query` (a checkbox
on the link's edit form, a boolean in the API) and the visitor's query string is
merged into the destination, with the destination's own parameters winning on
conflict.

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

`HEAD` works and does not record a click.

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
