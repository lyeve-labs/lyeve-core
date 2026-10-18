package engine

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/panicreport"
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

// catchPanicReports installs a reporter for the test and returns the reports
// from the goroutine named name. Other tests in the package panic in
// parallel, so their reports are dropped.
func catchPanicReports(t *testing.T, name string) <-chan panicreport.Panic {
	t.Helper()
	ch := make(chan panicreport.Panic, 1)
	panicreport.Set(func(p panicreport.Panic) {
		if p.Goroutine == name {
			ch <- p
		}
	})
	t.Cleanup(func() { panicreport.Set(nil) })
	return ch
}

func TestGoSafe_ReportsTheRecoveredPanic(t *testing.T) {
	reports := catchPanicReports(t, "gosafe-report")

	GoSafe("gosafe-report", func() { panic("gosafe boom") })

	select {
	case got := <-reports:
		if got.TenantID != "" || got.Route != "" || got.Value != "gosafe boom" {
			t.Fatalf("report = %+v, want no tenant and the panic value", got)
		}
		if !strings.Contains(string(got.Stack), "TestGoSafe_ReportsTheRecoveredPanic") {
			t.Fatalf("stack does not reach the panicking function:\n%s", got.Stack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GoSafe recovered the panic but never reported it")
	}
}
