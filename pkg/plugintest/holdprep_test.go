package plugintest

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Once a test holds preparation, the harness starts nothing more while it
// runs, so a leak check that takes its baseline after the hold counts only
// what the plugin started.
func TestHoldDatabasePreparation_StartsNoHarnessGoroutine(t *testing.T) {
	ForEachDialect(t, func(t *testing.T, _ core.Host) {
		HoldDatabasePreparation(t)
		before := goroutineIDs()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for id, stack := range goroutineStacks() {
				if !before[id] {
					t.Fatalf("a goroutine started while preparation was held:\n%s", stack)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

func goroutineIDs() map[string]bool {
	ids := map[string]bool{}
	for id := range goroutineStacks() {
		ids[id] = true
	}
	return ids
}

// goroutineStacks returns the stack of every goroutine, keyed by its id.
func goroutineStacks() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	stacks := map[string]string{}
	for _, stack := range strings.Split(string(buf), "\n\n") {
		id, _, _ := strings.Cut(strings.TrimPrefix(stack, "goroutine "), " ")
		stacks[id] = stack
	}
	return stacks
}
