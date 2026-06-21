package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoroutineTracker_Go(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: 5 * time.Second, MaxGoroutines: 10000})

	var counter atomic.Int32

	for i := 0; i < 10; i++ {
		tracker.Go("test-task", func(ctx context.Context) {
			counter.Add(1)
		})
	}

	// Wait for goroutines to complete.
	time.Sleep(100 * time.Millisecond)
	if n := counter.Load(); n != 10 {
		t.Errorf("expected 10 completed, got %d", n)
	}

	if c := tracker.Count(); c != 0 {
		t.Errorf("expected 0 tracked, got %d", c)
	}
}

func TestGoroutineTracker_PanicRecovery(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: 5 * time.Second, MaxGoroutines: 10000})

	var recovered atomic.Bool
	tracker.Go("panicking-task", func(ctx context.Context) {
		panic("test panic")
	})

	// The tracker should have caught this panic.
	time.Sleep(50 * time.Millisecond)

	// Verify the tracker is still alive by spawning another task.
	var ok atomic.Bool
	tracker.Go("after-panic", func(ctx context.Context) {
		ok.Store(true)
	})
	time.Sleep(50 * time.Millisecond)

	if !ok.Load() {
		t.Error("tracker did not recover from panic - subsequent tasks blocked")
	}
	_ = recovered.Load() // unused but verifies the pattern
}

func TestGoroutineTracker_Shutdown(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{MaxGoroutines: 10000})

	var running atomic.Bool
	running.Store(true)

	tracker.Go("long-running", func(ctx context.Context) {
		<-ctx.Done()
		running.Store(false)
	})

	// Shutdown should cancel the context.
	err := tracker.Shutdown(1 * time.Second)
	if err != nil {
		t.Errorf("shutdown error: %v", err)
	}

	if running.Load() {
		t.Error("goroutine still running after shutdown")
	}
}

func TestGoroutineTracker_Snapshot(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{MaxGoroutines: 10000})

	block := make(chan struct{})
	tracker.Go("blocked-task", func(ctx context.Context) {
		<-block
	})

	time.Sleep(10 * time.Millisecond) // let goroutine start
	snap := tracker.Snapshot()

	if snap.Total < 1 {
		t.Errorf("expected at least 1 tracked goroutine, got %d", snap.Total)
	}
	if snap.RuntimeTotal < 1 {
		t.Error("expected runtime goroutine count > 0")
	}
	if snap.BySource["blocked-task"] != 1 {
		t.Errorf("expected 1 blocked-task, got %d", snap.BySource["blocked-task"])
	}

	close(block)
	time.Sleep(50 * time.Millisecond)
}

func TestGoroutineTracker_DefaultConfig(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{})
	if tracker == nil {
		t.Fatal("expected non-nil tracker")
	}
	// Should have default leak threshold and max goroutines.
	snap := tracker.Snapshot()
	if snap.LeakThreshold == "" {
		t.Error("expected non-empty leak threshold")
	}
}
