package automation

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DevOfPie/LinkCtrl/internal/store/dbgen"
)

// The resume cursor is the pure half of the tie fix, and these three cases are
// its whole contract. Every match query orders by (event time, id) and the next
// window must open strictly after wherever the previous firing stopped — a
// *position* in that order, not just an instant, because a capped run can stop
// part-way through a group of subjects sharing one timestamp and an instant
// cannot say which of them were handled.
//
// The direction of every fallback is the same direction: when the id half is
// missing, the boundary instant is treated as fully spent. uuid.Max encodes
// that — nothing sorts after it — and uuid.Nil would encode the opposite,
// re-admitting the very subjects the last firing already handled, which is the
// runaway TestARuleFiresOnceForOneSubject exists to keep impossible.

func TestResumeCursorOpensAtCreationForAnUnfiredRule(t *testing.T) {
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	after, afterSubject := resumeCursor(dbgen.ListDueAutomationRulesRow{
		CreatedAt: created,
	})

	if !after.Equal(created) {
		t.Errorf("a rule that never fired resumes at %s, want its creation instant %s; "+
			"anything earlier fires for history that predates the rule", after, created)
	}
	if afterSubject != uuid.Max {
		t.Errorf("the subject half is %s, want uuid.Max: with no recorded subject, "+
			"everything at the boundary instant must count as spent, and only the "+
			"maximum uuid admits nothing past the tiebreak", afterSubject)
	}
}

func TestResumeCursorTreatsAMissingSubjectAsATimestampFullySpent(t *testing.T) {
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fired := created.Add(time.Hour)

	after, afterSubject := resumeCursor(dbgen.ListDueAutomationRulesRow{
		CreatedAt:   created,
		LastFiredAt: &fired,
	})

	if !after.Equal(fired) {
		t.Errorf("resume instant is %s, want the watermark %s", after, fired)
	}
	// Every row written before the subject column existed — and every re-armed
	// rule, whose subject half is reset — was written under strict `>` semantics:
	// its boundary instant is spent in full. uuid.Max is that exact semantic.
	if afterSubject != uuid.Max {
		t.Errorf("the subject half is %s, want uuid.Max: a legacy watermark means "+
			"strict `>` on the instant, and any smaller uuid would re-fire subjects "+
			"tied on it", afterSubject)
	}
}

func TestResumeCursorResumesInsideATieGroup(t *testing.T) {
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fired := created.Add(time.Hour)
	boundary := uuid.MustParse("018f0000-0000-7000-8000-000000000019")

	after, afterSubject := resumeCursor(dbgen.ListDueAutomationRulesRow{
		CreatedAt:          created,
		LastFiredAt:        &fired,
		LastFiredSubjectID: &boundary,
	})

	if !after.Equal(fired) {
		t.Errorf("resume instant is %s, want the watermark %s", after, fired)
	}
	if afterSubject != boundary {
		t.Errorf("the subject half is %s, want the recorded boundary %s: this pair is "+
			"what lets the next window re-enter a tie group the cap split, instead of "+
			"skipping past the tied remainder forever", afterSubject, boundary)
	}
}
