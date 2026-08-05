//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
	"github.com/DevOfPie/LinkCtrl/internal/notify"
)

type notifyFixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	svc   *notify.Service
	owner *auth.Identity
}

func newNotify(t *testing.T) *notifyFixture {
	t.Helper()
	pool := newDB(t)

	authSvc := auth.NewService(pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour},
	})
	owner, err := authSvc.Register(context.Background(), auth.RegisterInput{
		Email: "owner@example.com", Name: "Owner", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	return &notifyFixture{t: t, pool: pool, svc: notify.NewService(pool), owner: owner}
}

// The milestone's headline constraint: no DDL. The notifications table shipped
// dormant in Phase 1 and everything a kind needs goes in its jsonb, so the
// column set must be exactly what 00600 created.
//
// Asserted rather than assumed, because "we did not add a column" is a claim
// about a schema and only a schema check can hold it — the next milestone that
// wants somewhere to put a field will find this test before it finds a
// migration.
func TestNotificationsTableGainedNoColumns(t *testing.T) {
	pool := newDB(t)

	rows, err := pool.Query(t.Context(), `
		SELECT column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'notifications'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)

	want := []string{
		"body", "created_at", "data", "id", "kind", "read_at", "title",
		"user_id", "workspace_id",
	}
	if len(got) != len(want) {
		t.Fatalf("notifications has columns %v, want exactly %v: structure for a "+
			"dormant table goes in its jsonb until the feature that needs a column arrives",
			got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("notifications has columns %v, want exactly %v", got, want)
		}
	}
}

// An inbox is one person's. Reading is scoped by user rather than filtered
// afterwards, and this is the assertion that would notice if that changed.
// A workspace's notifications stay in that workspace, and the organization's
// follow the reader everywhere.
//
// The column recording which workspace produced a notification was written from
// M40 onward and read by nothing — no query selected it and the domain type had
// no field for it — while two comments in this package stated verbatim that it
// made the notification "appear in that workspace's inbox rather than wherever
// the reader happens to be standing" (F105). It was also F94's stated
// mitigation, so that row was closed believing in a containment that did not
// exist, which is why D102 built the filter rather than deleting the column.
//
// Both halves are asserted together because a bare `workspace_id = @ws` would
// hide every organization-level notification — disputes and audit growth write
// NULL — which is a worse defect than the one it closed.
func TestTheInboxIsScopedToTheWorkspaceExceptWhereItIsNot(t *testing.T) {
	f := newNotify(t)

	// A second workspace in the same organization, and an identity in each.
	second := addWorkspace(t, f.pool, f.owner.OrgID, "Second")
	here := f.owner
	there := *f.owner
	there.WorkspaceID = second

	if err := f.svc.Notify(t.Context(), f.owner.UserID, notify.Event{
		Kind: notify.KindAutomationFired, Title: "Belongs to the first workspace",
		WorkspaceID: &here.WorkspaceID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Notify(t.Context(), f.owner.UserID, notify.Event{
		Kind: notify.KindAuditGrowth, Title: "Belongs to the organization",
	}); err != nil {
		t.Fatal(err)
	}

	titles := func(actor *auth.Identity) []string {
		page, err := f.svc.List(t.Context(), actor, notify.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range page.Items {
			out = append(out, n.Title)
		}
		return out
	}

	got := titles(here)
	if len(got) != 2 {
		t.Errorf("standing in the workspace that produced it, the inbox held %v; want "+
			"both the workspace notification and the organization one", got)
	}

	got = titles(&there)
	if slices.Contains(got, "Belongs to the first workspace") {
		t.Error("a notification belonging to one workspace was shown while its reader " +
			"was standing in another. That is what the column was written for and what " +
			"two comments in this package have claimed since M40")
	}
	if !slices.Contains(got, "Belongs to the organization") {
		t.Errorf("an organization-level notification vanished when the reader moved "+
			"workspace (saw %v). Disputes and audit growth carry no workspace, so a "+
			"predicate without its IS NULL half hides exactly the notifications that "+
			"are not any one workspace's", got)
	}

	// The badge and the preview must agree with the list, or the bell shows a
	// count for rows the page will not show.
	if n, err := f.svc.Unread(t.Context(), &there); err != nil || n != 1 {
		t.Errorf("unread count in the second workspace = %d (err %v), want 1 — the "+
			"organization-level one only", n, err)
	}
	total, preview, err := f.svc.UnreadPreview(t.Context(), &there, notify.PreviewLimit)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(preview) != 1 {
		t.Errorf("preview in the second workspace = %d rows, total %d; want 1 and 1, "+
			"matching the count and the list", len(preview), total)
	}
}

func TestNotificationsAreNotVisibleToOtherUsers(t *testing.T) {
	f := newNotify(t)

	authSvc := auth.NewService(f.pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: time.Hour, Idle: time.Hour},
	})
	other, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "other@example.com", Name: "Other", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := f.svc.Notify(t.Context(), f.owner.UserID, notify.Event{
		Kind: notify.KindAuditGrowth, Title: "For the owner only",
	}); err != nil {
		t.Fatal(err)
	}

	page, err := f.svc.List(t.Context(), other, notify.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Errorf("another user's inbox returned %d notifications", len(page.Items))
	}
	if n, err := f.svc.Unread(t.Context(), other); err != nil || n != 0 {
		t.Errorf("unread for another user = %d (err %v), want 0", n, err)
	}

	// And marking it read from the wrong account changes nothing, rather than
	// erroring in a way that confirms the id exists.
	if err := f.svc.MarkRead(t.Context(), other, page0ID(t, f)); err != nil {
		t.Errorf("MarkRead on somebody else's notification: %v", err)
	}
	if n, _ := f.svc.Unread(t.Context(), f.owner); n != 1 {
		t.Error("another user marked the owner's notification read")
	}
}

func page0ID(t *testing.T, f *notifyFixture) uuid.UUID {
	t.Helper()
	page, err := f.svc.List(t.Context(), f.owner, notify.Filter{})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("expected a notification to exist: %v", err)
	}
	return page.Items[0].ID
}

// read_at is set once. Two tabs, or a double click, must not rewrite when the
// person first saw it.
func TestMarkingReadIsIdempotentAndKeepsTheFirstTimestamp(t *testing.T) {
	f := newNotify(t)
	if err := f.svc.Notify(t.Context(), f.owner.UserID, notify.Event{
		Kind: notify.KindAuditGrowth, Title: "Something happened",
	}); err != nil {
		t.Fatal(err)
	}
	id := page0ID(t, f)

	if err := f.svc.MarkRead(t.Context(), f.owner, id); err != nil {
		t.Fatal(err)
	}
	page, _ := f.svc.List(t.Context(), f.owner, notify.Filter{})
	first := page.Items[0].ReadAt
	if first == nil {
		t.Fatal("read_at was not set")
	}

	time.Sleep(10 * time.Millisecond)
	if err := f.svc.MarkRead(t.Context(), f.owner, id); err != nil {
		t.Errorf("marking an already-read notification errored: %v", err)
	}
	page, _ = f.svc.List(t.Context(), f.owner, notify.Filter{})
	if !page.Items[0].ReadAt.Equal(*first) {
		t.Errorf("read_at moved from %v to %v on a second mark; "+
			"when someone first saw a notification must not be rewritten",
			first, page.Items[0].ReadAt)
	}

	// A notification that does not exist at all is also not an error.
	if err := f.svc.MarkRead(t.Context(), f.owner, uuid.Must(uuid.NewV7())); err != nil {
		t.Errorf("marking an unknown id errored: %v", err)
	}
}

func TestUnreadCountAndMarkAllRead(t *testing.T) {
	f := newNotify(t)
	for i := range 3 {
		if err := f.svc.Notify(t.Context(), f.owner.UserID, notify.Event{
			Kind: notify.KindAuditGrowth, Title: "Notice", Body: "n",
			Data: map[string]any{"i": i},
		}); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := f.svc.Unread(t.Context(), f.owner); err != nil || n != 3 {
		t.Fatalf("unread = %d (err %v), want 3", n, err)
	}

	// unread=true is the filter the badge's list view uses. Asserted with a
	// read notification present, because with everything unread a filter that
	// does nothing at all returns the same answer as one that works.
	if err := f.svc.MarkRead(t.Context(), f.owner, page0ID(t, f)); err != nil {
		t.Fatal(err)
	}
	unread, err := f.svc.List(t.Context(), f.owner, notify.Filter{UnreadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread.Items) != 2 {
		t.Errorf("unread-only list returned %d with one of three read, want 2", len(unread.Items))
	}
	for _, n := range unread.Items {
		if n.ReadAt != nil {
			t.Errorf("a read notification came back from the unread-only filter")
		}
	}
	// The unfiltered list still has all three.
	if everything, _ := f.svc.List(t.Context(), f.owner, notify.Filter{}); len(everything.Items) != 3 {
		t.Errorf("unfiltered list returned %d, want 3", len(everything.Items))
	}

	cleared, err := f.svc.MarkAllRead(t.Context(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 2 {
		t.Errorf("MarkAllRead cleared %d, want 2 — the third was already read, and "+
			"counting it again would report work it did not do", cleared)
	}
	if n, _ := f.svc.Unread(t.Context(), f.owner); n != 0 {
		t.Errorf("unread = %d after marking all read", n)
	}
	// Still listed, just read: an inbox that empties itself has destroyed the
	// record rather than acknowledged it.
	all, _ := f.svc.List(t.Context(), f.owner, notify.Filter{})
	if len(all.Items) != 3 {
		t.Errorf("list returned %d after mark-all-read, want 3 still present", len(all.Items))
	}
}

// D19, at both edges. The threshold defaults to on, so the instance nobody
// configured is the one that gets warned — and the boundary is where an
// off-by-one would either warn constantly or never.
func TestAuditGrowthWarningFiresAtTheThreshold(t *testing.T) {
	tests := []struct {
		name            string
		size, threshold int64
		want            int
	}{
		{"below the threshold", 999, 1000, 0},
		{"exactly at it", 1000, 1000, 1},
		{"above it", 5000, 1000, 1},
		{"threshold of zero is off", 1 << 40, 0, 0},
		{"a negative threshold is off, not always-on", 1 << 40, -1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newNotify(t)
			if err := f.svc.WarnAuditGrowth(t.Context(), tt.size, tt.threshold); err != nil {
				t.Fatalf("WarnAuditGrowth: %v", err)
			}
			page, err := f.svc.List(t.Context(), f.owner, notify.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != tt.want {
				t.Fatalf("size %d against threshold %d produced %d notifications, want %d",
					tt.size, tt.threshold, len(page.Items), tt.want)
			}
			if tt.want > 0 {
				got := page.Items[0]
				if got.Kind != notify.KindAuditGrowth {
					t.Errorf("kind = %q", got.Kind)
				}
				// The numbers must be machine-readable, not only in the prose:
				// a later UI should not have to parse the sentence back out.
				if got.Data["bytes"] == nil || got.Data["threshold"] == nil {
					t.Errorf("data = %v, want bytes and threshold", got.Data)
				}
			}
		})
	}
}

// The threshold stays crossed until an operator acts, so the hourly job would
// otherwise file this every hour forever — and an inbox full of the same line
// is one that stops being read, which costs the warning D5 depends on.
func TestAuditGrowthWarningDoesNotRepeatOnEveryRun(t *testing.T) {
	f := newNotify(t)

	for range 5 {
		if err := f.svc.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
			t.Fatal(err)
		}
	}

	page, err := f.svc.List(t.Context(), f.owner, notify.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("five runs over the threshold produced %d notifications, want 1", len(page.Items))
	}

	// Once the reminder interval has passed it does warn again — the guard is a
	// silence window, not a permanent mute, or an operator who ignored it once
	// would never hear about it again.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE notifications SET created_at = created_at - $1::interval`,
		(notify.AuditGrowthReminderInterval + time.Hour).String()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
		t.Fatal(err)
	}
	page, _ = f.svc.List(t.Context(), f.owner, notify.Filter{})
	if len(page.Items) != 2 {
		t.Errorf("after the reminder interval elapsed there are %d notifications, want 2: "+
			"the guard is a silence window, not a permanent mute", len(page.Items))
	}
}

// Only owners hear about it. An editor cannot act on the retention setting, so
// telling them is noise in the inbox of somebody who cannot help.
func TestAuditGrowthWarningGoesToOwnersOnly(t *testing.T) {
	f := newNotify(t)

	authSvc := auth.NewService(f.pool, auth.ServiceConfig{
		Params: fastParams,
		TTL:    auth.SessionTTL{Absolute: time.Hour, Idle: time.Hour},
	})
	editor, err := authSvc.Register(t.Context(), auth.RegisterInput{
		Email: "editor@example.com", Name: "Editor", Password: "a-sufficiently-long-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Registration makes an owner of a personal organization; demote this one
	// inside the owner's organization so there is a non-owner to check.
	if _, err := f.pool.Exec(t.Context(), `
		UPDATE memberships
		   SET role_id = (SELECT id FROM roles WHERE slug = 'editor'),
		       organization_id = $2
		 WHERE user_id = $1`, editor.UserID, f.owner.OrgID); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.WarnAuditGrowth(t.Context(), 10_000, 1_000); err != nil {
		t.Fatal(err)
	}

	if n, _ := f.svc.Unread(t.Context(), f.owner); n != 1 {
		t.Errorf("the owner has %d notifications, want 1", n)
	}
	if n, _ := f.svc.Unread(t.Context(), editor); n != 0 {
		t.Errorf("an editor was told about the audit log growing; they cannot "+
			"change the retention setting, so it is noise (%d received)", n)
	}
}

// The API surface, over HTTP, including the badge count the nav renders from.
func TestNotificationAPIListsMarksAndCounts(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	var userID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM users WHERE email = 'owner@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	svc := notify.NewService(f.pool)
	for i := range 3 {
		if err := svc.Notify(t.Context(), userID, notify.Event{
			Kind: notify.KindAuditGrowth, Title: "Notice", Data: map[string]any{"i": i},
		}); err != nil {
			t.Fatal(err)
		}
	}

	var count struct {
		Unread int64 `json:"unread"`
	}
	f.decode(f.do(http.MethodGet, "/api/v1/notifications/unread", nil), &count)
	if count.Unread != 3 {
		t.Fatalf("unread = %d, want 3", count.Unread)
	}

	var page struct {
		Items []struct {
			ID     string  `json:"id"`
			Kind   string  `json:"kind"`
			ReadAt *string `json:"read_at"`
		} `json:"items"`
		HasMore bool `json:"has_more"`
	}
	f.decode(f.do(http.MethodGet, "/api/v1/notifications", nil), &page)
	if len(page.Items) != 3 {
		t.Fatalf("listed %d notifications, want 3", len(page.Items))
	}

	resp := f.do(http.MethodPost, "/api/v1/notifications/"+page.Items[0].ID+"/read", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("mark read = %d, want 204", resp.StatusCode)
	}
	f.decode(resp, nil)

	f.decode(f.do(http.MethodGet, "/api/v1/notifications/unread", nil), &count)
	if count.Unread != 2 {
		t.Errorf("unread = %d after marking one read, want 2", count.Unread)
	}

	var cleared struct {
		MarkedRead int64 `json:"marked_read"`
	}
	f.decode(f.do(http.MethodPost, "/api/v1/notifications/read", nil), &cleared)
	if cleared.MarkedRead != 2 {
		t.Errorf("marked_read = %d, want 2", cleared.MarkedRead)
	}
	f.decode(f.do(http.MethodGet, "/api/v1/notifications/unread", nil), &count)
	if count.Unread != 0 {
		t.Errorf("unread = %d after mark-all, want 0", count.Unread)
	}
}

// The badge is drawn from the shell on every dashboard page, so a broken count
// is a broken product rather than a broken page.
func TestNavShowsTheUnreadBadge(t *testing.T) {
	f := newWeb(t)
	f.claim()

	var userID uuid.UUID
	if err := f.pool.QueryRow(t.Context(),
		`SELECT id FROM users WHERE email = 'owner@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := notify.NewService(f.pool).Notify(t.Context(), userID, notify.Event{
		Kind:  notify.KindAuditGrowth,
		Title: "The audit log has passed its size threshold",
	}); err != nil {
		t.Fatal(err)
	}

	body := f.body(f.get("/dashboard", nil))
	if !strings.Contains(body, `aria-label="1 unread"`) {
		t.Error("the dashboard nav shows no unread badge with one unread notification")
	}
	if !strings.Contains(body, `href="/notifications"`) {
		t.Error("the nav has no link to the notifications page")
	}

	page := f.body(f.get("/notifications", nil))
	if !strings.Contains(page, "The audit log has passed its size threshold") {
		t.Error("the notifications page does not render the notification")
	}

	// Marking it read from the page clears the badge, which is the whole point
	// of the page existing.
	f.wantRedirect(f.postForm("/notifications/read", url.Values{}, nil), "/notifications")
	if after := f.body(f.get("/dashboard", nil)); strings.Contains(after, "unread") {
		t.Error("the badge survived mark-all-read")
	}
}
