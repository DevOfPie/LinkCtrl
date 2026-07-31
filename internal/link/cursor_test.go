package link

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
)

func encode(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// The cursor is the one piece of state a client holds between requests, and it
// had no direct test. Both defects it grew — losing the sort it was minted
// under, and carrying no click count for the sort that orders by clicks — are
// invisible until a second page is fetched under a non-default sort.
func TestCursorRoundTrips(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 890123456, time.UTC)
	id := uuid.MustParse("018f3a2b-0000-7000-8000-000000000001")

	for _, sort := range []string{"newest", "oldest", "clicks"} {
		t.Run(sort, func(t *testing.T) {
			got, err := decodeCursor(encodeCursor(sort, at, 4200, id))
			if err != nil {
				t.Fatalf("decoding a cursor we just encoded failed: %v", err)
			}
			if got.Sort != sort {
				t.Errorf("sort = %q, want %q", got.Sort, sort)
			}
			if !got.CreatedAt.Equal(at) {
				t.Errorf("created_at = %s, want %s", got.CreatedAt, at)
			}
			if got.Clicks != 4200 {
				t.Errorf("clicks = %d, want 4200", got.Clicks)
			}
			if got.ID != id {
				t.Errorf("id = %s, want %s", got.ID, id)
			}
		})
	}
}

// Nanosecond precision has to survive, or a cursor lands between rows created
// within the same microsecond and the page either repeats or skips them.
func TestCursorKeepsSubSecondPrecision(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	got, err := decodeCursor(encodeCursor("newest", at, 0, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Nanosecond() != at.Nanosecond() {
		t.Errorf("nanoseconds = %d, want %d", got.CreatedAt.Nanosecond(), at.Nanosecond())
	}
}

// A malformed cursor must be an error, not a zero value: decoding one to
// (zero time, nil uuid) would silently restart pagination from the top, or
// under a `>` predicate return the entire table.
func TestCursorRejectsMalformedInput(t *testing.T) {
	bad := map[string]string{
		"not base64":      "!!!!not-base64!!!!",
		"empty":           "",
		"too few fields":  encode("1|newest|2026-01-01T00:00:00Z"),
		"too many fields": encode("1|newest|2026-01-01T00:00:00Z|0|" + uuid.Nil.String() + "|extra"),
		"unknown version": encode("9|newest|2026-01-01T00:00:00Z|0|" + uuid.Nil.String()),
		"unversioned v0":  encode("2026-01-01T00:00:00Z|" + uuid.Nil.String()),
		"bad timestamp":   encode("1|newest|not-a-time|0|" + uuid.Nil.String()),
		"bad click count": encode("1|newest|2026-01-01T00:00:00Z|many|" + uuid.Nil.String()),
		"bad uuid":        encode("1|newest|2026-01-01T00:00:00Z|0|not-a-uuid"),
	}

	for name, s := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCursor(s); err == nil {
				t.Errorf("accepted a malformed cursor (%s)", name)
			}
		})
	}
}
