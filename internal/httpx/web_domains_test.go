package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/ui"
)

// The domains page's grace-window line, in both tenses (F161).
//
// **The state under test is at most an hour wide and cannot be waited for.** The
// hourly re-verification pass is the only thing that stops serving a hostname, so
// between D70's window expiring and that pass arriving the row is *still served*
// — `verified_at` is set, the badge correctly reads "Verified — links are served
// here" — while the deadline beside it is in the past. The page had one tense and
// wrote the past one as a promise: "stop being served at <a time that has been>",
// directly under a badge saying they are served. That is the page disagreeing
// with itself about the hostname somebody opened it to diagnose.

func TestTheGraceDeadlineKnowsWhetherItHasPassed(t *testing.T) {
	deadline := time.Date(2026, 8, 9, 14, 30, 0, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		now        time.Time
		wantPassed bool
	}{
		{"an hour before", deadline.Add(-time.Hour), false},
		{"a second before", deadline.Add(-time.Second), false},
		// The instant itself counts as passed: at exactly the deadline the window
		// is spent, and "stops being served at 14:30" read at 14:30 is a sentence
		// about nothing.
		{"the instant itself", deadline, true},
		{"a second after", deadline.Add(time.Second), true},
		{"most of an hour after, before the pass arrives", deadline.Add(59 * time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label, passed := domainStopsAt(deadline, tc.now)
			if passed != tc.wantPassed {
				t.Errorf("passed = %v at %s against a deadline of %s, want %v",
					passed, tc.now.UTC(), deadline, tc.wantPassed)
			}
			if want := "9 Aug 2026 14:30 UTC"; label != want {
				t.Errorf("label = %q, want %q; the time a row shows must not move "+
					"with the tense it is written in", label, want)
			}
		})
	}
}

// TestTheDomainRowNeverPromisesAStopThatHasPassed renders the panel, because the
// tense lives in the template and a test of the boolean alone would pass on a
// build where the template ignores it.
func TestTheDomainRowNeverPromisesAStopThatHasPassed(t *testing.T) {
	r, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New: %v", err)
	}

	// Both rows are Verified, which is the point: the badge and the deadline are
	// rendered from the same row, and the pair is only a contradiction in one of
	// these two tenses.
	data := domainsPageData{
		Rows: []domainRow{
			{
				ID: "0198c9c5-0000-7000-8000-000000000101", Hostname: "future.example.com",
				ScopeLabel: "This workspace", Verified: true, Manageable: true,
				RecordName: "_linkctrl-challenge.future.example.com", RecordData: "tok-future",
				CheckError: "no TXT record was found",
				StopsAt:    "9 Aug 2026 14:30 UTC", StopsAtPassed: false,
			},
			{
				ID: "0198c9c5-0000-7000-8000-000000000102", Hostname: "expired.example.com",
				ScopeLabel: "This workspace", Verified: true, Manageable: true,
				RecordName: "_linkctrl-challenge.expired.example.com", RecordData: "tok-expired",
				CheckError: "no TXT record was found",
				StopsAt:    "8 Aug 2026 09:00 UTC", StopsAtPassed: true,
			},
		},
	}

	rec := httptest.NewRecorder()
	if err := r.RenderPartial(rec, http.StatusOK, "domains", "domain_panel", data); err != nil {
		t.Fatalf("render domain_panel: %v", err)
	}
	body := rec.Body.String()

	// The row that is genuinely counting down still counts down. This pair —
	// Verified beside a future stop — is not the defect and must go on rendering.
	if !strings.Contains(body, "stop being served at 9 Aug 2026 14:30 UTC") {
		t.Error("the future deadline lost its countdown; a hostname inside the grace " +
			"window has to be told when it runs out")
	}
	if !strings.Contains(body, "Verified — links are served here") {
		t.Error("the badge did not render, so this test is not looking at the pair it claims to")
	}

	// The row whose window has run out must not be written in the future tense.
	if strings.Contains(body, "stop being served at 8 Aug 2026 09:00 UTC") {
		t.Error("the page says links stop being served at a time that has already " +
			"passed, beside a badge saying they are served here (F161)")
	}
	if !strings.Contains(body, "ran out at 8 Aug 2026 09:00 UTC") {
		t.Error("the expired window was not reported at all; the deadline is still the " +
			"only thing on the page that says why a serving hostname is about to stop")
	}
}
