package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The shutdown context must be alive even though the context that triggered
// shutdown is not. It is what OnShutdown flushes buffered clicks and API key
// usage with, so handing it a cancelled context would turn every graceful
// restart into silent data loss — and the loss would be invisible, because the
// flush would return "context canceled" into a log nobody reads.
func TestShutdownFlushesWithALiveContext(t *testing.T) {
	srv := New(Options{
		Addr:    "127.0.0.1:0",
		Handler: http.NotFoundHandler(),
		Logger:  quiet(),
	})
	srv.ShutdownTimeout = 5 * time.Second

	var (
		drained     bool
		flushed     bool
		flushErr    error
		hadDeadline bool
	)
	srv.OnDrain = func() { drained = true }
	srv.OnShutdown = func(ctx context.Context) error {
		flushed = true
		flushErr = ctx.Err()
		_, hadDeadline = ctx.Deadline()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Cancelling is the shutdown signal, standing in for SIGTERM.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if !drained {
		t.Error("OnDrain did not run; readiness would still report ok while the listener closed")
	}
	if !flushed {
		t.Fatal("OnShutdown did not run; buffered work would be lost on every restart")
	}
	if flushErr != nil {
		t.Errorf("the flush context was already done (%v); buffered work cannot be written with it", flushErr)
	}
	if !hadDeadline {
		t.Error("the flush context has no deadline, so a stuck flush would hang shutdown forever")
	}
}

// A listener that cannot bind must be reported rather than waited on: the
// select in Run has to notice the serve error, not sit on ctx.Done until the
// operator gives up and reads the logs.
func TestRunReturnsABindFailure(t *testing.T) {
	srv := New(Options{Addr: "127.0.0.1:-1", Handler: http.NotFoundHandler(), Logger: quiet()})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := srv.Run(ctx)
	if err == nil {
		t.Fatal("Run reported success for an address it cannot listen on")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Run waited for the context instead of reporting the listen failure")
	}
}
