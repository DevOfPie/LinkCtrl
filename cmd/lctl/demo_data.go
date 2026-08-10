package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/link"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pgxExecutor is the one method the reset statements need, so a helper can be
// handed the transaction rather than the pool and cannot commit it.
type pgxExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// demoReset removes the previous demo dataset.
//
// Scoped to this workspace, to the catalogue's own aliases and to the demo
// organization, so it cannot delete links somebody added by hand. Click events
// are truncated wholesale because they carry no marker distinguishing demo rows
// from real ones — which is why the command refuses to run against production
// without --force.
//
// Every row the Phase 2 seeding writes is removed here too, in demoResetPhase2.
// That is what makes `make demo-update` idempotent: run twice, the same demo.
func demoReset(ctx context.Context, pool *pgxpool.Pool, actor *auth.Identity, cat []demoLink) error {
	// The second workspace's aliases are reset here too, and not only by the
	// workspace delete at the end of demoResetPhase2 that cascades them (F168).
	//
	// That delete reaches a link only if the link is *inside* the workspace, and
	// on 2026-08-07 four of these five were found in the demo's **Default**
	// workspace instead — `spring-webinar`, `roadshow`, `partner-portal` and
	// `cw-survey` — while the second workspace held none. Aliases are unique per
	// **domain** and both workspaces are on the instance default, so the stranded
	// copies refused the next run's creation of the same names and no re-run could
	// clear them: `make demo-update` failed identically forever. The reset is what
	// makes this command idempotent, and an idempotency that depends on the last
	// run having put every row in the right place is not one.
	//
	// Matching by alias across the organization costs nothing when the rows are
	// where they belong — the workspace delete gets there first — and is the whole
	// recovery when they are not.
	aliases := make([]string, 0, len(cat))
	for _, d := range cat {
		if d.alias != "" {
			aliases = append(aliases, d.alias)
		}
	}
	for _, d := range demoWorkspace2Catalogue() {
		if d.alias != "" {
			aliases = append(aliases, d.alias)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// **Every statement here is scoped to the organization**, and that uniformity
	// is the point rather than a widening.
	//
	// These three used to be scoped to `actor.WorkspaceID` while the rollups
	// below, the click events and everything in demoResetPhase2 were scoped to
	// the organization. An actor resolving into any *other* workspace of the same
	// organization therefore committed a half-reset — the links left standing,
	// their analytics wiped — and reported success. M36 removed the reachable
	// path to that state; it did not remove the asymmetry, which is what makes it
	// a defect waiting for the demo to gain a third workspace (F68).
	//
	// The organization is the right scope on its own terms: the demo has two
	// workspaces already, the catalogue is seeded across them, and every other
	// statement in this function had already reached that conclusion.
	//
	// Generated-alias links have no stable name to match on, so they are found
	// by their destination instead.
	const del = `
		DELETE FROM links
		 WHERE workspace_id IN (SELECT id FROM workspaces WHERE organization_id = $1)
		   AND (alias = ANY($2::text[])
		        OR primary_url IN ('https://example.com/whitepaper/short-links.pdf',
		                           'https://example.com/webinars/replay'))`
	if _, err := tx.Exec(ctx, `DELETE FROM link_tags WHERE link_id IN (
			SELECT id FROM links
			 WHERE workspace_id IN (SELECT id FROM workspaces WHERE organization_id = $1)
			   AND alias = ANY($2::text[]))`,
		actor.OrgID, aliases); err != nil {
		return fmt.Errorf("reset tags: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM destinations WHERE link_id IN (
			SELECT id FROM links
			 WHERE workspace_id IN (SELECT id FROM workspaces WHERE organization_id = $1)
			   AND alias = ANY($2::text[]))`,
		actor.OrgID, aliases); err != nil {
		return fmt.Errorf("reset destinations: %w", err)
	}
	if _, err := tx.Exec(ctx, del, actor.OrgID, aliases); err != nil {
		return fmt.Errorf("reset links: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM reserved_aliases WHERE alias = ANY($1::text[])`,
		aliases); err != nil {
		return fmt.Errorf("reset reservations: %w", err)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE click_events`); err != nil {
		return fmt.Errorf("reset clicks: %w", err)
	}
	// Scoped to the organization rather than to one workspace, because the demo
	// now has two and the rollup tables carry no foreign key that would take the
	// second one's rows with it.
	for _, t := range []string{"link_click_daily", "link_dimension_daily", "workspace_click_daily"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE workspace_id IN (
				SELECT id FROM workspaces WHERE organization_id = $1)`, actor.OrgID); err != nil {
			return fmt.Errorf("reset %s: %w", t, err)
		}
	}
	if err := demoResetPhase2(ctx, tx, actor.OrgID, actor.UserID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// demoCreateLinks creates the catalogue through the service, then applies the
// states a client cannot ask for directly.
func demoCreateLinks(ctx context.Context, pool *pgxpool.Pool, svc *link.Service,
	actor *auth.Identity, cat []demoLink, now time.Time,
) (map[int]uuid.UUID, error) {
	ids := make(map[int]uuid.UUID, len(cat))

	for i, d := range cat {
		in := link.CreateInput{
			URL: d.url, Alias: d.alias, Title: d.title,
			Description: d.desc, Tags: d.tags, ForwardQuery: d.forwardQuery,
			ForwardPath: d.forwardPath,
		}
		// The webinar expires while the demo is still current, which is the
		// state the form describes; the sale expires in the past, which no API
		// accepts and only the clock produces.
		if d.alias == "webinar" {
			exp := now.AddDate(0, 0, 21)
			in.ExpiresAt = &exp
		}
		created, err := svc.Create(ctx, actor, in)
		if err != nil {
			return nil, fmt.Errorf("create %q: %w", d.alias, err)
		}
		ids[i] = created.ID

		// Backdate creation so the list is not twenty rows from the same second.
		if _, err := pool.Exec(ctx,
			`UPDATE links SET created_at = $2, updated_at = $2 WHERE id = $1`,
			created.ID, now.AddDate(0, 0, -d.age)); err != nil {
			return nil, fmt.Errorf("backdate %q: %w", created.Alias, err)
		}

		switch d.state {
		case "archived":
			if _, err := svc.Archive(ctx, actor, created.ID); err != nil {
				return nil, fmt.Errorf("archive %q: %w", created.Alias, err)
			}
		case "trashed":
			if err := svc.Delete(ctx, actor, created.ID); err != nil {
				return nil, fmt.Errorf("delete %q: %w", created.Alias, err)
			}
		case "expired":
			// Past expiry is reached by the clock, never by a request. Written
			// directly for that reason, and it is the only field here that is.
			if _, err := pool.Exec(ctx,
				`UPDATE links SET expires_at = $2 WHERE id = $1`,
				created.ID, now.AddDate(0, 0, -4)); err != nil {
				return nil, fmt.Errorf("expire %q: %w", created.Alias, err)
			}
		}
	}
	return ids, nil
}

// Distributions. Values are the exact strings analytics.Classify emits, so a
// backfilled row is indistinguishable from an ingested one to every query.
var (
	demoCountries = []string{
		"US", "US", "US", "US", "US", "US", "US", "US", "US", "US", "US", "US", "US",
		"US", "US", "US", "US", "US", "US", "US", "US", "US", "US", "US", "US", "US",
		"GB", "GB", "GB", "GB", "GB", "GB", "GB", "GB", "GB", "GB", "GB", "GB", "GB",
		"DE", "DE", "DE", "DE", "DE", "DE", "DE", "DE", "DE",
		"IN", "IN", "IN", "IN", "IN", "IN", "IN", "IN",
		"FR", "FR", "FR", "FR", "FR", "FR",
		"CA", "CA", "CA", "CA", "CA", "CA",
		"AU", "AU", "AU", "AU", "NL", "NL", "NL", "NL", "BR", "BR", "BR", "BR",
		"JP", "JP", "JP", "ES", "ES", "ES", "IT", "IT", "IT", "SE", "SE", "PL", "PL",
		"MX", "MX", "SG", "IE", "CH", "NO", "ZA",
	}
	demoReferrers = []string{
		"", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "",
		"", "", "", "", "", "", "", "", "",
		"www.google.com", "www.google.com", "www.google.com", "www.google.com",
		"www.google.com", "www.google.com", "www.google.com", "www.google.com",
		"www.google.com", "www.google.com", "www.google.com", "www.google.com",
		"www.google.com", "www.google.com", "www.google.com", "www.google.com",
		"www.google.com",
		"www.linkedin.com", "www.linkedin.com", "www.linkedin.com", "www.linkedin.com",
		"www.linkedin.com", "www.linkedin.com", "www.linkedin.com", "www.linkedin.com",
		"www.linkedin.com",
		"t.co", "t.co", "t.co", "t.co", "t.co", "t.co", "t.co",
		"news.ycombinator.com", "news.ycombinator.com", "news.ycombinator.com",
		"news.ycombinator.com", "news.ycombinator.com",
		"www.reddit.com", "www.reddit.com", "www.reddit.com", "www.reddit.com",
		"www.reddit.com",
		"github.com", "github.com", "github.com", "github.com",
		"mail.google.com", "mail.google.com", "mail.google.com", "mail.google.com",
		"slack-redir.net", "slack-redir.net", "slack-redir.net", "slack-redir.net",
		"app.slack.com", "app.slack.com", "app.slack.com",
		"www.bing.com", "www.bing.com", "www.bing.com",
		"medium.com", "medium.com", "medium.com",
		"duckduckgo.com", "duckduckgo.com",
		"teams.microsoft.com", "teams.microsoft.com",
		"dev.to", "dev.to", "twitter.com", "twitter.com",
	}
	// Business-hours weighted.
	demoHours = []int{
		0, 1, 2, 3, 4, 5, 6, 7, 7, 8, 8, 8, 9, 9, 9, 9, 10, 10, 10, 10, 11, 11, 11, 11,
		12, 12, 12, 13, 13, 13, 13, 14, 14, 14, 14, 15, 15, 15, 15, 16, 16, 16, 16,
		17, 17, 17, 18, 18, 18, 19, 19, 20, 20, 21, 21, 22, 23,
	}
)

func demoLanguage(country string) string {
	switch country {
	case "DE", "CH":
		return "de"
	case "FR":
		return "fr"
	case "NL":
		return "nl"
	case "BR":
		return "pt"
	case "JP":
		return "ja"
	case "ES", "MX":
		return "es"
	case "IT":
		return "it"
	case "SE":
		return "sv"
	case "PL":
		return "pl"
	case "NO":
		return "nb"
	default:
		return "en"
	}
}

// demoAgent derives device, browser and OS from a visitor index, so one visitor
// keeps the same device all day. Resampling per click would make the device
// split perfectly stable and perfectly meaningless.
func demoAgent(vidx int) (device, browser, os string) {
	d, b, o := vidx%100, (vidx*7)%100, (vidx*13)%100
	switch {
	case d < 54:
		device = "desktop"
		switch {
		case b < 52:
			browser = "Chrome"
		case b < 68:
			browser = "Edge"
		case b < 82:
			browser = "Firefox"
		case b < 94:
			browser = "Safari"
		case b < 97:
			browser = "Opera"
		default:
			browser = "Brave"
		}
		switch {
		case o < 55:
			os = "Windows"
		case o < 85:
			os = "macOS"
		case o < 97:
			os = "Linux"
		default:
			os = "ChromeOS"
		}
	case d < 93:
		device = "mobile"
		switch {
		case b < 45:
			browser = "Chrome"
		case b < 85:
			browser = "Safari"
		case b < 93:
			browser = "Samsung Internet"
		case b < 97:
			browser = "Firefox"
		default:
			browser = "Edge"
		}
		if o < 52 {
			os = "Android"
		} else {
			os = "iOS"
		}
	default:
		device = "tablet"
		switch {
		case b < 55:
			browser = "Safari"
		case b < 95:
			browser = "Chrome"
		default:
			browser = "Firefox"
		}
		if o < 60 {
			os = "iOS"
		} else {
			os = "Android"
		}
	}
	return device, browser, os
}

// demoClickRow is one backfilled click, before it reaches the database.
type demoClickRow struct {
	linkID         uuid.UUID
	at             time.Time
	hash           []byte
	country        string
	device         string
	browser        string
	os             string
	lang, referrer string
	isBot          bool
	latency        int32
}

// demoCountedClicks generates exactly n human clicks on one link.
//
// The weighted generator below cannot be asked for a number. It draws from a
// PRNG, discards whatever lands after the cutoff and adds crawler traffic on
// top, which is right for a catalogue whose totals only have to look like a
// month of use — and wrong for a link whose click total is being *compared*
// against something. `first-fifty` is that link: its budget says twelve of fifty
// are spent, and a redirect that spends a click records one, so the two numbers
// have to be the same number (F166).
//
// No PRNG for the same reason: nothing here may move between two runs of
// `lctl demo --reset` on the same day. Every field is derived from the click's
// index, and `ago` never reaches zero, so every timestamp is in a day that is
// already over and none is ever discarded — n rows in, n rows out.
//
// No bot clicks. All three callers are gated links, and a crawler that met the
// password form, the signature refusal or a spent budget did not reach the
// destination; counting it as a click on the link would be the same kind of
// number-that-cannot-be-true this exists to remove.
func demoCountedClicks(linkID uuid.UUID, n, oldest int, now time.Time) []demoClickRow {
	if n <= 0 || oldest <= 0 {
		return nil
	}
	midnight := now.Truncate(24 * time.Hour)
	rows := make([]demoClickRow, 0, n)
	for i := range n {
		// Oldest first, thinning towards today, and never today itself.
		ago := max(1, oldest-(i*oldest)/n)
		day := midnight.AddDate(0, 0, -ago)
		vidx := (i*37 + 11) % 900
		country := demoCountries[(i*13)%len(demoCountries)]
		device, browser, osName := demoAgent(vidx)
		rows = append(rows, demoClickRow{
			linkID: linkID,
			at: day.
				Add(time.Duration(demoHours[(i*7)%len(demoHours)]) * time.Hour).
				Add(time.Duration((i*17)%60) * time.Minute).
				Add(time.Duration((i*29)%60) * time.Second),
			hash:     demoVisitorHash(day, vidx, false),
			country:  country,
			device:   device,
			browser:  browser,
			os:       osName,
			lang:     demoLanguage(country),
			referrer: demoReferrers[(i*23)%len(demoReferrers)],
			latency:  int32(200 + (i*211)%2600), //nolint:gosec // bounded
		})
	}
	return rows
}

// demoClickRows generates the click history.
//
// Pure, and separated from the COPY below for that reason: the history is the
// one part of the demo that two runs can disagree about, so the property
// `lctl demo --reset` has to have — run it again, get the same demo — is
// asserted directly against this function rather than by seeding twice and
// comparing row counts.
func demoClickRows(cat []demoLink, ids map[int]uuid.UUID, opt demoOptions, now time.Time) []demoClickRow {
	// Deterministic on purpose: two runs with the same --seed produce the same
	// dataset, so a screenshot and a bug report describe the same instance.
	rng := rand.New(rand.NewPCG(opt.prng, 0x9E3779B97F4A7C15)) //nolint:gosec // G404: demo data, determinism wanted

	var rows []demoClickRow

	// Today is only partly over, so some of the clicks generated for it land in
	// the future and are not written. The cutoff for that is the top of the
	// current hour rather than the instant itself, so that every run within the
	// same hour drops exactly the same clicks and generates exactly the same
	// history — which is what makes two runs minutes apart produce the same demo
	// (F71, F74). The cost is that the newest hour of traffic is missing, out of
	// thirty days of it.
	cutoff := now.Truncate(time.Hour)
	midnight := now.Truncate(24 * time.Hour)
	for i, d := range cat {
		if d.weight == 0 {
			continue
		}
		linkID, ok := ids[i]
		if !ok {
			continue
		}
		for ago := opt.days - 1; ago >= 0; ago-- {
			if ago > d.from || ago < d.to {
				continue
			}
			day := midnight.AddDate(0, 0, -ago)

			factor := opt.volume
			if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
				factor *= 0.55
			}
			// Gentle growth towards today.
			factor *= 0.75 + 0.25*float64(opt.days-1-ago)/float64(opt.days)
			factor *= 0.80 + rng.Float64()*0.40
			if d.spikeMult > 0 && ago == d.spikeDay {
				factor *= d.spikeMult
			}
			n := int(float64(d.weight)*factor + 0.5)

			for range n {
				// A visitor is the same person all day and a different hash
				// tomorrow, because the real salt rotates daily.
				vidx := rng.IntN(900)
				at := day.
					Add(time.Duration(demoHours[rng.IntN(len(demoHours))]) * time.Hour).
					Add(time.Duration(rng.IntN(60)) * time.Minute).
					Add(time.Duration(rng.IntN(60)) * time.Second)
				// Every draw for this click is made before the click can be
				// discarded, so a discarded one costs the same draws as a kept
				// one. Skipping them shifted the shared PRNG stream, and every
				// link and day generated afterwards then differed — which is
				// how dropping a single click changed the total by hundreds in
				// either direction (F74).
				country := demoCountries[rng.IntN(len(demoCountries))]
				referrer := demoReferrers[rng.IntN(len(demoReferrers))]
				latency := int32(200 + rng.IntN(2600)) //nolint:gosec // bounded
				if at.After(cutoff) {
					continue
				}
				device, browser, os := demoAgent(vidx)
				rows = append(rows, demoClickRow{
					linkID: linkID, at: at,
					hash:    demoVisitorHash(day, vidx, false),
					country: country, device: device, browser: browser, os: os,
					lang:     demoLanguage(country),
					referrer: referrer,
					latency:  latency,
				})
			}

			// Automated traffic: unfurlers, uptime checks, crawlers. Recorded,
			// and excluded from every dimension rollup, which is the whole
			// reason the flag is on the row.
			for range max(1, int(float64(n)*0.085+0.5)) {
				vidx := rng.IntN(40)
				at := day.
					Add(time.Duration(rng.IntN(24)) * time.Hour).
					Add(time.Duration(rng.IntN(60)) * time.Minute).
					Add(time.Duration(rng.IntN(60)) * time.Second)
				// Drawn before the discard, for the reason above.
				browser, os := "Other", "Other"
				if rng.IntN(10) >= 8 {
					browser, os = "Chrome", "Linux"
				}
				country := demoCountries[rng.IntN(len(demoCountries))]
				latency := int32(200 + rng.IntN(1800)) //nolint:gosec // bounded
				if at.After(cutoff) {
					continue
				}
				rows = append(rows, demoClickRow{
					linkID: linkID, at: at,
					hash:    demoVisitorHash(day, vidx, true),
					country: country,
					device:  "bot", browser: browser, os: os,
					// Crawlers send neither.
					lang: "", referrer: "",
					isBot: true, latency: latency,
				})
			}
		}
	}
	return rows
}

// demoClicks writes the click history with COPY and returns how many rows it
// wrote.
func demoClicks(ctx context.Context, pool *pgxpool.Pool, workspaceID string,
	cat []demoLink, ids map[int]uuid.UUID, opt demoOptions, now time.Time,
) (int64, error) {
	wsID, err := uuid.Parse(workspaceID)
	if err != nil {
		return 0, err
	}
	return demoCopyClicks(ctx, pool, wsID, demoClickRows(cat, ids, opt, now))
}

// demoCopyClicks writes a batch of click events and refreshes the counters.
//
// Separate from demoClicks so that the Phase 2 seeding can write traffic for
// links the weighted catalogue above does not describe — the gated links and the
// one on the custom hostname, which were created through the service and then
// never touched again (F165, F166). Same COPY, same columns, same counter
// refresh, because a click event those pages disagreed with the catalogue's
// would be a second shape of demo click for a reader to notice.
func demoCopyClicks(ctx context.Context, pool *pgxpool.Pool, wsID uuid.UUID,
	rows []demoClickRow,
) (int64, error) {
	copied, err := pool.CopyFrom(ctx,
		pgx.Identifier{"click_events"},
		[]string{
			"id", "link_id", "workspace_id", "occurred_at", "visitor_hash",
			"is_first_visit", "country", "region", "city", "device", "browser",
			"os", "language", "referrer_host", "is_bot", "latency_us",
		},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			r := rows[i]
			return []any{
				uuid.Must(uuid.NewV7()), r.linkID, wsID, r.at, r.hash,
				// The ingester writes false too; nothing computes this.
				false,
				r.country, nil, nil, r.device, r.browser, r.os,
				r.lang, r.referrer, r.isBot, r.latency,
			}, nil
		}))
	if err != nil {
		return 0, fmt.Errorf("copy click events: %w", err)
	}

	// The denormalized counter, which the ingester maintains in the same
	// transaction as the events. Bots count here: it answers "how many times was
	// this fetched", and the charts are where humans get separated out.
	if _, err := pool.Exec(ctx, `
		UPDATE links l
		   SET click_count = c.n, last_click_at = c.last
		  FROM (SELECT link_id, count(*) AS n, max(occurred_at) AS last
		          FROM click_events GROUP BY link_id) c
		 WHERE c.link_id = l.id`); err != nil {
		return 0, fmt.Errorf("update click counts: %w", err)
	}
	return copied, nil
}

// demoVisitorHash produces a 16-byte value shaped like the real one.
//
// The real hash is HMAC(salt of the day, ip || user-agent || workspace), so the
// same person is a different hash tomorrow. Keying on (day, visitor) reproduces
// that, and no analytics_salts rows are needed: the salts these would have used
// are long past their purge date, which is the point of them.
func demoVisitorHash(day time.Time, vidx int, bot bool) []byte {
	seed := day.Format("2006-01-02") + ":" + fmt.Sprint(vidx)
	if bot {
		seed = day.Format("2006-01-02") + ":bot:" + fmt.Sprint(vidx)
	}
	sum := sha256.Sum256([]byte(seed))
	// Truncated to 16 bytes, the same as analytics.VisitorHashLength, so a
	// backfilled hash is the same shape as an ingested one.
	return sum[:16]
}
