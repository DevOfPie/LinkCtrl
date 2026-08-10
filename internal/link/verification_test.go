package link

import (
	"strings"
	"testing"
	"time"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// TestOnlyTheWriteThatFilledTheColumnCountsAsAVerification is F180's
// discriminator, on its own.
//
// **The question is "did *this* call verify it", and the row a caller read
// before its DNS lookup cannot answer it.** MarkDomainVerified writes
// `verified_at = COALESCE(verified_at, now())` beside `updated_at = now()` in one
// statement, so the two agree exactly when the NULL was filled here. Two leaders
// that both read the row unverified — a whole DNS round trip apart from their own
// writes — both believed they had verified it and both wrote a `domain.verified`
// audit record for one verification.
func TestOnlyTheWriteThatFilledTheColumnCountsAsAVerification(t *testing.T) {
	wrote := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	earlier := wrote.Add(-90 * time.Minute)

	for _, tc := range []struct {
		name string
		row  dbgen.Domain
		want bool
	}{
		{
			// The winner. Its own statement filled the column, so both stamps are
			// the one transaction timestamp.
			name: "this write filled the column",
			row:  dbgen.Domain{VerifiedAt: &wrote, UpdatedAt: wrote},
			want: true,
		},
		{
			// The loser of F180's race: it wrote, the write succeeded, and it
			// verified nothing — somebody else's now() is in verified_at.
			name: "another write got there first",
			row:  dbgen.Domain{VerifiedAt: &earlier, UpdatedAt: wrote},
			want: false,
		},
		{
			// The hourly re-check of a hostname that has served for weeks. This
			// was never the duplicate, and it must stay silent.
			name: "an ordinary re-check of a serving hostname",
			row:  dbgen.Domain{VerifiedAt: &earlier, UpdatedAt: wrote.Add(time.Hour)},
			want: false,
		},
		{
			// Cannot come back from MarkDomainVerified, whose COALESCE always
			// leaves the column set. Asserted anyway, because the unsafe reading
			// of a NULL here is "yes, newly verified".
			name: "no verified_at at all",
			row:  dbgen.Domain{UpdatedAt: wrote},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newlyVerified(tc.row); got != tc.want {
				t.Errorf("newlyVerified = %v, want %v; a %v here is one "+
					"domain.verified audit record too %s", got, tc.want, got,
					map[bool]string{true: "many", false: "few"}[got])
			}
		})
	}
}

// TestTheGraceClauseIsWrittenInTheTenseItIsRead is F161's second limb at the
// service, where the same past deadline reaches the API as well as the page.
//
// The window is measured entirely in database time: `verification_failing_since`
// anchors it and `verification_checked_at` is the check that just ran, and both
// come out of the same statement. Nothing here consults an app clock, which is
// the point — a serving hostname's remaining window must not depend on which
// replica answered.
func TestTheGraceClauseIsWrittenInTheTenseItIsRead(t *testing.T) {
	const grace = 24 * time.Hour
	failing := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	stops := failing.Add(grace) // 9 Aug 09:00

	at := func(ts time.Time) *time.Time { return &ts }
	// The one construction that promises a future stop, so the negative
	// assertions below cannot be satisfied by a sentence that merely mentions the
	// deadline — the past arm names the same instant and must name it differently.
	promise := "they stop at " + stops.Format(time.RFC1123)

	t.Run("inside the window", func(t *testing.T) {
		checked := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
		got := stillServedFor(dbgen.Domain{
			VerificationFailingSince: at(failing), VerificationCheckedAt: at(checked),
		}, grace)
		if !strings.Contains(got, promise) {
			t.Errorf("%q does not count the window down; a hostname with thirteen "+
				"hours left has to be told when they run out", got)
		}
	})

	t.Run("after the window, before the pass that acts on it", func(t *testing.T) {
		// Up to an hour wide: the hourly pass is the only thing that stops
		// serving, so the deadline is behind us while links are still served.
		checked := stops.Add(20 * time.Minute)
		got := stillServedFor(dbgen.Domain{
			VerificationFailingSince: at(failing), VerificationCheckedAt: at(checked),
		}, grace)
		if strings.Contains(got, promise) {
			t.Errorf("%q promises a stop at a time that has passed, in the same "+
				"sentence that says the links are still served (F161)", got)
		}
		if !strings.Contains(got, "ran out at "+stops.Format(time.RFC1123)) {
			t.Errorf("%q does not report the spent window, which is the only thing on "+
				"the page that says why a serving hostname is about to stop", got)
		}
		if !strings.Contains(got, "they stop at the next check") {
			t.Errorf("%q does not say what ends it", got)
		}
	})

	t.Run("the instant the window closes", func(t *testing.T) {
		got := stillServedFor(dbgen.Domain{
			VerificationFailingSince: at(failing), VerificationCheckedAt: at(stops),
		}, grace)
		if strings.Contains(got, promise) {
			t.Errorf("%q says links stop at the very instant being reported; the "+
				"boundary belongs to the past arm", got)
		}
	})

	t.Run("no anchor at all", func(t *testing.T) {
		// The statement COALESCEs it, so this cannot arrive — and if it ever
		// does, the unsafe reading is "the window has not started".
		got := stillServedFor(dbgen.Domain{}, grace)
		if !strings.Contains(got, "at the next check") {
			t.Errorf("%q treats a missing anchor as time in hand", got)
		}
	})
}
