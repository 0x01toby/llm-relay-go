// Package server assembles the HTTP server with graceful shutdown.
//
// It fixes a real defect in the original Node service: the original exports
// waitForPendingResponseLogs but never wires it to a signal handler, so
// in-flight background log writes can be lost on kill. Here, SIGTERM/SIGINT
// trigger Shutdown (which waits for active requests), and callers register a
// drain function (via WithDrain) to flush background work before the process
// exits.
package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// ShutdownTimeout is the maximum time we wait for in-flight work during a
// graceful shutdown. Generous because LLM streams can be long-lived; the
// background-log drain has its own (shorter) budget.
const ShutdownTimeout = 30 * time.Second

// Server holds the HTTP server and its lifecycle dependencies.
type Server struct {
	cfg          ServerConfig
	httpSrv      *http.Server
	drain        func(context.Context) error
	shutdownOnce atomic.Bool
}

// ServerConfig configures the server.
type ServerConfig struct {
	// Addr is the listen address (":3300").
	Addr string
	// Handler is the root HTTP handler (the chi router in full builds; a
	// minimal mux for the P1 skeleton).
	Handler http.Handler
	// ReadHeaderTimeout caps how long we wait for request headers.
	ReadHeaderTimeout time.Duration
}

// New constructs a Server. Handler must already have CORS middleware applied
// at its outermost layer (the server itself does not add CORS).
func New(cfg ServerConfig) *Server {
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           cfg.Handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		// WriteTimeout/ReadTimeout are intentionally unset: LLM streams are
		// long-lived and a global timeout would kill them. Per-request
		// timeouts live in the gateway engine (P4).
	}
	return &Server{cfg: cfg, httpSrv: srv}
}

// WithDrain registers a function invoked during graceful shutdown, after the
// HTTP server stops accepting new connections but before the process exits.
// Use it to flush background log writes (the logtasks worker pool, P5).
func (s *Server) WithDrain(drain func(context.Context) error) { s.drain = drain }

// Run starts the server and blocks until it receives SIGINT/SIGTERM, then
// performs a graceful shutdown. It returns nil on a clean shutdown.
func (s *Server) Run() error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("server listening on %s", s.cfg.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
		return s.Shutdown()
	}
}

// Shutdown stops accepting new connections, waits for active requests to
// finish, then runs the registered drain function. Safe to call once; further
// calls are no-ops.
func (s *Server) Shutdown() error {
	if !s.shutdownOnce.CompareAndSwap(false, true) {
		return nil
	}

	// Stop accepting new requests; give active ones time to finish.
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()

	if err := s.httpSrv.Shutdown(ctx); err != nil {
		log.Printf("http shutdown error: %v", err)
		// Continue to drain anyway.
	}

	// Drain background work (e.g. pending log writes). Give it a shorter
	// budget than the overall shutdown so we still exit promptly.
	if s.drain != nil {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.drain(drainCtx); err != nil {
			log.Printf("drain error: %v", err)
		}
		drainCancel()
	}
	return nil
}
