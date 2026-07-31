# `lctl`

The operator CLI. It shares the server's configuration loading and database
layer, so what it reports is what the server would see.

It also acts as a **named user** rather than as root: `apikey` resolves `--user`
to an identity and then goes through the same service calls and the same
permission checks the API does. Someone with database access could bypass RBAC by
hand anyway; the point is that the supported path does not, because a CLI that
quietly ignores permissions is where the exception becomes the habit.

## Running it

Inside compose, `lctl` sits beside the server in the image:

```sh
docker compose exec -T app /lctl migrate status
```

For one-shot use without a running container — note `--entrypoint`, since the
image's entrypoint is the server:

```sh
docker compose run --rm --entrypoint /lctl app migrate status
```

From a checkout:

```sh
go run ./cmd/lctl migrate status
```

It reads the same environment as the server. From a checkout that means
`LINKCTRL_APP_ENV=development` must be in the *environment* for `.env` to be
read at all — configuration loading checks that variable before it reads the
file. `make migrate-status` and friends handle this; see
[development.md](build-notes/development.md).

## Commands

```
config check       Validate configuration, reporting every problem at once
migrate up         Apply pending migrations, then ensure partitions exist
migrate down       Roll back the most recent migration
migrate status     Show applied and pending migrations
partitions ensure  Create partitions for the current and next two months
apikey create      Issue an API key   --user --name --scopes [--expires-in]
apikey list        List a user's API keys                          --user
apikey revoke      Revoke an API key                         --user --id
seed               Generate a load-testing dataset  --links --clicks [--reset]
demo               Fill an instance with demo data              [--reset]
version            Print version information
```

### `config check`

```sh
$ lctl config check
configuration OK
```

Prints every problem at once when something is wrong, each line naming the
variable. Run it before a deploy rather than reading a crash loop.

### `migrate`

```sh
$ lctl migrate status
VER    STATE    NAME                                     APPLIED
100    applied  00100_extensions.sql                     2026-07-30T05:14:29Z
200    applied  00200_identity.sql                       2026-07-30T05:14:29Z
…
```

`up` applies pending migrations and then ensures partitions exist — partitions
are created by application code, never declared in SQL, so the two belong
together.

`down` rolls back one migration. Test it on a copy: `down` migrations drop
columns, and a rollback after real traffic loses whatever those columns held.

The server does all of this at boot unless `MIGRATE_ON_START=false`, which is the
setting to use if you want migrations to be a deliberate step.

### `partitions ensure`

```sh
$ lctl partitions ensure
partitions ensured (0 created)
```

Creates monthly partitions for `click_events`, `visitors` and `audit_logs` for
the current month and the next two. The background scheduler runs this hourly,
so it is normally only needed after a restore or when investigating. The
headroom is deliberate: creating next month's partition on the last day of this
one is a single point of failure with a hard deadline, and two months turns a
missed run into a warning rather than an outage.

