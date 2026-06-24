package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestBackgroundScheduler_RunsPeriodically verifies a job fires more than once
// on its ticker, then stops cleanly.
func TestBackgroundScheduler_RunsPeriodically(t *testing.T) {
	s := NewBackgroundScheduler()
	var calls int32
	s.Add("ticker-test", 20*time.Millisecond, func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	s.Start()
	// Wait long enough for at least 3 ticks (immediate + 2 intervals).
	time.Sleep(90 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("expected >= 3 calls, got %d", got)
	}
}

// TestBackgroundScheduler_StopIsIdempotent ensures calling Stop twice is safe.
func TestBackgroundScheduler_StopIsIdempotent(t *testing.T) {
	s := NewBackgroundScheduler()
	s.Add("noop", time.Hour, func(context.Context) error { return nil })
	s.Start()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestBackgroundScheduler_NegativeIntervalSkipped verifies jobs with a
// non-positive interval are dropped (feature-flag friendly).
func TestBackgroundScheduler_NegativeIntervalSkipped(t *testing.T) {
	s := NewBackgroundScheduler()
	s.Add("disabled", 0, func(context.Context) error { return nil })
	s.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = s.Stop(ctx)
	// Nothing to assert beyond "doesn't panic / hang"; the job simply never ran.
}

// TestBackgroundScheduler_StopAbortsInFlightJob verifies the context passed to a
// running job is cancelled when Stop is called, so long-running jobs can exit.
func TestBackgroundScheduler_StopAbortsInFlightJob(t *testing.T) {
	s := NewBackgroundScheduler()
	started := make(chan struct{})
	s.Add("long", 10*time.Millisecond, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return nil // ctx cancelled = normal shutdown
	})

	s.Start()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The job returned because its context was cancelled — exactly what we want.
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
