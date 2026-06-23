package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/taozhang/llmrelay/internal/health"
)

// freeAddr returns a host:port on a listening socket that is immediately
// closed, guaranteeing the address is free for the test server to bind.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// startAndSignal starts srv.Run() in a goroutine and returns a function that
// sends SIGINT to the current process to trigger the real signal-based
// shutdown path, then waits for Run to return. Using the actual signal (rather
// than calling Shutdown directly) exercises the production code path and
// guarantees Run() unblocks.
func startAndSignal(t *testing.T, srv *Server) func() {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = srv.Run(); close(done) }()
	// Wait until the listener is actually accepting connections.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", srv.cfg.Addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() {
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run() did not return after SIGINT")
		}
	}
}

// TestServer_GracefulShutdown_WaitsForActiveRequests verifies that the
// signal-triggered shutdown does not cut off a request already in flight —
// the core fix for the original service's defect.
func TestServer_GracefulShutdown_WaitsForActiveRequests(t *testing.T) {
	var active atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Store(true)
		time.Sleep(150 * time.Millisecond) // simulate a slow request
		w.WriteHeader(http.StatusOK)
		active.Store(false)
	})

	srv := New(ServerConfig{Addr: freeAddr(t), Handler: handler})
	stop := startAndSignal(t, srv)

	// Fire a request, then immediately signal. The request must still complete.
	reqDone := make(chan struct{})
	go func() {
		resp, err := http.Get("http://" + srv.cfg.Addr + "/")
		if err == nil {
			resp.Body.Close()
		}
		close(reqDone)
	}()
	time.Sleep(20 * time.Millisecond) // let the request land in the handler

	stop()

	select {
	case <-reqDone:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request did not complete during shutdown")
	}
	if active.Load() {
		t.Error("handler was still running when shutdown completed")
	}
}

// TestServer_DrainInvokedDuringShutdown verifies the registered drain function
// runs during the signal-based shutdown (this is what flushes background log
// writes in P5).
func TestServer_DrainInvokedDuringShutdown(t *testing.T) {
	var drained atomic.Bool
	srv := New(ServerConfig{Addr: freeAddr(t), Handler: http.NotFoundHandler()})
	srv.WithDrain(func(ctx context.Context) error {
		drained.Store(true)
		return nil
	})
	stop := startAndSignal(t, srv)
	stop()
	if !drained.Load() {
		t.Error("drain function was not invoked")
	}
}

// TestServer_DrainErrorDoesNotAbort verifies a failing drain is logged, not
// fatal — the process still exits cleanly.
func TestServer_DrainErrorDoesNotAbort(t *testing.T) {
	srv := New(ServerConfig{Addr: freeAddr(t), Handler: http.NotFoundHandler()})
	srv.WithDrain(func(ctx context.Context) error { return errors.New("drain failed") })
	stop := startAndSignal(t, srv)
	stop() // returns when Run() returns; no fatal
}

// TestServer_ShutdownIdempotent verifies Shutdown can be called multiple times
// safely, without a running server (Shutdown works on a never-started server).
func TestServer_ShutdownIdempotent(t *testing.T) {
	srv := New(ServerConfig{Addr: freeAddr(t), Handler: http.NotFoundHandler()})
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

// --- Root mux tests ---

func TestRootMux_HealthDegraded(t *testing.T) {
	mig := health.New()
	mig.Set(health.StatusSnapshot{State: health.StatusFailed, Err: "conn refused"})
	mux := RootMux(mig, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("health degraded: got %d", w.Code)
	}
}

func TestRootMux_RootDegradedPage(t *testing.T) {
	mig := health.New()
	mig.Set(health.StatusSnapshot{State: health.StatusFailed, Err: "conn refused"})
	// GatewayMux owns the root handler; pass a placeholder root handler.
	mux := GatewayMux(mig, nil, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("root degraded status: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "migration failed") {
		t.Errorf("expected degraded HTML, got: %s", w.Body.String())
	}
}

func TestRootMux_RootHealthyPlaceholder(t *testing.T) {
	mig := health.New() // success by default
	mux := GatewayMux(mig, nil, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"service":"llm-relay"}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("root healthy status: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "llm-relay") {
		t.Errorf("expected placeholder, got: %s", w.Body.String())
	}
}

func TestRootMux_DbReset_RefusedWhenHealthy(t *testing.T) {
	mig := health.New()
	mux := RootMux(mig, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/db/reset", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("reset when healthy should be 400, got %d", w.Code)
	}
}

func TestRootMux_DbReset_RecoversOnSuccess(t *testing.T) {
	mig := health.New()
	mig.Set(health.StatusSnapshot{State: health.StatusFailed})
	called := false
	mux := RootMux(mig, func() (bool, string, error) {
		called = true
		return true, "reset ok", nil
	})
	req := httptest.NewRequest(http.MethodPost, "/api/db/reset", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !called {
		t.Error("reset function not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status: %d", w.Code)
	}
	// After success the service should be healthy again.
	if !mig.Healthy() {
		t.Error("migration status not flipped to healthy after reset")
	}
}

func TestRootMux_DbReset_AllowsGetMethodOnly(t *testing.T) {
	mig := health.New()
	mig.Set(health.StatusSnapshot{State: health.StatusFailed})
	mux := RootMux(mig, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/db/reset", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should be 405, got %d", w.Code)
	}
}

// silence unused import if signal isn't referenced on all build configs.
var _ = syscall.SIGTERM
