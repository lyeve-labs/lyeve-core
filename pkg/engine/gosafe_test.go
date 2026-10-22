package engine

import (
	"context"
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
// parallel, so their reports are dropped, and so is a report that finds the
// channel full, which would otherwise block the goroutine reporting it.
func catchPanicReports(t *testing.T, name string) <-chan panicreport.Panic {
	t.Helper()
	ch := make(chan panicreport.Panic, 8)
	panicreport.Set(func(p panicreport.Panic) {
		if p.Goroutine != name {
			return
		}
		select {
		case ch <- p:
		default:
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

// awaitPanicReport returns the first report carrying value, or fails the test
// once two seconds pass without one.
func awaitPanicReport(t *testing.T, reports <-chan panicreport.Panic, value any) panicreport.Panic {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case got := <-reports:
			if got.Value == value {
				return got
			}
		case <-timeout:
			t.Fatalf("the panic %v was recovered but never reported", value)
		}
	}
}

// The pool recovers a task's panic inside its own worker, so GoSafe never
// sees it. Cron jobs, flow runs and webhook deliveries run there.
func TestWorkerPool_ReportsTheRecoveredPanic(t *testing.T) {
	reports := catchPanicReports(t, "workerpool")
	pool := NewWorkerPool(WorkerPoolConfig{Size: 2})
	t.Cleanup(pool.Shutdown)

	pool.Submit(context.Background(), func(context.Context) error { panic("pool boom") })

	got := awaitPanicReport(t, reports, "pool boom")
	if !strings.Contains(string(got.Stack), "TestWorkerPool_ReportsTheRecoveredPanic") {
		t.Fatalf("stack does not reach the panicking task:\n%s", got.Stack)
	}
}

// A ParallelEngine task recovers its own panic, ahead of the GoSafe around it.
func TestParallelEngine_ReportsTheRecoveredPanic(t *testing.T) {
	e := NewParallelEngine(ParallelConfig{MaxConcurrent: 2})

	t.Run("FanOut", func(t *testing.T) {
		reports := catchPanicReports(t, "parallel-map-task")
		_, _ = e.FanOut(context.Background(), []any{1}, func(context.Context, any, int) (any, error) { panic("map boom") })
		awaitPanicReport(t, reports, "map boom")
	})
	t.Run("FanOutVoid", func(t *testing.T) {
		reports := catchPanicReports(t, "parallel-each-task")
		e.FanOutVoid(context.Background(), []any{1}, func(context.Context, any, int) error { panic("each boom") })
		awaitPanicReport(t, reports, "each boom")
	})
}
