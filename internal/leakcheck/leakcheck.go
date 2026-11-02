// Package leakcheck fails a test binary that leaves a goroutine leaked.
package leakcheck

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"testing"
)

// Main runs the package's tests and then fails the binary when the runtime
// reports a leaked goroutine. Call it from TestMain as os.Exit(leakcheck.Main(m)).
//
// A leaked goroutine here is one parked on a channel or a lock that nothing
// outside the leaked goroutines can reach, so nothing can ever wake it. The
// runtime finds them during a collection that the goroutineleak profile
// starts, which costs about one collection and reports no false positives.
// A goroutine parked on something still reachable, such as a ticker, is not
// reported.
func Main(m *testing.M) int {
	code := m.Run()
	n, err := Find(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "leakcheck: %v\n", err)
		return 1
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "leakcheck: leaked goroutines: %d\n", n)
		return 1
	}
	return code
}

// Find runs the leak detection and returns how many goroutines it reported.
// When there are any, it writes their stacks to w.
func Find(w io.Writer) (int, error) {
	return find(pprof.Lookup("goroutineleak"), w)
}

// profile is the part of a pprof profile that find reads.
type profile interface {
	WriteTo(w io.Writer, debug int) error
	Count() int
}

// find reads p, and fails when it cannot be written, because a check that
// read nothing has not found the leaks absent.
func find(p profile, w io.Writer) (int, error) {
	var buf bytes.Buffer
	if err := p.WriteTo(&buf, 1); err != nil {
		return 0, fmt.Errorf("write the goroutineleak profile: %w", err)
	}
	n := p.Count()
	if n > 0 {
		_, _ = w.Write(buf.Bytes())
	}
	return n, nil
}
