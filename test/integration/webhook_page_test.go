//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// The delivery log's limit parameter, at the HTTP surface (M42).
//
// The service clamps a limit of `<= 0` or `> domain.MaxWebhookDeliveryPage` to
// the maximum, but it receives an int32 — so the clamp can only defend against
// what survives the handler's narrowing. ?limit=4294967297 is 2^32+1: truncated
// unchecked it becomes int32(1) and answers one delivery with a 200 and no
// signal that the number was mangled, instead of falling to the clamp the way
// every other out-of-range limit does. The handler must drop out-of-range
// values before narrowing, exactly as the link, audit, dispute and notification
// lists do, so all three malformed spellings below land on the same clamp path
// as no limit at all.
func TestDeliveryLimitOutOfRangeFallsToTheClamp(t *testing.T) {
	f := newAPI(t)
	f.setupOwner()

	// Straight into the tables, the way registerRaw goes: registration's URL
	// policy refuses anything a test can reach, and nothing here is about
	// registration or delivery — only about how the log is read back.
	var wsID uuid.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT id FROM workspaces ORDER BY created_at LIMIT 1`).Scan(&wsID); err != nil {
		t.Fatalf("read the workspace id: %v", err)
	}
	hookID := uuid.Must(uuid.NewV7())
	if _, err := f.pool.Exec(f.t.Context(), `
		INSERT INTO webhooks (id, workspace_id, url, events, description, enabled)
		VALUES ($1, $2, 'https://receiver.example/hook', '{link.created}'::text[], 'limit test', true)`,
		hookID, wsID); err != nil {
		t.Fatalf("insert the webhook row: %v", err)
	}
	// Three rows: enough that a limit truncated to int32(1) answers fewer than
	// the clamp does, and few enough to stay far under the page maximum.
	for range 3 {
		if _, err := f.pool.Exec(f.t.Context(), `
			INSERT INTO webhook_deliveries (id, webhook_id, event, payload, status, attempts, completed_at)
			VALUES ($1, $2, 'link.created', '{}'::jsonb, 'delivered', 1, now())`,
			uuid.Must(uuid.NewV7()), hookID); err != nil {
			t.Fatalf("insert a delivery row: %v", err)
		}
	}

	page := func(query string) int {
		t.Helper()
		resp := f.do(http.MethodGet, "/api/v1/webhooks/"+hookID.String()+"/deliveries"+query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET deliveries%s returned %d, want 200", query, resp.StatusCode)
		}
		var body struct {
			Deliveries []json.RawMessage `json:"deliveries"`
		}
		f.decode(resp, &body)
		return len(body.Deliveries)
	}

	want := page("")
	if want != 3 {
		t.Fatalf("no limit answered %d deliveries, want all 3 under the clamp", want)
	}
	for _, raw := range []string{
		// 2^32+1: survives Atoi on 64-bit, truncates to int32(1) if narrowed
		// unchecked — the one spelling the service clamp cannot catch.
		"4294967297",
		// 2^31: wraps to a negative int32 if narrowed unchecked; the clamp
		// happens to catch a negative, but by accident rather than by design.
		"2147483648",
		// Not a number at all: Atoi fails and the value must be dropped.
		"abc",
	} {
		if got := page("?limit=" + raw); got != want {
			t.Errorf("?limit=%s answered %d deliveries, want %d — an out-of-range "+
				"limit must fall through to the service clamp, not narrow into a "+
				"different page size", raw, got, want)
		}
	}
}
