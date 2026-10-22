package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
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

// The tracker recovers a tracked goroutine's panic itself, ahead of GoSafe,
// so it has to report the panic too.
func TestGoroutineTracker_ReportsTheRecoveredPanic(t *testing.T) {
	reports := catchPanicReports(t, OwnerEngine+"/tracked-report")
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: 5 * time.Second, MaxGoroutines: 10000})

	tracker.Go("tracked-report", func(ctx context.Context) { panic("tracked boom") })

	select {
	case got := <-reports:
		if got.Value != "tracked boom" {
			t.Fatalf("report = %+v, want the panic value", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the tracker recovered the panic but never reported it")
	}
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

// lockedBuffer serializes writes from any tracker's monitor loop against
// the test's read of what was logged.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) count(substr string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), substr)
}

func captureWarnings(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestGoroutineTracker_LongLivedWorkerIsNotALeak(t *testing.T) {
	logs := captureWarnings(t)
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: time.Millisecond, MaxGoroutines: 50000})

	var stopped atomic.Bool
	tracker.GoLongLived("long-lived-sweeper", func(ctx context.Context) {
		<-ctx.Done()
		stopped.Store(true)
	})

	time.Sleep(10 * time.Millisecond)
	tracker.checkLeaks()
	tracker.checkLeaks()

	if n := logs.count("name=long-lived-sweeper"); n != 0 {
		t.Errorf("long-lived worker reported as a leak %d times, want 0", n)
	}
	if c := tracker.Count(); c != 1 {
		t.Errorf("tracked = %d, want 1: a long-lived worker stays visible", c)
	}
	if err := tracker.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !stopped.Load() {
		t.Error("Shutdown returned before the long-lived worker stopped")
	}
}

func TestGoroutineTracker_LeakWarnsOncePerGoroutine(t *testing.T) {
	logs := captureWarnings(t)
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: time.Millisecond, MaxGoroutines: 50000})
	t.Cleanup(func() { _ = tracker.Shutdown(time.Second) })

	tracker.Go("stuck-task", func(ctx context.Context) { <-ctx.Done() })

	time.Sleep(10 * time.Millisecond)
	for range 3 {
		tracker.checkLeaks()
	}

	if n := logs.count("name=stuck-task"); n != 1 {
		t.Errorf("stuck goroutine reported %d times over three checks, want 1", n)
	}
}