Nothing here *drops* partitions. Retention is enforced by the hourly `retention`
job instead, which drops whole months once they are entirely outside the window —
see [operations.md](operations.md#retention).

### `apikey`

Creating the first key on a headless instance is the reason this exists — minting
one through the API needs a browser session.

```sh
$ lctl apikey create --user you@example.com --name ci-deploy \
    --scopes links.read,links.create --expires-in 720h
created ci-deploy (lk_live_iszzewpi) for you@example.com
scopes: links.read, links.create
this is the only time the key is shown:
lk_live_iszzewpi_4oppgem7xFr9ZhKfKTEG4tgy3dXksX2C2X9uxuzhWss
```

The token goes to **stdout** and everything else to stderr, so redirecting
captures the key and nothing else:

```sh
lctl apikey create --user you@example.com --name ci --scopes links.read > key.txt
```

`--expires-in` takes a Go duration (`720h`, `2160h`); omit it for a key that never
expires. Scopes must be permissions that user's role grants —
`--scopes apikeys.write` is refused, because a key that can mint keys makes
revoking a leaked one meaningless.

```sh
$ lctl apikey list --user you@example.com
ID                                    PREFIX            NAME      STATE   LAST USED  SCOPES
019fb19b-6fa9-7932-9de0-81810c2db7b2  lk_live_iszzewpi  ci-smoke  active  never      links.read,links.create
```

`STATE` is `active`, `expired` or `revoked`. `LAST USED` is written asynchronously
on a coarse cadence — it answers "is this key still in use", not "when exactly".

```sh
$ lctl apikey revoke --user you@example.com --id 019fb19b-6fa9-7932-9de0-81810c2db7b2
revoked 019fb19b-6fa9-7932-9de0-81810c2db7b2
```

Revocation takes effect on the key's next request; nothing about keys is cached.

### `version`

```sh
$ lctl version
```

Version, commit and Go version, from build-time ldflags. `linkctrl version` and
`linkctrl --version --json` report the same for the server.

### `seed`

Generates a dataset for load testing. The defaults are the numbers the redirect
SLO is defined against, and seeding them takes about 90 seconds:

```sh
$ lctl seed --reset --links 100000 --clicks 5000000
reset: previous seed removed
created 4 partitions covering the seeded range
links: 100000/100000
clicks: 5000000/5000000
updating click counts…
analyzing…
seeded 100000 links and 5000000 click events in 1m25s
aliases: ld0 … ld99999 on localhost:8080
```

`--reset` truncates `click_events` and `visitors` — **all of them**, not only
seeded ones — and hard-deletes links matching the prefix *in the seeded
workspace only*. The prefix is restricted to lowercase letters, digits and
hyphens, so it cannot smuggle `LIKE` wildcards into that delete. The command
refuses to run at all when `APP_ENV=production` unless `--force` is given,
because a load-test dataset in a production database is not a mistake anyone
should be able to make with the up-arrow key.

Two things about seeded rows are worth knowing, because they are not quite real
ones. Links carry their destination URL directly and have no `destinations` row:
the redirect path reads `links.primary_url` and nothing else, so resolution is
identical, but the editing surface is not exercised. Click events carry random
visitor hashes rather than real HMACs, which is what a hash looks like to every
query that reads one.

`--seed N` fixes the PRNG, so two runs produce the same dataset and two load
results are comparable. `make seed` is the small development variant; `make
seed-slo` is the SLO dataset. See [slo.md](slo.md).

### `demo`

Fills an instance with a workspace worth looking at. `seed` is for load tests;
this is for seeing the product.

```sh
$ lctl demo --reset
reset: previous demo data removed
links: 21
clicks: 31008
rollups computed

demo data ready for you@example.com
links are at http://localhost:8080/<alias>, dashboard at http://localhost:8080
```

Around twenty links with titles, tags, descriptions and destinations, and a month
of click history with weekday seasonality, a launch spike, bots, and a spread of
devices, browsers, countries, languages and referrers. Every status the dashboard
can render is present: an archived link, an expired campaign that answers `410`,
one in the trash, one with query forwarding on, and two with generated aliases.

It needs a user to own the data, so claim the instance first — through the setup
form or `POST /api/v1/auth/setup`. `--user` picks one; the default is the
earliest.

Two properties are worth stating because they are what make it trustworthy.
Links are created **through the same service call the REST API uses**, so alias
policy, destination validation and tag creation all run: the dataset cannot
describe a state the product could not reach. Click history is written directly —
the redirect path can only make traffic for right now — but every column matches
what the ingester would have written, including no address anywhere, referrers
already reduced to a host, and device and browser strings from the vocabulary
`Classify` emits.

The one field written directly that a client could not set is the expired
campaign's past `expires_at`. That state is reached by the clock, never by a
request.

`--reset` deletes the demo links in the owning workspace and truncates
`click_events` — **all of them**, since a click row carries no marker saying it
was seeded. Like `seed`, it refuses to run when `APP_ENV=production` without
`--force`. `--seed N` fixes the PRNG so two runs produce the same dataset, and
`--days`, `--volume` change how much history there is. `make demo` runs it with
`--reset` against the development database.
