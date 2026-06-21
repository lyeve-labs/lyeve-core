package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestGoSafe_NormalFn(t *testing.T) {
	t.Parallel()

	var ran atomic.Bool
	GoSafe("test-normal", func() {
		ran.Store(true)
	})

	// Give the goroutine time to run
	time.Sleep(50 * time.Millisecond)

	if !ran.Load() {
		t.Fatal("GoSafe: normal function did not execute")
	}
}

func TestGoSafe_PanicRecovered(t *testing.T) {
	t.Parallel()

	// A panic inside GoSafe should be recovered without crashing the process.
	// Verify by running a panicking function and ensuring the test continues.
	done := make(chan struct{})
	GoSafe("test-panic", func() {
		close(done)
		panic("deliberate panic for test")
	})

	select {
	case <-done:
		// The function started: good.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("GoSafe: goroutine did not start")
	}

	// If we reach here, the panic was recovered and the test process survived.
	time.Sleep(50 * time.Millisecond)
}

func TestGoSafe_MultipleGoroutines(t *testing.T) {
	t.Parallel()

	const numGoroutines = 20
	var ran atomic.Int32

	for i := range numGoroutines {
		GoSafe("test-multi", func() {
			ran.Add(1)
			_ = i
		})
	}

	time.Sleep(200 * time.Millisecond)

	if got := ran.Load(); got != numGoroutines {
		t.Fatalf("GoSafe: expected %d goroutines to run, got %d", numGoroutines, got)
	}
}
