package logtasks

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrack_FireAndForget(t *testing.T) {
	c := New()
	var ran atomic.Bool
	c.Track(func() {
		time.Sleep(10 * time.Millisecond)
		ran.Store(true)
	})
	_ = c.Wait(context.Background())
	if !ran.Load() {
		t.Error("task did not run")
	}
}

func TestTrackRequestWrite_SerializesPerKey(t *testing.T) {
	c := New()
	var order []int

	// Two writes for the same request must run in insertion order.
	c.TrackRequestWrite("req1", func() { order = append(order, 1) })
	c.TrackRequestWrite("req1", func() { order = append(order, 2) })
	_ = c.Wait(context.Background())

	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("order: %v (expected [1 2])", order)
	}
}

func TestTrackRequestWrite_DifferentKeysConcurrent(t *testing.T) {
	c := New()
	// Different keys are independent (no forced ordering between them).
	doneA := c.TrackRequestWrite("a", func() { time.Sleep(20 * time.Millisecond) })
	doneB := c.TrackRequestWrite("b", func() {})
	// "b" should complete before "a" since it has no wait.
	select {
	case <-doneB:
	case <-time.After(time.Second):
		t.Fatal("b did not complete promptly")
	}
	<-doneA
	_ = c.Wait(context.Background())
}

func TestWaitForRequest_NoPendingReturnsImmediately(t *testing.T) {
	c := New()
	ch := c.WaitForRequest("none")
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("WaitForRequest with no pending task should return immediately")
	}
}

func TestWaitForRequest_WaitsForCompletion(t *testing.T) {
	c := New()
	done := c.TrackRequestWrite("r", func() { time.Sleep(30 * time.Millisecond) })
	waitCh := c.WaitForRequest("r")
	// Not done yet.
	select {
	case <-waitCh:
		t.Fatal("should not be done yet")
	default:
	}
	<-done
	<-waitCh
}

func TestWait_DrainsAll(t *testing.T) {
	c := New()
	var n atomic.Int32
	for i := 0; i < 10; i++ {
		c.Track(func() {
			time.Sleep(5 * time.Millisecond)
			n.Add(1)
		})
	}
	_ = c.Wait(context.Background())
	if n.Load() != 10 {
		t.Errorf("only %d tasks ran", n.Load())
	}
	if c.PendingCount() != 0 {
		t.Errorf("pending count after drain: %d", c.PendingCount())
	}
}

func TestWait_RespectsContextCancel(t *testing.T) {
	c := New()
	c.Track(func() { time.Sleep(5 * time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Wait(ctx); err == nil {
		t.Error("Wait should return ctx.Err() on timeout")
	}
}
