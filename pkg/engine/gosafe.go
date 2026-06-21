package engine

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// GoSafe spawns fn in a goroutine with panic recovery. If fn panics, the
// panic is recovered, logged with slog.Error, and the goroutine exits
// cleanly: the calling process is never crashed.
//
// The name parameter identifies the goroutine source in log output.
func GoSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("GoSafe recovered panic",
					"name", name,
					"panic", fmt.Sprintf("%v", r),
					"stack", string(debug.Stack()),
				)
			}
		}()
		fn()
	}()
}
