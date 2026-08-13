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
| `/links` | Search the list; filter by status, folder, campaign or hostname; sort; create a link; page through with a cursor. The search box is the first control on the page, filters as you type and updates the address bar, so a reload or a shared URL shows the same view. Everything except search is behind **Filters**, and creating a link is behind **Create a link** — see [The links list](#the-links-list). |
| `/links/{id}` | Everything about one link: edit destination, alias, title, description, expiry and tags; per-window analytics (7/30/90 days) with device, browser, OS, referrer, language and country breakdowns, each with a share ring, plus a world choropleth over the country figures; recent activity; archive, restore and delete. |
| `/keys` | Mint, list and revoke API keys, and choose whether a new one reaches one workspace, one organization, or your whole account. The list is your account's, so it shows keys from every organization you belong to. Rotation is not here: it replaces the credential that made the request, and a browser session is not one. |
| `/links/{id}/qr` | The link's QR code, its style form and the downloads, on their own page. The same thing the **QR** tab on the link's page shows, on a page a bookmark can reach. |
| `/forgot`, `/reset/{token}` | Recovering a forgotten password. Public, because whoever needs them has no session. `/forgot` mails a single-use link and answers the same way whatever address you type; `/reset/{token}` is where that link lands. **Both need a mailer** — see [Recovering a forgotten password](#recovering-a-forgotten-password). |
| `/notifications` | Things the instance wanted you to know about. Opening one goes to what it is about and marks it read; a read one can be marked unread again. |
| `/disputes` | The review queue: destinations somebody was refused and has asked you to look at. Needs `destinations.review`, which is held instance-wide rather than by a role. The account that claimed the instance also appoints other reviewers here. |
| `/disputes/reviewers` | Who reviews disputes, and appointing or withdrawing them. Needs `instance.admin`, and the queue above shows the list in summary. Also the **Change who reviews** panel on `/disputes`. |
| `/feeds` | Whether this instance sends the destinations you type to a third party, and to whom. Read-only, and readable by everybody — what it describes is what happens to your own data. |
| `/members` | Who is in this organization, at what role, with role changes and removal. Behind `members.read` and `members.write` from an organization-wide membership. |
| `/workspaces` | The organization's workspaces: create, rename, delete. |
| `/invites` | Pending invitations, and issuing one. Organization-wide `members.write`. |
| `/organizations` | Create an organization of your own, behind `orgs.create`. |
| `/domains` | Hostnames this workspace has registered, their verification state, and the challenge record to publish. |
| `/webhooks` | Outbound subscriptions and their delivery log. |
| `/automation` | Standing rules the scheduler runs unattended, and whether each is paused. |
| `/campaigns` | Campaign labels, and the links filed under each. |
| `/account` | Your profile, password, appearance — and deleting the account. |

This table listed eight of these pages until 0.2.0 and omitted the rest, including three that share the identity menu with pages it did list ([F45](build-notes/deferred-findings.md)).

**The dashboard needs JavaScript**, and there is no `<noscript>` fallback — the
stance is recorded rather than defended in markup nobody reads. htmx makes
search and filtering swap a fragment instead of reloading, and it carries some
of the writes too: the QR logo upload applies the file as soon as you choose
one, which is a post over htmx and is why a refused upload answers `200` with
the reason in the panel rather than a status the swap would throw away.

Some pages are plain forms and links throughout and do keep working with
scripting off — the folder tree and the link filters are both said to below.
Read those as *these controls need no script*, which is what they are, rather
than as a fallback: nothing tests the scriptless path and no page is held to it,
so a page that works today may stop the next time htmx carries one of its
writes.

*(This paragraph said the dashboard worked without JavaScript, and that it was
used for search and filtering only. The first was already stale — the
requirement was settled deliberately, and well before 0.3.0 shipped — and the
logo upload is what made the second false too.)*

**Short links need none of it.** The redirect path is scriptless, which is the
part a visitor touches.

### The header

Two destinations at the top level — **Dashboard** and **Links** — and on the
right, in order: one bordered control naming the workspace you are in, with the
switcher as a chevron beside the name when there is anywhere to go, then a
notification bell and your email address. Below `sm` the bar is two lines rather than one, so nothing
is dropped on a phone and no page scrolls sideways.

**API keys was a third destination up here** and is now in the identity menu,
with the administrative surfaces. A key is minted once and then not thought
about; the top level is for where work happens.

The **bell** carries the unread count and, opened, shows the newest few unread
notifications with a **View all** link to `/notifications`, which is still the
full surface: everything, paged, with mark-read.

**Clicking a notification goes to what it is about and marks it read**, in the
bell and on the page alike. A dispute filing opens the queue at the row that is
waiting; an automation firing opens `/automation`; a domain warning opens
`/domains`; an accepted invitation opens `/invites`; a dispute decided in your
favour opens `/links`, which is where you can now create the link that was
refused. Three kinds lead nowhere and say so by offering nothing to click — an
audit-growth warning, because the audit log has no page and what to do about it
is `LINKCTRL_AUDIT_RETENTION_DAYS`; a dispute whose refusal was upheld, because
no page shows a refusal that stands; and, since 0.3.0, a new release being
available, because nothing in this product upgrades it and what the reader does
next is at a shell.

**A read notification can be marked unread**, which is the undo for having
opened one by accident. It sets the read timestamp back to nothing, so "when did
you first see this" is discarded — deliberately, by you. Over the API that is
`DELETE /api/v1/notifications/{id}/read`.

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
**Members**, **Invitations**, **Workspaces**, **Domains**, **Blocked
destinations**, **Webhooks** and **Automation**, each shown only if you hold the
permission its page needs — plus **API keys**, **Reputation feeds**, **Account**
and **Sign out**. They live here rather than at the top level because each is
visited when something *changes*, where the two top-level destinations are where
work happens.

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

### The links list

The page opens on the list, and the **search box is the first control on it** —
it filters as you type and pushes the result into the address bar, so a reload or
a shared URL shows the same view.

Everything else that narrows the list lives behind **Filters**: status, folder,
campaign, hostname and sort order. Folder, campaign and hostname appear only when
the workspace has any of those, because a select whose only option is *All* is a
control that can do nothing. **The panel opens by itself whenever one of those
filters is set**, so a list is never narrowed for a reason you cannot see, and
none of the query parameters changed when the controls moved — `?status=`,
`?folder=`, `?campaign=`, `?domain=`, `?sort=` and `?search=` are what they
always were.

**Create a link** is a panel too, directly under the filters and one click from
anywhere on the page. It opens on its own when a creation was refused, carrying
what you typed, the reason and — where the refusal was a rule that can be
wrong — the button that asks for a review.

*Campaigns* and *Folders* are a second bar under the header, on every page of the
links area rather than only on this one.

The **Filter** button beside the search box applies every control at once,
including the ones inside the panel — one plain form submission, so the filters
need no script even though the page around them does.

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
eight levels — those destinations simply offer no button. Every control on this
page is a plain form or link: operable from a keyboard, working with scripting
off, and never a drag. That is what these controls are rather than a fallback
the dashboard maintains — the note above says why the distinction matters.

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

### On-demand panels

One thing you reach occasionally is behind a panel rather than in the page
body: **Change who reviews** on the dispute queue. The panel opens over the
page you are on and closes on Escape or a click outside. A link's QR settings
used to be the second panel; with the link page behind tabs they live on the
**QR** tab instead, one click away.

**The panel is a page as well.** `/disputes/reviewers` serves exactly what it
holds, with the header and a way back, so you can bookmark it, open it in a
second tab, or share the URL — and a browser too old for the popup renders the
panel inline instead of hiding it. The panel's own **Open as a page** link is
where that URL comes from. `/links/{id}/qr` serves the QR settings the same
way, and the codes list on the QR tab links through it.

Neither surface changes who may do anything. The QR settings are still
`links.update` and the reviewer roster is still `instance.admin`; the queue shows
who reviews it either way.

### QR codes

Every link has one, and always at least one. There is nothing to create and no row to make: open a link's
page and a small version of the code is drawn beside the link's name, at the top.
Clicking it opens the **QR** tab, which holds the full code, the style form and
**Download the PNG** and **Download the SVG** buttons.

**Two formats, one picture.** The PNG is the file most programs open; the SVG is
vector text and is the one to use for anything that will be resized again. They
are generated from the same grid at the same size, so they are the same image.

**The code encodes your short URL with `?src=qr` on it**, and that is what makes
a scan countable. A camera sends no `Referer` header, so without the parameter
every scan would arrive indistinguishable from somebody typing the URL by hand.
Scans show up in the link's **Referrers** breakdown as `qr`, beside `direct` —
counted, deduplicated by visitor and filtered for bots like every other click.
That row stays one row however many codes the link carries; which code was
scanned is the per-code breakdown's question, below.
One thing follows: somebody who types `?src=qr` by hand is counted as a scan.

### More than one code for a link

A print run and a shop-window card pointing at the same link used to be the same
picture, so their scans were the same number. **Add another code** on the QR tab,
give it a name, and it becomes a code of its own — the same destination, its own
row in the breakdown.

Every code prints an identity in its picture, as `&qrc=<something>` beside
`?src=qr`. That is what the redirect reads to say which code was scanned. The
name you type is for you: it never appears in the picture, in a URL, or anywhere
a visitor can see it.

**Your original code gets one too, at the moment you add the second.** Until then
it has nothing to be told apart from, so it carries the picture it always had
with nothing added to the payload; adding a second code is what gives it a
`qrc` of its own, and from then on what you download carries it.

**That makes what it encodes a little longer, and both codes are re-measured
against it.** A longer payload sometimes needs a bigger grid of squares. Almost
always the size you set is kept and only the squares behind it change, and there
is nothing worth saying about it; when the size is too small to hold the bigger
grid with a margin anything can read — which takes a code sitting at the very
bottom of the size control — it is raised to the smallest size that does and you
are told, with both numbers. Over the API the same rise comes back as `refit` on
the create. What is never allowed is a code that says one size and produces
another.

**Nothing you have already printed changes what it counts as.** Every copy of the
original code already printed, mounted or published carries no `qrc` at all, and
a scan of one is counted against whichever code is the link's **default** — which
starts as the code it always was. So the copy on last year's poster and the copy
you download today are the same row in the breakdown, and there is nothing to
reprint.

### The default code, and removing one

**The default is the code a picture with no `qrc` on it is counted against.** One
of a link's codes always holds that role, and every picture printed before codes
had identities relies on it.

Each row in the codes list carries **Make default** and **Remove**. Making a code
the default moves where those untagged scans land — including the ones already
recorded, because what they record is *no code* rather than a code that has gone.
Nothing about any picture changes when you move it.

**Any code can be removed once a link has two**, the default included. Removing
the default hands the role to the oldest code left and says which one, because
that is where your old posters start being counted. A link's **last** code cannot
be removed: a link always has a code, and the pictures already printed of it have
to resolve somewhere.

**Restore defaults** clears the style of the code you have selected — the colours
and the size — and leaves the code, its name and any logo on it alone.

A link carries at most **twenty** codes. That is the number that keeps the codes
list a list and the breakdown a chart.

A code has no destination, no expiry and no gate of its own. Those belong to the
link, which is what makes changing the link's destination change every printed
code at once. A code that pointed somewhere else would be a second link, and you
can make one of those.

**Removing a code keeps what it recorded and stops it growing.** The rows it
earned stay in the breakdown, marked as removed. A scan arriving afterwards from
a picture printed with it is counted as the link's **default** code, because the
identity it prints is no longer one this link recognises — it is not credited
back to the code that is gone, and it is not stored as an unknown value either.
So a chart read across the removal shows one line stopping and another taking up
the traffic, which is worth knowing before you remove a code that is still in the
world.

**A name can be changed at any time and changes nothing else.** The identity in
the picture is fixed when the code is made and is never rewritten, because it is
printed.

**Restyling changes the drawing and never the content.** The form takes a
foreground colour, a background colour and a **size in pixels** — 64 to 2048,
set with a slider that stops at 128, 256, 300, 512, 600, 1024, 1200 and 2048 or
with the box beside it, which takes any number in range. **Restore defaults**
appears once a style is stored, and clears the size along with the colours.

The bottom of that range is not reachable for every code: a longer URL is a
bigger grid of squares, and a grid with no room left for a margin is refused with
the smallest size that code can be drawn at in the message — 70 pixels for a
short link, more for a long one. The slider starts at that number rather than at
64, and drops any stop below it.

**The preview keeps its own size.** The frame beside the form is 18rem square
whatever you set, and a code larger than that is drawn scaled down to fit it, so
what you read the size off is the control rather than the picture. Before this
the frame grew with the setting until it hit the edge of the column, which made
the largest size a page you had to scroll rather than a file you could print.
*(A caption under the picture repeated the served size until 2026-08-13; it
called that number the "stored size", which reads as an amount of data rather
than as a measurement of the image.)*

**The size you set is the size you get, exactly.** Ask for 500 pixels and the
file is 500 pixels across. A code is a grid of squares and an arbitrary pixel
size does not divide evenly into it — 300px over a 29-square code is 10.34 pixels
a square — so something has to absorb the remainder. It is the empty margin
around the code: the squares themselves stay whole, which is what keeps the SVG
and the PNG the same picture, and the margin is white space that can be any
number of pixels wide. *(This used to say the size snapped to the nearest one
that kept the squares whole, and it did until 2026-08-12.)*

**The margin aims at four squares and never goes below three.** Four is what the
specification asks for; three is a quarter under it, and it is measured rather
than assumed — every size and version this product draws is decoded at five
simulated viewing distances through two independent decoders before that number
is allowed to stand. On a large code in a small picture there may be no setting
that lands between three and five at all, and the margin then comes out *wider*
than five rather than narrower than three: extra white costs nothing to read, and
a thin margin costs a scan. A style written over the API can ask for a wider
quiet zone in squares and is drawn with it; setting a size on that code from the
form replaces it with the margin the size implies.

**The error-correction level is chosen for you, and the API can only raise it.**
It is a tradeoff between how much damage a printed code survives and how tightly
it is packed, and there is no way to judge it from a dashboard. Since 0.3.0
every code is drawn at **the strongest level that does not make the symbol any
bigger** — a QR symbol steps between versions, and correction below the next
step costs nothing, so an ordinary short URL comes out at `Q` in exactly the
picture `M` would have produced. The `PUT` below sets a **floor**: asking for a
stronger level than the free one gets it, at whatever size that costs; asking
for a weaker one changes nothing, because there is no saving in less correction
at the same density. Saving the form afterwards keeps whatever was set.

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
# The picture, as vector text.
curl -sS "$BASE/api/v1/links/$LINK_ID/qr.svg" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" -o code.svg

# The same picture, rasterised. Capped at 2048px; a stored style that draws
# larger than that is refused rather than rasterised.
curl -sS "$BASE/api/v1/links/$LINK_ID/qr.png" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" -o code.png

# What it encodes, and how it is drawn. The top-level `size` is the output size
# in pixels — read-only, and equal to `style.size` when one is stored.
curl -sS "$BASE/api/v1/links/$LINK_ID/qr" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY"

# Restyle it. An omitted field is its default, so {} is plain black on white.
# `style.size` is the picture in pixels and is what the dashboard writes; give
# `margin` and `scale` instead to state the geometry in squares directly.
curl -sS -X PUT "$BASE/api/v1/links/$LINK_ID/qr" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"style": {"foreground": "#123a6b", "level": "Q", "size": 512, "scale": 16}}'
```

Seeing a code is `links.read` and styling one is `links.update`: a QR code is a
picture of the link's own short URL, so anybody who can see the link can see its
code.

#### A logo on a code

**The one thing in this product that accepts a file**, and it is `PUT` with a
`multipart/form-data` body. It takes `links.update`, like every other change to
how a code is drawn, and an API key that holds it may use it. The QR tab on a
link's page does the same thing from a browser.

Two addresses, one operation — the same relationship `qr.png` and
`codes/{slug}/image.png` have. The shorthand answers for the link's **default**
code, which is a role rather than an address: it is whichever code an untagged
picture is counted against, and it can be moved. A code's own slug in the
collection is the address that does not move.

```sh
# The link's default code. Upload; the part must be named `logo`, and the
# filename is ignored.
curl -sS -X PUT "$BASE/api/v1/links/$LINK_ID/qr/logo" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" \
  -F 'logo=@brand.png'

# A named code, by the slug printed in its payload.
curl -sS -X PUT "$BASE/api/v1/links/$LINK_ID/qr/codes/$SLUG/logo" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY" \
  -F 'logo=@brand.png'

# Remove it. The code stays; the image goes. Idempotent, at either address.
curl -sS -X DELETE "$BASE/api/v1/links/$LINK_ID/qr/logo" \
  -H "Authorization: Bearer $LINKCTRL_API_KEY"
```

The shorthand answers under `qr` and the collection under `code`, like every
other operation in their respective families.

Six things are worth knowing before you script it.

**PNG and JPEG only, decided by the bytes.** Neither the filename nor the
`Content-Type` you send is read, so a `.png` holding a JPEG works and a `.jpg`
holding something else does not. **An SVG is refused** and says so: it is a
document that can carry script and fetch other files, and this product will not
serve one it did not write.

**What is stored is a PNG this server encoded**, never your file. That strips
metadata as a side effect, so do not use this to keep an original.

**Three bounds, and exceeding any of them is a `422` naming which one and what
your image measured**: the request body stops at 1,048,576 bytes, the image at
1024 pixels a side, and the re-encoded result at 1,060,000 bytes. The middle one
is checked from the file's header before anything is decoded.

**A large image is resized rather than refused.** A stored logo holds at most
262,144 pixels in total, and that is a target rather than a bound: anything over
it is scaled down to fit with its aspect ratio kept, and the response carries a
`resampled` object naming what you sent and what was stored. It is absent when
nothing was resized, so its presence is the signal. *(That figure was a second
header bound until now, and an image over it was a `422`.)*

**Uploads have their own rate limit** (`UPLOAD_RATE_PER_MIN`, thirty a minute by
default) on top of the API's, so a `429` here can arrive while everything else
is still answering.

**A logo changes the picture in two ways, and one of them is `level`.** The
image covers a centred square three tenths of the code's width — 9% of its area —
and the code is drawn at **error-correction level H**, which is what lets a
reader recover a code with part of it covered. So `level` stops being yours to
choose while a logo is there: a `PUT` naming another one is **accepted and
answered with `H`** rather than refused, because this endpoint replaces the
style whole and an omitted `level` sets no floor at all — refusing would fail
a request that only changed a colour. The response and every later `GET` report
what was applied, so nothing is silent. **Removing the logo returns the code to
the rule** rather than to a remembered level — the payload is unchanged, so a
picture already printed still resolves, and level H on a code with nothing
covering it is about 30% more modules a side than it needs, which is 30% less
distance a phone reads it from. *(It stayed at H until 0.3.0.)* H packs more
modules into the symbol, and **the drawn size is held where it was** rather than
allowed to grow with it: the style is re-fitted against the larger symbol so the
picture stays at the size the code was already drawn at, which is what stops a
code near the raster ceiling from crossing it and refusing to download. Since
2026-08-12 that is the identical number rather than the nearest achievable one,
because the margin carries the remainder. This paragraph said `size` can grow
when you add a logo until 0.3.0.

**One case still grows**, and it is the one where holding the size cannot be
done: a code drawn below the floor its level-H symbol needs — `2 × (module
count + 6)` pixels, and H's module count is the larger one — has no size to be
re-fitted into, so the style is left as it stands and the picture is drawn from
its `margin` and `scale`, which is bigger. Refusing the upload instead would say
no to a picture the SVG draws correctly, and squeezing it would serve a quiet
zone nothing can read. It takes a code already close to its own floor to reach.

There is no operation that reads a logo back: `has_logo` on the code says
whether one is there, and both picture endpoints draw it.

**Uploading to a default code that has never been styled creates its stored
row**, so `stored` turns true alongside `has_logo`. The bytes live on the row, so
there has to be one; the style written is the one the code was already being
drawn at, plus the level above.

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

**`qr` stays one row however many codes a link carries.** It answers *how many of
these visits came from a QR code at all*, which is a different question from
*which code*; the per-code split is its own section on the link page and its own
`qr_codes` field in the API's answer.

The bare `qr` is also a *stored* value, and there it means something narrower: a
scan that named no code. Those are counted against the link's default code in the
per-code split, which is what lets a picture printed before codes had identities
and one printed since be the same row.

Whether a QR code is scanned or its URL is typed cannot be distinguished — the
label travels in the URL, and anybody can type it.

Resolving a country needs a GeoIP database, which cannot be shipped in the image
— see [deployment.md](deployment.md#optional-geographic-analytics). **What is
drawn follows the data rather than the setting**: a link with countries in the
window you are looking at gets the ranked list and the map whether or not a
database is configured now, because the rows are already resolved and a database
is only how new clicks join them. Where there is neither — no country in *that
window* and no database — the page says the data is unavailable rather than
drawing a blank chart, and the map is not drawn at all: a world coloured entirely
"unknown" would be a picture of nothing that looks like a picture of something.
The window is part of the test, so a link whose countries are all older than the
one you have selected meets the sentence until you widen it. With a database and no
clicks yet, it is the ordinary *no data yet* and not a claim about the instance.
Region and city are not stored even with a database configured: nothing shows
them, and city plus a timestamp is close to a location history.

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

**Reach**, and there are three of them. A key is bound to the workspace you
created it in and acts only there. The two wider choices are the *Reach* control
on the form, `"org_wide": true` on the API, or `--org-wide` on the CLI. Either
needs `apikeys.write` held through an **organization-wide** membership: a role you
hold in one workspace issues keys for that workspace, which is the same rule that
stops a workspace-scoped admin re-roling an organization-wide member. The key
list, `lctl apikey list` and the API all say which of the three a key is.

| Reach | Acts in | Asked for by |
| --- | --- | --- |
| **Workspace** | The one you created it in | The default — nothing |
| **Organization** | Every workspace in that organization, and no further | `organization_id` on the API, `--pin` on the CLI |
| **Account** | Every organization you hold an organization-wide membership in | Nothing, once the key is unpinned |

Unpinned means account-wide: **a key belongs to your account, not to the tenant
you happened to be standing in.** Each request still resolves exactly one
workspace, the way a sign-in does, following where you are working. If you want
the key frozen to today's organization, pin it — and pin it now, because a
rotation may narrow a key's reach and never widen it.

An account key **reaches an organization you join later**. That is what account
means: the alternative is a key whose reach is a snapshot of a membership list
you cannot see or correct. It does *not* reach an organization where your role is
scoped to a single workspace — mint a key in that workspace instead.

**Its permissions differ per organization.** The scopes are intersected with your
role *there*, on every request. Own one organization and the key does what an
owner can; be a viewer in the next and the identical key can only read. This
surprises people, and it is the correct behaviour: a credential cannot be more
than the person it acts as, anywhere.

Every key issued before 0.3.0 is pinned to the organization it was created in and
stayed that way. The upgrade changed no key's reach.

Scopes are permission slugs you already hold, checked again on every request
against your current role. Demote the owner and their keys weaken immediately.

```
links.read   links.create   links.update   links.delete
tags.read    tags.write     analytics.read domains.write
members.read members.write  workspace.read workspace.write
orgs.create
```

`apikeys.read`, `apikeys.write`, `org.delete`, `audit.read`, `webhooks.write`,
`automation.write`, `audit.read.instance`, `destinations.decide` and
`instance.admin` are never grantable to a key — a key that can mint keys makes revoking a leaked one
meaningless, an irreversible action should need an interactive sign-in, the audit
log ties a network prefix to a named person, a key that could allow a blocked
destination could then point links at it, a webhook or an automation rule keeps
running after the credential that registered it is revoked, and a key that could
appoint a reviewer would widen its reach by manufacturing somebody else's.

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

The successor is **identical or narrower**. Same workspace, same name; scopes are
this key's unless you name a subset, and a scope this key does not hold is refused
rather than dropped. Its expiry is the predecessor's *lifetime* from now, so a
30-day key rotates into another 30-day key and a key that never expires rotates
into one that never expires.

Reach is the second axis of *narrower*. Omit it and the successor keeps this key's;
send `"reach": "organization"` and an account-wide key rotates into one pinned to
the organization the request resolved into. **The reverse is refused.** A pinned
key sending `"reach": "account"` gets a `422` — a successor may not reach more
organizations than the key it replaces, and widening on the strength of a token
alone is exactly what rotation must not be. Widen by minting a new key from a
session.

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

### Revoking a key

`DELETE /api/v1/api-keys/{id}`, or the button on `/keys`. It takes effect on the
key's next request; nothing about a key is cached. Revoking your own key is not
audited, because you are the record.

Holding `apikeys.write` through an **organization-wide** membership also lets you
stop somebody else's key, and what that does depends on the key:

- **Pinned to your organization** — revoked outright. Your organization was all
  it reached, so cutting the reach and destroying the credential are the same act.
- **Account-wide** — your organization is cut out of its reach. The key stops
  resolving into your tenant and keeps working for its owner elsewhere, because
  it belongs to an account you hold no authority over. The record is
  `apikey.reach_revoked` rather than `apikey.revoked`, so an incident review can
  tell "stopped" from "stopped here".

You do not choose between them, and a key you may not act on answers `404` rather
than `403` so ids cannot be probed.

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
| `404` | Does not exist, or belongs to a workspace you cannot see. Someone else's resource is never a `403`, so ids cannot be probed. A spent, lapsed or unknown password-reset token is `reset-not-valid` here rather than `410`, and so is a token for an account that cannot be recovered — `410` would concede that the token existed. |
| `409` | Alias already taken. |
| `422` | Validation failed; `errors` names each field. |
| `429` | `rate-limited`: your address is going too fast. `Retry-After` says how long to wait, and waiting works. This used to be two types — the other was `account-locked` — and it is one now, because which of them you got answered whether the address you named is registered. A locked account is a `401` like every other sign-in refusal; the lockout is fifteen minutes and further attempts extend it, so the `401` body says so whether or not one is in force. |
| `503` | `no-mailer`, from the two account-recovery endpoints only: this instance has no SMTP relay, so it cannot send the message the operation *is*. Not `403` and not `404` — the mechanism exists and is unavailable, so a retry after the operator configures a relay succeeds. |
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

**`qrc` is the second reserved parameter**, and it says which of a link's QR
codes was scanned. It is only read beside a recognised `src`, and its value must
be one this link actually issued: anything else is counted as the link's default
code and is never stored, which is what stops a stranger writing rows into your
analytics by editing a URL. It is not stripped either, for the reason `src` is
not — it is a label rather than a credential, and it is no more evidence than
`src` is.

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

Nothing else is kept, and the link refusal is most of why: an organization that
can be deleted holds no links, so there are no aliases left to protect. The
analytics rollups are the part the refusal does not cover — `link_click_daily`,
`link_dimension_daily` and `workspace_click_daily` carry a workspace id with no
foreign key, so nothing cascades them and they used to outlive the tenancy they
described. They are deleted explicitly in the same transaction as of 0.2.0.
Unreachable before that rather than exposed, since every reader scopes to a live
workspace; what they were is aggregate data with no owner. Aliases that had
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

## Recovering a forgotten password

**Only if this instance has a mailer**, because the recovery is the mail. With
`LINKCTRL_SMTP_HOST` unset there is no *forgot your password?* link on the
sign-in page, `/forgot` says the instance cannot send mail, and
`POST /api/v1/auth/forgot` answers `503`. That is deliberate rather than a gap
being papered over: everything else that uses the mailer has a second channel —
an invitation has a copyable link, a notification is in the dashboard — and this
has none, so a page that said "check your inbox" would be lying. On such an
instance the route back is the operator, with database access, and
[operations.md](operations.md#moving-the-instance-principal) covers the one
account that has a command instead.

With a mailer, the flow is three steps and no support ticket:

1. **`/forgot`**, or `POST /api/v1/auth/forgot` with `{"email": "..."}`. The
   answer is the same whatever you type — the page says a message is on its way
   and the API answers `202` — so neither can be asked whether an address has an
   account. The answer goes to the address instead: an address that cannot be
   recovered receives a message saying no link was created, which is what
   registration already does for an address that is taken.
2. **Open the link.** It works once and lapses after an hour, and asking again
   replaces it. Choose a password of at least twelve characters, the same floor
   every other password in this product has.
3. **Sign in with it.** No session is started for you, deliberately — see below.

**What setting it does**, all of which the page and the mail say:

- **Every session on the account is signed out**, including any you did not
  open. That is the point: a recovery that left somebody else's browser signed
  in would have recovered nothing.
- **Every other outstanding reset link stops working**, for the same reason.
- **API keys keep working.** A key is a separate credential with its own
  rotation, and revoking them here would turn a recovery into an outage for
  whatever it was minted for. If you are recovering because you believe the
  account was reached by somebody else, revoke the keys yourself at `/keys`.
- **No session is started.** You type the new password at the sign-in form,
  which is also the proof you know it.
- **The reset is audited** as `password.reset`, with the account as the actor and
  a network prefix rather than an address. It is an instance-wide record rather
  than an organization's, because nobody was signed in to any organization when
  it happened — so it is read at `GET /api/v1/instance/audit` by whoever holds
  `audit.read.instance`.

A link that has been used, has lapsed, or names an account that cannot be
recovered — a suspended one, or one that signs in some other way — is answered
`404` and the page says the same words for all of them. The endpoint cannot be
asked which, on purpose.

Recovery shares the sign-in rate limit, per address, so alternating between the
two surfaces does not double anybody's budget. The cost of the identical answer
is stated rather than hidden: this instance will mail an address that never
registered, if somebody types one in.

## Deleting your account

At the bottom of `/account`, or `DELETE /api/v1/account`. Both act on **your own
account and nothing else**: there is no way to delete somebody else's, and there
is deliberately no administrative one — who may end another person's account is
a permission question this product has not answered.

You confirm with your own password and, on the page, by typing `DELETE`. **An
API key cannot do this**, however it is scoped. The credential is not the
person, and a leaked key must not be able to delete its owner.

**Two things stop it, and each says what to do about it.**

- **You administer this instance.** Move the principal to another account first
  with `lctl instance principal move --to <email>` — see
  [operations.md](operations.md#moving-the-instance-principal). Deleting the one
  account that can administer the box leaves no way back that does not involve
  the database.
- **You are the only owner of an organization that still exists.** Make somebody
  else an owner, or delete the organization, and the refusal says which
  organizations are blocking. Every self-registered account owns a personal
  organization, so this is the one most people meet.

Being left belonging to **no** organization is not a refusal. It is the ordinary
way to arrive here, and an account in that state can still sign in and still
delete itself — through the API, since every dashboard page but the ones about
joining an organization needs one.

**What goes immediately**, in a single transaction: every session, every API key,
every membership, your notifications, any outstanding password-reset link, and
any instance-level grant you hold. Your address becomes available for a new
account. When the call returns there is no credential that reaches the account.

**What stays, with you taken out of it.** The audit log and the
destination-dispute queue keep their rows — they record what happened, and one
that vanishes with the person is not a record. An hourly job replaces your name
and address in them with `deleted account`, and clears the address, name and
password from your account row. Access does not wait for that job; only what is
left of your name does.

The same job reaches three records that are **about** you but belong to somebody
else, and it reaches them because a record you cannot see is still a record with
your address in it: the invitation you joined by, which your organization's
invitation list would otherwise go on showing in full; the notification that told
whoever invited you that you had accepted, in the sentence they read as well as in
the detail behind it; and the list of outgoing administrators on an
instance-principal handover, if you were one. An invitation still **outstanding**
to your address is deliberately left alone — it is an offer to an address, and
your address became available again the moment you deleted the account.

**What is not deleted at all: your links.** They belong to the workspace, and the
workspace outlives you leaving it — as do the QR codes, folders and campaigns
inside it. If you want them gone, delete them before you delete the account.

Two things worth knowing before you rely on this:

- **The audit log still tells one erased person apart from another**, because the
  actor id survives while the name becomes a constant. That is deliberate — a
  trail in which every departed actor looks identical cannot be read — and it
  means the remainder is pseudonymous rather than anonymous. `docs/SECURITY.md`
  says exactly what is and is not claimed.
- **Your address can be registered again**, by you or by somebody else, and the
  new account is a different account. It inherits nothing. Old audit entries
  under `deleted account` are not theirs, and the ids differ.

## Which workspace you are in

Every request acts in exactly one workspace. With one membership — which is
every account that has not accepted an invitation — there is nothing to choose
and the dashboard shows no switcher at all. It still says where you are: the
header names the current organization and workspace whether or not there is
anywhere to switch to.

Once there is more than one, the header's workspace box grows a chevron —
label, a hairline divider, then a chevron button. It opens a menu hanging off
the box, listing the workspaces you can move to and **not** the one you
are already in — that one is named beside it in the same box, as
*organization · workspace*, which is the label that appears at every
membership count including one. Switching moves *that browser*,
immediately and for the rest of the session, so two windows can sit in two
workspaces. It is also remembered: the next time you sign in, you start where
you last were.

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
