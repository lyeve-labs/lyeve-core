package engine

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoroutineTracker_1000Goroutines(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: 5 * time.Second, MaxGoroutines: 10000})
	t.Cleanup(func() { tracker.Shutdown(1 * time.Second) })

	var completed atomic.Int32
	startGate := make(chan struct{})

	for i := 0; i < 1000; i++ {
		tracker.Go("stress-1k", func(ctx context.Context) {
			<-startGate
			completed.Add(1)
		})
	}

	// Entries are added synchronously in Go() before the goroutine starts.
	if c := tracker.Count(); c != 1000 {
		t.Fatalf("expected 1000 tracked, got %d", c)
	}

	// Release all goroutines.
	close(startGate)

	// Wait for all to finish.
	if err := tracker.Wait(5 * time.Second); err != nil {
		t.Fatalf("wait error: %v", err)
	}

	if n := completed.Load(); n != 1000 {
		t.Errorf("expected 1000 completed, got %d", n)
	}
	if c := tracker.Count(); c != 0 {
		t.Errorf("expected 0 tracked after completion, got %d", c)
	}
}

func TestGoroutineTracker_ConcurrentGo(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: 5 * time.Second, MaxGoroutines: 10000})
	t.Cleanup(func() { tracker.Shutdown(1 * time.Second) })

	var completed atomic.Int32
	var spawners sync.WaitGroup

	const numSpawners = 50
	const perSpawner = 20
	const total = numSpawners * perSpawner

	for range numSpawners {
		spawners.Add(1)
		go func() {
			defer spawners.Done()
			for j := 0; j < perSpawner; j++ {
				tracker.Go("concurrent-go", func(ctx context.Context) {
					completed.Add(1)
				})
			}
		}()
	}

	// Wait for all spawners to finish submitting.
	spawners.Wait()

	// Wait for all tracked goroutines to complete.
	if err := tracker.Wait(5 * time.Second); err != nil {
		t.Fatalf("wait error: %v", err)
	}

	if n := completed.Load(); n != total {
		t.Errorf("expected %d completed, got %d", total, n)
	}
	if c := tracker.Count(); c != 0 {
		t.Errorf("expected 0 tracked after completion, got %d", c)
	}
}

func TestGoroutineTracker_ShutdownWithRunning(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{MaxGoroutines: 10000})

	var exited atomic.Int32
	const numGoroutines = 10

	for i := 0; i < numGoroutines; i++ {
		tracker.Go("shutdown-worker", func(ctx context.Context) {
			<-ctx.Done()
			exited.Add(1)
		})
	}

	// Give goroutines time to start and block on ctx.Done().
	time.Sleep(50 * time.Millisecond)

	if c := tracker.Count(); c != numGoroutines {
		t.Fatalf("expected %d tracked before shutdown, got %d", numGoroutines, c)
	}

	if err := tracker.Shutdown(1 * time.Second); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	if n := exited.Load(); n != numGoroutines {
		t.Errorf("expected %d goroutines to exit, got %d", numGoroutines, n)
	}
	if c := tracker.Count(); c != 0 {
		t.Errorf("expected 0 tracked after shutdown, got %d", c)
	}
}

func TestGoroutineTracker_WaitTimeout(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{MaxGoroutines: 10000})

	neverDone := make(chan struct{})
	defer close(neverDone) // clean up after test

	tracker.Go("stuck", func(ctx context.Context) {
		// Ignore ctx.Done(): this goroutine never exits on its own.
		<-neverDone
	})

	// The goroutine will never complete, so Wait must time out.
	err := tracker.Wait(50 * time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error from Wait, got nil")
	}

	// Tracker should still be usable after a timeout.
	if c := tracker.Count(); c != 1 {
		t.Errorf("expected 1 tracked after timeout, got %d", c)
	}

	// Cleanup: close neverDone (via defer) then wait for the goroutine.
	// Already deferred close(neverDone).
	tracker.Wait(1 * time.Second) // should succeed now
}

