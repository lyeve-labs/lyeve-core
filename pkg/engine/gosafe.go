package engine

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/lyeve-labs/lyeve-core/internal/panicreport"
)

// GoSafe spawns fn in a goroutine with panic recovery. If fn panics, the
// panic is recovered, logged with slog.Error and handed to the panic
// reporter, and the goroutine exits cleanly: the calling process is never
// crashed.
//
// The name parameter identifies the goroutine source in log output.
func GoSafe(name string, fn func()) {
	// lyeve:bare-goroutine this is the recovering spawn every other one goes through
	go func() {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				slog.Error("GoSafe recovered panic",
					"name", name,
					"panic", fmt.Sprintf("%v", r),
					"stack", string(stack),
				)
				panicreport.Report(panicreport.Panic{Goroutine: name, Value: r, Stack: stack})
			}
		}()
		fn()
	}()
}
