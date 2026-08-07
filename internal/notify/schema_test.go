package notify

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// M48's schema claim, asserted rather than promised.
//
// The milestone adds a click-through and an undo for the read it performs, and
// its bullet says both cost no DDL: *"**No schema change**, asserted by a test
// that this milestone adds no migration."* `notifications.data` is a jsonb that
// already carries every subject identifier the mapping reads, and `read_at` has
// been nullable since 00600 — so marking one unread is setting a column back to
// NULL, and there is nothing to migrate.

const migrationsDir = "../store/migrations"

// notificationsCreatedIn is the one migration allowed to shape this table.
//
// It is the dormant-structure migration the table shipped in, and the rule the
// whole package follows: structure a kind needs goes in the jsonb until the
// feature that needs a column actually arrives.
const notificationsCreatedIn = "00600_phase2_dormant.sql"

// touchesNotifications matches DDL aimed at this table.
//
// ALTER and the index/constraint forms, because a column is not the only way to
// change a table's shape — and CREATE TABLE, which is how the fixed point below
// is checked rather than assumed.
var touchesNotifications = regexp.MustCompile(
	`(?i)\b(alter|create|drop)\s+(table|index|unique\s+index)\s+(if\s+(not\s+)?exists\s+)?[a-z_]*notifications\b`)

// TestNotificationsNeedNoMigration is the assertion.
//
// **It is written as "no migration shapes this table except the one that created
// it" rather than as "the migration count is N".** A pinned count fails on the
// next milestone that migrates anything at all, which would make it a test about
// the calendar; this one fails exactly when somebody changes the notifications
// table, which is the claim M48 is making. A later phase that genuinely needs a
// column changes this constant deliberately, and the diff says so.
func TestNotificationsNeedNoMigration(t *testing.T) {
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read %s: %v", migrationsDir, err)
	}

	var shaped []string
	var created bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(migrationsDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		if !touchesNotifications.Match(b) {
			continue
		}
		if e.Name() == notificationsCreatedIn {
			created = true
			continue
		}
		shaped = append(shaped, e.Name())
	}

	if !created {
		t.Fatalf("no DDL for `notifications` was found in %s, so this test matched "+
			"nothing and would pass however the table changed. The table is created "+
			"in %s; if it moved, this constant and the pattern above are what have "+
			"to move with it.", notificationsCreatedIn, notificationsCreatedIn)
	}
	if len(shaped) > 0 {
		t.Errorf("these migrations change the `notifications` table besides the one "+
			"that created it: %s\n\nThe inbox's structure lives in its jsonb `data` "+
			"column until a feature genuinely needs a column, which is the rule every "+
			"dormant table in this schema follows — and M48's click-through and "+
			"mark-unread were both built on the promise that they needed neither.",
			strings.Join(shaped, ", "))
	}
}

// TestReadAtIsNullable is the premise mark-unread rests on, read out of the
// schema rather than trusted.
//
// Marking a notification unread is `read_at = NULL`. If the column were ever
// made NOT NULL — with a sentinel timestamp for unread, say — MarkUnread would
// start failing at the database and this package's whole "no schema change"
// argument would be wrong in a way no Go test would notice.
func TestReadAtIsNullable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(migrationsDir, notificationsCreatedIn))
	if err != nil {
		t.Fatalf("read %s: %v", notificationsCreatedIn, err)
	}

	_, after, ok := strings.Cut(string(b), "CREATE TABLE notifications")
	if !ok {
		t.Fatalf("%s does not create `notifications`", notificationsCreatedIn)
	}
	body, _, ok := strings.Cut(after, ");")
	if !ok {
		t.Fatalf("the `notifications` table in %s is never closed", notificationsCreatedIn)
	}

	var line string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "read_at") {
			line = strings.TrimSpace(l)
			break
		}
	}
	switch {
	case line == "":
		t.Fatal("`notifications` has no read_at column, and unread is what its " +
			"absence means")
	case strings.Contains(strings.ToUpper(line), "NOT NULL"):
		t.Errorf("read_at is NOT NULL: %s\n\nUnread is NULL in this schema — the "+
			"partial index the badge is served by has `WHERE read_at IS NULL` in its "+
			"predicate — so a non-nullable column would break the count as well as "+
			"the undo.", line)
	}
}
