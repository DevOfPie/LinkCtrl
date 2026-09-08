//go:build integration

package integration

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

// notificationQueries counts the queries a request sends to the notifications
// table, whatever issues them.
//
// Matching on the table name rather than on a query's generated constant is
// deliberate: the thing being held is "one round trip about notifications per
// render", and a second lookup written a different way would be exactly the
// regression a constant-name match would miss.
type notificationQueries struct {
	mu sync.Mutex
	n  int
}

func (c *notificationQueries) TraceQueryStart(
	ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData,
) context.Context {
	if strings.Contains(data.SQL, "notifications") {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return ctx
}

func (c *notificationQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *notificationQueries) reset() {
	c.mu.Lock()
	c.n = 0
	c.mu.Unlock()
}

func (c *notificationQueries) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestTheBellCostsNoExtraQuery is the constraint that shaped this feature.
//
// The header ran one notification query per page render before the bell existed
// — a bare unread count — and the obvious way to add a preview is a second
// query beside it, on every page of the dashboard, for a decoration most people
// never open. So the count and the rows come back from one call, and this is
// what says so: it counts what reaches Postgres, from a page render, with the
// preview visibly rendered in the response.
//
// Both halves matter. Counting alone would pass on a bell that queries nothing
// and shows nothing, and rendering alone would pass on a bell that costs two.
func TestTheBellCostsNoExtraQuery(t *testing.T) {
	counter := &notificationQueries{}
	pool, _ := newTracedDB(t, counter)
	f := newWebOn(t, pool)
	f.claim()

	var userID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM users WHERE email = 'owner@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	svc := notify.NewService(f.pool)
	for _, title := range []string{"First notice", "Second notice", "Third notice"} {
		if err := svc.Notify(t.Context(), userID, notify.Event{
			Kind: notify.KindAuditGrowth, Title: title, Body: "Something happened.",
		}); err != nil {
			t.Fatal(err)
		}
	}

	counter.reset()
	body := f.body(f.get("/dashboard", nil))

	if got := counter.count(); got != 1 {
		t.Errorf("rendering /dashboard issued %d notification queries, want 1; the "+
			"shell's unread lookup has to be extended to carry the preview rather "+
			"than joined by a second query, because this runs on every page of the "+
			"dashboard", got)
	}
	if !strings.Contains(body, `aria-label="3 unread"`) {
		t.Error("the badge does not show the unread count")
	}
	for _, want := range []string{"First notice", "Third notice"} {
		if !strings.Contains(body, want) {
			t.Errorf("the bell's preview does not contain %q; a query count over a "+
				"bell that renders nothing proves nothing", want)
		}
	}

	// The count stays exact past the preview's limit — the badge is a count, not
	// the length of the list beneath it.
	for i := range notify.PreviewLimit + 3 {
		if err := svc.Notify(t.Context(), userID, notify.Event{
			Kind: notify.KindAuditGrowth, Title: "Later notice", Data: map[string]any{"i": i},
		}); err != nil {
			t.Fatal(err)
		}
	}
	counter.reset()
	body = f.body(f.get("/links", nil))

	if got := counter.count(); got != 1 {
		t.Errorf("rendering /links issued %d notification queries, want 1", got)
	}
	if !strings.Contains(body, `aria-label="11 unread"`) {
		t.Error("the badge is not the exact unread count once there are more " +
			"notifications than the preview shows")
	}
	if got := strings.Count(body, "Later notice"); got != notify.PreviewLimit {
		t.Errorf("the preview shows %d of the newest notifications, want %d; it is "+
			"bounded so that it cannot become a worse copy of /notifications",
			got, notify.PreviewLimit)
	}
}

// TestTheHeaderMovedAccountAndNotificationsOutOfTheNav is F6 and F7 over HTTP:
// the same page a person loads, not a template rendered with a fixture.
func TestTheHeaderMovedAccountAndNotificationsOutOfTheNav(t *testing.T) {
	f := newWeb(t)
	f.claim()

	body := f.body(f.get("/dashboard", nil))

	nav, _, found := strings.Cut(body, `<div class="ml-auto`)
	if !found {
		t.Fatal("the served header has no right-hand group")
	}
	for _, gone := range []string{`href="/account"`, `href="/notifications"`} {
		if strings.Contains(nav, gone) {
			t.Errorf("the top-level nav still carries %s", gone)
		}
	}

	// Both destinations are still one click away, from the menus.
	for _, want := range []string{
		`href="/account"`,                       // in the identity menu
		`<form method="post" action="/logout">`, // sign out, still a POST
		`href="/notifications"`,                 // View all, in the bell
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the served header is missing %s", want)
		}
	}

	// And they still work when followed.
	account := f.get("/account", nil)
	defer func() { _ = account.Body.Close() }()
	if account.StatusCode != 200 {
		t.Errorf("/account returned %d from the identity menu's link", account.StatusCode)
	}
	notifications := f.get("/notifications", nil)
	defer func() { _ = notifications.Body.Close() }()
	if notifications.StatusCode != 200 {
		t.Errorf("/notifications returned %d from the bell's View all", notifications.StatusCode)
	}
}
