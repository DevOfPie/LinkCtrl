package audit

import (
	"encoding/base64"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/auth"
)

// The cursor is the pagination contract. A cursor that decodes to a different
// position than it encoded silently skips or repeats records, which on an audit
// log means a reader concludes something did not happen.
func TestCursorRoundTrips(t *testing.T) {
	at := time.Date(2026, 7, 31, 14, 22, 9, 123456789, time.UTC)
	id := uuid.Must(uuid.NewV7())

	got, err := decodeCursor(encodeCursor(at, id))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OccurredAt.Equal(at) {
		t.Errorf("occurred_at = %v, want %v", got.OccurredAt, at)
	}
	if got.ID != id {
		t.Errorf("id = %v, want %v", got.ID, id)
	}
}

// Nanosecond precision has to survive the round trip. audit_logs is written by
// a burst of events inside one request, and truncating to a second makes the
// timestamp tiebreak useless — the id comparison would then be carrying the
// whole ordering, against rows the query already filtered out.
func TestCursorKeepsSubSecondPrecision(t *testing.T) {
	at := time.Date(2026, 7, 31, 14, 22, 9, 987654321, time.UTC)
	got, err := decodeCursor(encodeCursor(at, uuid.Must(uuid.NewV7())))
	if err != nil {
		t.Fatal(err)
	}
	if got.OccurredAt.Nanosecond() != at.Nanosecond() {
		t.Errorf("nanoseconds = %d, want %d", got.OccurredAt.Nanosecond(), at.Nanosecond())
	}
}

func TestMalformedCursorsAreRefused(t *testing.T) {
	id := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	for name, raw := range map[string]string{
		"not base64":        "!!!!",
		"empty":             encodeRaw(""),
		"wrong version":     encodeRaw("2|" + now + "|" + id),
		"too few fields":    encodeRaw("1|" + now),
		"too many fields":   encodeRaw("1|" + now + "|" + id + "|extra"),
		"unparseable time":  encodeRaw("1|yesterday|" + id),
		"unparseable uuid":  encodeRaw("1|" + now + "|not-a-uuid"),
		"link cursor shape": encodeRaw("1|newest|" + now + "|3|" + id),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCursor(raw); err == nil {
				t.Errorf("decodeCursor(%q) succeeded; a cursor that is not a "+
					"position this build minted must be refused, not reinterpreted", raw)
			}
		})
	}
}

// actor_label is a snapshot, and its whole job is to stay readable when the
// account is gone. It must never come out empty: a record naming nobody is one
// an operator cannot act on.
func TestActorLabelAlwaysNamesSomething(t *testing.T) {
	tests := []struct {
		name  string
		actor *auth.Identity
		want  string
	}{
		{"email preferred", &auth.Identity{Email: "ana@example.com", Name: "Ana"}, "ana@example.com"},
		{"falls back to name", &auth.Identity{Name: "Ana"}, "Ana"},
		{"no actor at all is the system", nil, "system"},
		{"an identity with neither", &auth.Identity{UserID: uuid.Must(uuid.NewV7())}, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := actorLabel(tt.actor); got != tt.want {
				t.Errorf("actorLabel = %q, want %q", got, tt.want)
			}
		})
	}
}

// The privacy line, at the level Record reduces it. The integration test asserts
// what reaches the column; this asserts the function that decides it, including
// the case that has caught this project before — an IPv4-mapped IPv6 address
// masked as though it were IPv6 keeps the entire embedded v4 address.
func TestOnlyAPrefixIsEverDerived(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"203.0.113.42", "203.0.113.0/24"},
		{"2001:db8:1234:5678::1", "2001:db8:1234::/48"},
		{"::ffff:203.0.113.42", "203.0.113.0/24"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := auth.AnonymizeIP(netip.MustParseAddr(tt.in))
			if got != tt.want {
				t.Errorf("AnonymizeIP(%s) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	// No address in context is no prefix, not a misleading one. The CLI and the
	// job runner both write in that state.
	if got := auth.AnonymizeIP(auth.ClientIPFrom(t.Context())); got != "" {
		t.Errorf("prefix from an empty context = %q, want empty", got)
	}
}

// The context carrier is what lets a service write an audit event without being
// handed the request. If it stops round-tripping, every event silently loses its
// network and nothing else fails.
func TestClientIPSurvivesTheContext(t *testing.T) {
	addr := netip.MustParseAddr("198.51.100.7")
	ctx := auth.WithClientIP(t.Context(), addr)

	if got := auth.ClientIPFrom(ctx); got != addr {
		t.Errorf("ClientIPFrom = %v, want %v", got, addr)
	}
	if got := auth.AnonymizeIP(auth.ClientIPFrom(ctx)); got != "198.51.100.0/24" {
		t.Errorf("prefix = %q, want 198.51.100.0/24", got)
	}
}

// encodeRaw wraps a cursor body in the same encoding decodeCursor expects, so a
// test can hand it a well-encoded cursor with the wrong contents — which is the
// case that matters. A malformed base64 string is caught by the decoder before
// any of the shape checks run.
func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
