package server

import (
	"context"
	"log"
	"sync"
	"time"
)

// BackgroundScheduler runs named periodic jobs on a single goroutine, each on
// its own ticker. It is the home for housekeeping tasks that don't belong on the
// request path (e.g. pruning old log rows). Today it backs the consolestore
// cleanup; when more periodic jobs appear they register here, and the type can
// graduate to its own package without touching call sites.
//
// Lifecycle:
//   - Add registers a job before Start (interval <= 0 means "skip").
//   - Start launches one goroutine for the whole scheduler. Each job runs on a
//     nested goroutine so a slow job can't starve the others.
//   - Stop cancels every job and waits for in-flight runs to finish (or the
//     context deadline), then returns. Wire Stop into the server's drain so the
//     scheduler shuts down cleanly on SIGTERM/SIGINT.
type BackgroundScheduler struct {
	mu      sync.Mutex
	jobs    []scheduledJob
	stop    context.CancelFunc
	wg      sync.WaitGroup // tracks the scheduler loop + in-flight job runs
	started bool
}

type scheduledJob struct {
	name     string
	interval time.Duration
	fn       func(context.Context) error
}

// NewBackgroundScheduler builds an empty scheduler.
func NewBackgroundScheduler() *BackgroundScheduler {
	return &BackgroundScheduler{}
}

// Add registers a periodic job. Must be called before Start. Jobs with a
// non-positive interval are dropped (handy for feature flags). Each run gets the
// scheduler's context, which is cancelled on Stop so a well-behaved job can
// abort promptly.
func (s *BackgroundScheduler) Add(name string, interval time.Duration, fn func(context.Context) error) {
	if interval <= 0 || fn == nil {
		return
	}
	s.mu.Lock()
	s.jobs = append(s.jobs, scheduledJob{name: name, interval: interval, fn: fn})
	s.mu.Unlock()
}

// Start launches the scheduler. It runs an initial pass of every job
// immediately (so the first cleanup isn't delayed by a full interval), then
// each job ticks on its own cadence. Panics if called twice.
func (s *BackgroundScheduler) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		panic("BackgroundScheduler: Start called twice")
	}
	s.started = true
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	jobs := append([]scheduledJob(nil), s.jobs...)
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// One sub-goroutine per job, so they tick independently.
		for _, j := range jobs {
			s.wg.Add(1)
			go func(j scheduledJob) {
				defer s.wg.Done()
				// Fire once immediately, then on the ticker.
				s.runJob(ctx, j)
				t := time.NewTicker(j.interval)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						s.runJob(ctx, j)
					}
				}
			}(j)
		}
	}()
	log.Printf("[scheduler] started with %d job(s)", len(jobs))
}

// runJob executes one tick of a job, logging (never panicking on) errors so a
// single bad run doesn't kill the goroutine.
func (s *BackgroundScheduler) runJob(ctx context.Context, j scheduledJob) {
	start := time.Now()
	// Track the run so Stop waits for it. Add/Done bracket the call.
	s.wg.Add(1)
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[scheduler] job %q panic: %v", j.name, r)
		}
	}()
	if err := j.fn(ctx); err != nil && ctx.Err() == nil {
		log.Printf("[scheduler] job %q error: %v", j.name, err)
	}
	_ = start // available for future debug logging of run duration
}

// Stop signals every job to wind down and waits for in-flight runs to finish
// or ctx to expire. Safe to call once; further calls are no-ops. Wire this into
// the server's drain function for graceful shutdown.
func (s *BackgroundScheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.stop
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()

	// Wait for jobs, but give up when the shutdown context expires.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("[scheduler] stopped cleanly")
	case <-ctx.Done():
		log.Printf("[scheduler] stop timed out, exiting anyway")
	}
	return nil
}
