package engine_test

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/lyeve-labs/lyeve-core/internal/leakcheck"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The leak profile reports only goroutines parked on something nothing can
// reach. This package's loops park on tickers and on channels their owner
// still holds, which stay reachable through the runtime's timers, so a pool,
// tracker or executor a test never stops is invisible to it. goleak catches
// those by listing every goroutine still running once the tests end.
func TestMain(m *testing.M) {
	code := leakcheck.Main(m)
	if code == 0 {
		// On MySQL and SQL Server the shared test databases are prepared in
		// the background, and a preparation can still be running when the
		// tests end. Its connections are not leaks, so wait for it.
		testdb.StopPreparation()
		err := goleak.Find(
			// Shared test databases: their containers and the preparation
			// loops, now parked, outlive any one test by design.
			goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
			goleak.IgnoreAnyFunction("github.com/lyeve-labs/lyeve-core/internal/testdb.(*readyQueue).start.func1.1"),
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}
