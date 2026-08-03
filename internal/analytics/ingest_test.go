package analytics

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newQueueOnlyIngester builds an ingester without starting its consumer.
//
// Deliberately unstarted: every test here is about the producer side and the
// shutdown handshake, both of which run entirely in Record and Close. Starting
// the loop would drain the queue into a flush, and a flush wants the database
// this test does not have. With no consumer the queue also stays full, which is
// the condition the overflow behaviour is defined by.
func newQueueOnlyIngester(queue int) *Ingester {
	return NewIngester(nil, nil, IngestConfig{
		QueueSize:     queue,
		BatchSize:     1 << 20,
		FlushInterval: time.Hour,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func testEvent() Event {
	return Event{LinkID: uuid.New(), WorkspaceID: uuid.New(), OccurredAt: time.Now()}
}

// Record and Close race by construction: Record's closed check and its send
// cannot be one atomic step, so a redirect already past the check can reach the
// send after Close has run. While Close closed the event channel, that was a
// panic — during shutdown, in the goroutine serving a live request, in the one
// window where the process is trying not to lose data. Close now signals
// through a separate channel and never closes the one Record sends on.
//
// There is no error to assert on: the regression crashes the test binary.
func TestRecordDuringCloseDoesNotPanic(t *testing.T) {
	for range 20 {
		i := newQueueOnlyIngester(8)

		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range 128 {
					i.Record(testEvent())
				}
			}()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		close(start)
		if err := i.Close(ctx); err != nil {
			cancel()
			t.Fatalf("Close: %v", err)
		}
		wg.Wait()
		cancel()
	}
}

// Close is idempotent. The shutdown path calls it from main's defer stack as
// well as from the server wrapper, and closing the stop channel twice panics.
func TestCloseIsIdempotent(t *testing.T) {
	i := newQueueOnlyIngester(4)

	if err := i.Close(t.Context()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := i.Close(t.Context()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// The contract Record is written to: never blocks, whatever the queue is doing.
// A redirect must not wait on analytics, so overflow is a drop and a counter.
func TestRecordNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	i := newQueueOnlyIngester(2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			i.Record(testEvent())
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a full queue; the redirect path would block with it")
	}

	if got := i.Stats.Enqueued.Load(); got != 2 {
		t.Errorf("Enqueued = %d, want 2 (the queue size)", got)
	}
	if got := i.Stats.Dropped.Load(); got != 998 {
		t.Errorf("Dropped = %d, want 998; overflow has to be counted or it is invisible", got)
	}
}

// After Close, Record is a no-op rather than an enqueue: the consumer is gone,
// so anything accepted would be counted as enqueued and never written.
func TestRecordAfterCloseIsDropped(t *testing.T) {
	i := newQueueOnlyIngester(8)
	if err := i.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := i.Stats.Enqueued.Load()
	i.Record(testEvent())
	if got := i.Stats.Enqueued.Load(); got != before {
		t.Errorf("Enqueued moved from %d to %d after Close", before, got)
	}
}

// TestCopyRowsMatchTheColumnList is half the guard on the milestone's named
// risk (M36).
//
// pgx.CopyFrom sends values by position. A column list and a row slice that
// disagree do not produce an error — they write a browser into a country and a
// latency into a language, silently, forever. This catches the omission half:
// a column added to one list and not the other.
//
// The reordering half cannot be caught here, because a shuffled row of the right
// length is still the right length. That is asserted against a real database in
// test/integration/split_test.go, which reads a written row back column by
// column. Both exist because either alone would leave the failure mode open.
//
// **What this test does not see, stated so nobody relies on it:** the row below
// is a hand-written copy of what `prepare` builds, not the output of `prepare`,
// so a change made to `prepare` alone passes here. It fails in
// test/integration/split_test.go instead, which is why that test reads every
// column back rather than only the new one. The copy is deliberate — deriving
// the row from `prepare` would need an enriched batch, a salt cache and a
// database, and the check would then be an integration test wearing a unit
// test's name.
func TestCopyRowsMatchTheColumnList(t *testing.T) {
	// The shape of the row prepare builds, written out here rather than derived,
	// so that a column added to clickEventColumns alone is what fails.
	row := []any{
		uuid.Nil, uuid.Nil, uuid.Nil, time.Now(), []byte{1},
		false, nil, nil, nil, "desktop", "Chrome",
		"macOS", "en", "example.org", false, int32(1),
		destinationOrNil(uuid.Nil),
	}
	if len(row) != len(clickEventColumns) {
		t.Fatalf("prepare builds %d values for %d columns (%v); a value is about to "+
			"land in the wrong column and nothing will report it",
			len(row), len(clickEventColumns), clickEventColumns)
	}
	if clickEventColumns[len(clickEventColumns)-1] != "destination_id" {
		t.Errorf("destination_id is no longer last in the column list. It was appended "+
			"rather than inserted so the sixteen positions that predate it could not "+
			"shift; moving it means the row builder has to move with it. Columns: %v",
			clickEventColumns)
	}
}

// TestDestinationOrNilKeepsTheZeroUUIDOutOfTheColumn is why the breakdown has no
// bucket that looks like data.
func TestDestinationOrNilKeepsTheZeroUUIDOutOfTheColumn(t *testing.T) {
	if got := destinationOrNil(uuid.Nil); got != nil {
		t.Errorf("the zero uuid became %v, want NULL — NULL is what means \"the link's "+
			"own destination\", and a literal zero would be a destination id that "+
			"resolves to nothing", got)
	}
	id := uuid.Must(uuid.NewV7())
	got := destinationOrNil(id)
	if got == nil || *got != id {
		t.Errorf("destinationOrNil(%v) = %v", id, got)
	}
}
