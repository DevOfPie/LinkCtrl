# Example add-ons

First-party add-ons this repository ships as examples. They are not part of the
product: nothing here is imported by `cmd/` or `internal/`, and an instance runs
one only because its operator pointed `LINKCTRL_ADDONS_DIR` at a directory
holding it.

| Add-on | Class | What it shows |
| --- | --- | --- |
| [pageviews](pageviews/) | `redirect-observe` | Counting redirects out of band, into a schema the host gives the add-on, with settings of all four declared types |

## Why one is built into the image

The demo instance runs `pageviews`, and that is a decision rather than a
convenience — [D265](../../docs/build-notes/decisions.md) deferred it to
[M68](../../docs/build-notes/phase-details/m68.md) in exactly those terms. The
Add-on manager is a page about add-ons, and on an instance running none it is an
empty table with an upload form: every column the milestone exists to show —
declaration class, held permissions, schema size, per-module latency, settings —
has nothing to render. So the module is built in the image's build stage and
copied to `/addons/pageviews/`, and `.env.demo` sets `LINKCTRL_ADDONS_DIR=/addons`.

**Only the demo turns it on.** The files are in every image; the variable is not
in every environment, and without the variable there is no add-on host at all.
That is the same switch an operator uses, which is the point — the demo is not
running a special build.

**The demo's copy cannot be uninstalled through the page.** The container's
filesystem is read-only, so the add-ons directory is too, and both install and
removal are refused with a message saying so — the page redirects back to itself
carrying the reason, which is what every refusal on it does so that a reload does
not re-post the upload. The API answers the same case with `503`. That is the
documented behaviour of a `:ro` mount rather than a demo-only limitation, and it
is the right one for an instance strangers can sign into.

## Building one by hand

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
    -o /tmp/pageviews.wasm ./examples/addons/pageviews
sha256sum /tmp/pageviews.wasm
```

Then put the digest into a copy of `addon.json.in` in place of `@SHA256@`, and
upload the pair through the Add-on manager or `POST /api/v1/addons`. The host
checks the digest before it writes anything, so a mismatch is a refusal rather
than a module that runs.
