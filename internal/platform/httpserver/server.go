// Package httpserver runs an HTTP server with a drain-aware shutdown sequence.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Server wraps http.Server with the shutdown ordering the service needs.
type Server struct {
	srv *http.Server
	log *slog.Logger

	// OnDrain runs first, before the drain delay. It flips readiness to 503 so
	// a load balancer stops sending new requests while this instance is still
	// able to finish the ones it has.
	OnDrain func()

	// DrainDelay is how long to keep serving after readiness starts failing.
	// It must cover the load balancer's health-check interval, or connections
	// will still be arriving when the listener closes.
	DrainDelay time.Duration

	// ShutdownTimeout bounds waiting for in-flight requests.
	ShutdownTimeout time.Duration

	// OnShutdown runs after the listener is closed and in-flight requests have
	// finished. This is where the click-event buffer gets its final flush.
	OnShutdown func(context.Context) error
}

type Options struct {
	Addr              string
	Handler           http.Handler
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	Logger            *slog.Logger
}

func New(o Options) *Server {
	return &Server{
		srv: &http.Server{
			Addr:    o.Addr,
			Handler: o.Handler,
			// Without ReadHeaderTimeout a client can hold a connection open
			// indefinitely by dribbling headers (Slowloris).
			ReadHeaderTimeout: o.ReadHeaderTimeout,
			WriteTimeout:      o.WriteTimeout,
			ErrorLog:          slog.NewLogLogger(o.Logger.Handler(), slog.LevelWarn),
		},
		log: o.Logger,
	}
}

// Addr reports the address the server is configured to listen on.
func (s *Server) Addr() string { return s.srv.Addr }

// Run serves until ctx is cancelled, then shuts down in order.
//
// The ordering is what makes a rolling restart lossless:
//
//  1. readiness fails, so the load balancer deregisters this instance
//  2. wait DrainDelay, so in-flight and just-dispatched requests are absorbed
//  3. close the listener and wait for in-flight requests to finish
//  4. run OnShutdown, which flushes buffered analytics
//
// Skipping step 2 is the common mistake: the listener closes while the load
// balancer still believes the instance is healthy, and clients see connection
// resets during every deploy.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", slog.String("addr", s.srv.Addr))
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// Failed before any shutdown was requested, e.g. the port is taken.
		return err
	case <-ctx.Done():
	}

	return s.shutdown(ctx, errCh)
}

func (s *Server) shutdown(ctx context.Context, errCh <-chan error) error {
	if s.OnDrain != nil {
		s.log.Info("shutdown: draining", slog.Duration("delay", s.DrainDelay))
		s.OnDrain()
	}
	if s.DrainDelay > 0 {
		time.Sleep(s.DrainDelay)
	}

	// Detached from the parent's cancellation, which by this point has already
	// fired — that is what got us here — but not from its values, so anything
	// carried on the context still reaches the flush. Shutdown work needs its
	// own budget rather than inheriting a dead deadline.
	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), s.ShutdownTimeout)
	defer cancel()

	s.log.Info("shutdown: closing listener and finishing in-flight requests")
	var firstErr error
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		s.log.Error("shutdown: some requests did not finish in time", slog.Any("error", err))
		firstErr = err
	}

	if err := <-errCh; err != nil && firstErr == nil {
		firstErr = err
	}

	if s.OnShutdown != nil {
		s.log.Info("shutdown: flushing buffered work")
		if err := s.OnShutdown(shutdownCtx); err != nil {
			s.log.Error("shutdown: flush failed", slog.Any("error", err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	s.log.Info("shutdown: complete")
	return firstErr
}
