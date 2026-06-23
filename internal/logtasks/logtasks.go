// Package logtasks orchestrates asynchronous request-log writes so they never
// block the hot request path, while guaranteeing ordering and graceful drain.
//
// It is the Go equivalent of src/console-log-tasks.ts. The original used a
// Promise chain per requestId to serialize writes (INSERT before UPDATE) plus a
// global Set for drain. Here:
//
//   - A global sync.WaitGroup tracks all in-flight background tasks (drained on
//     shutdown via Wait).
//   - Per-requestId serialization is modeled as a chain of goroutines guarded
//     by a mutex over a map of "last done" signals, so writes for the same
//     request never overlap and always run in insertion order.
package logtasks

import (
	"context"
	"sync"
)

// Coordinator tracks in-flight async log tasks and serializes per-request
// writes. Construct one, share it across all request handlers, and call
// Wait during graceful shutdown.
type Coordinator struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	last   map[string]chan struct{} // requestId → "previous write done" signal
	count  int64                    // in-flight task count (for metrics)
}

// New builds a Coordinator.
func New() *Coordinator {
	return &Coordinator{last: map[string]chan struct{}{}}
}

// Track runs fn in a background goroutine and registers it with the global
// WaitGroup. Use this for fire-and-forget response observation.
func (c *Coordinator) Track(fn func()) {
	c.wg.Add(1)
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			c.count--
			c.mu.Unlock()
		}()
		fn()
	}()
}

// TrackRequestWrite runs fn for the given requestId, serialized after any
// prior write for the same requestId. This guarantees the request INSERT
// completes before the response UPDATE for the same request. Returns a done
// signal channel the caller may wait on.
//
// Mirrors trackPendingConsoleRequestWrite: builds a chain on the previous
// task for this requestId, so a failed prior write does not abort the next.
func (c *Coordinator) TrackRequestWrite(requestID string, fn func()) <-chan struct{} {
	c.mu.Lock()
	prev := c.last[requestID]
	done := make(chan struct{})
	c.last[requestID] = done
	c.mu.Unlock()

	c.wg.Add(1)
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer close(done)
		defer func() {
			c.mu.Lock()
			c.count--
			c.mu.Unlock()
		}()
		if prev != nil {
			<-prev
		}
		fn()
	}()
	return done
}

// WaitForRequest returns a channel that closes when the latest tracked write
// for requestId completes (or immediately if none is pending). Callers use
// this to ensure the INSERT has landed before issuing the UPDATE.
func (c *Coordinator) WaitForRequest(requestID string) <-chan struct{} {
	c.mu.Lock()
	ch := c.last[requestID]
	c.mu.Unlock()
	if ch == nil {
		// No pending write; return an already-closed channel.
		done := make(chan struct{})
		close(done)
		return done
	}
	return ch
}

// Wait blocks until all tracked background tasks finish, or until ctx is
// cancelled. Used for graceful shutdown.
func (c *Coordinator) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PendingCount returns the number of tracked in-flight tasks (for metrics /
// the perf monitor).
func (c *Coordinator) PendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int(c.count)
}
