package hotreload

import (
	"context"
	"testing"
)

// TestWatchHotReload_SignatureParity verifies that WatchHotReload has the
// same signature across the dev and !dev build-tag variants. This file has
// no build tag, so it compiles under both configurations: if the signatures
// were mismatched, this file would fail to compile under one of them.
func TestWatchHotReload_SignatureParity(t *testing.T) {
	err := WatchHotReload(context.Background(), nil, Config{})
	// Under !dev: the no-op returns nil.
	// Under dev: nil activator triggers the "activator must be non-nil" error.
	// Both are expected: the test just proves the signature compiles.
	if err == nil {
		return // no-op path (production build)
	}
	// Dev path: the only error from a nil activator+default config is
	// "activator must be non-nil".
	if err.Error() == "dev.WatchHotReload: activator must be non-nil" {
		return
	}
	t.Fatalf("unexpected error: %v", err)
}