func TestGoroutineTracker_MultiplePanics(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{LeakThreshold: 5 * time.Second, MaxGoroutines: 10000})
	t.Cleanup(func() { tracker.Shutdown(1 * time.Second) })

	var successCount atomic.Int32
	const panicCount = 10

	for i := 0; i < panicCount; i++ {
		tracker.Go("panicking", func(ctx context.Context) {
			panic("deliberate test panic")
		})
	}

	// Verify tracker is still alive by spawning a non-panicking goroutine.
	tracker.Go("after-panics", func(ctx context.Context) {
		successCount.Add(1)
	})

	// All goroutines (including the panicked ones) should eventually be
	// cleaned up from the entries map.
	if err := tracker.Wait(2 * time.Second); err != nil {
		t.Fatalf("wait error (tracker may not have recovered from panics): %v", err)
	}

	if n := successCount.Load(); n != 1 {
		t.Errorf("expected 1 successful goroutine after panics, got %d", n)
	}
	if c := tracker.Count(); c != 0 {
		t.Errorf("expected 0 tracked after panics, got %d", c)
	}
}

func TestGoroutineTracker_SnapshotWhileRunning(t *testing.T) {
	tracker := NewGoroutineTracker(TrackerConfig{MaxGoroutines: 10000})
	t.Cleanup(func() { tracker.Shutdown(1 * time.Second) })

	startGate := make(chan struct{})

	// Spawn goroutines with distinct names to verify BySource breakdown.
	tracker.Go("task-alpha", func(ctx context.Context) { <-startGate })
	tracker.Go("task-alpha", func(ctx context.Context) { <-startGate })
	tracker.Go("task-alpha", func(ctx context.Context) { <-startGate })
	tracker.Go("task-alpha", func(ctx context.Context) { <-startGate })
	tracker.Go("task-alpha", func(ctx context.Context) { <-startGate })

	tracker.Go("task-beta", func(ctx context.Context) { <-startGate })
	tracker.Go("task-beta", func(ctx context.Context) { <-startGate })
	tracker.Go("task-beta", func(ctx context.Context) { <-startGate })

	tracker.Go("task-gamma", func(ctx context.Context) { <-startGate })

	time.Sleep(20 * time.Millisecond)

	snap := tracker.Snapshot()

	if snap.Total != 9 {
		t.Errorf("expected 9 total tracked, got %d", snap.Total)
	}
	if snap.BySource["task-alpha"] != 5 {
		t.Errorf("expected 5 task-alpha, got %d", snap.BySource["task-alpha"])
	}
	if snap.BySource["task-beta"] != 3 {
		t.Errorf("expected 3 task-beta, got %d", snap.BySource["task-beta"])
	}
	if snap.BySource["task-gamma"] != 1 {
		t.Errorf("expected 1 task-gamma, got %d", snap.BySource["task-gamma"])
	}
	if snap.RuntimeTotal == 0 {
		t.Error("expected non-zero runtime goroutine count")
	}
	if snap.MaxGoroutines != 10000 {
		t.Errorf("expected MaxGoroutines 10000, got %d", snap.MaxGoroutines)
	}

	// Release all goroutines and verify snapshot reflects completion.
	close(startGate)
	if err := tracker.Wait(2 * time.Second); err != nil {
		t.Fatalf("wait error after release: %v", err)
	}

	snapAfter := tracker.Snapshot()
	if snapAfter.Total != 0 {
		t.Errorf("expected 0 tracked after release, got %d", snapAfter.Total)
	}
}

func TestGoroutineTracker_LeakDetection(t *testing.T) {
	// Capture slog output at WARN level.
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))

	// Use a very short leak threshold so we can trigger detection quickly.
	tracker := NewGoroutineTracker(TrackerConfig{
		LeakThreshold: 1 * time.Millisecond,
		MaxGoroutines: 50000,
	})
	defer tracker.Shutdown(1 * time.Second)

	blockForever := make(chan struct{})
	defer close(blockForever)

	tracker.Go("leaky-goroutine", func(ctx context.Context) {
		<-blockForever
	})

	// Let the goroutine age past the leak threshold.
	time.Sleep(10 * time.Millisecond)

	// Trigger leak detection directly (we are in package engine so
	// unexported methods are accessible).
	tracker.checkLeaks()

	output := buf.String()
	if output == "" {
		t.Fatal("expected leak warning in slog output, got empty output")
	}
	if !bytes.Contains([]byte(output), []byte("goroutine may be leaking")) {
		t.Errorf("expected 'goroutine may be leaking' in log output, got: %s", output)
	}
	if !bytes.Contains([]byte(output), []byte("leaky-goroutine")) {
		t.Errorf("expected goroutine name in log output, got: %s", output)
	}

	// Clean up: close blockForever (deferred) and wait.
	tracker.Wait(1 * time.Second)
}
