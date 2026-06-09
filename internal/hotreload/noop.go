//go:build !dev

// Package hotreload keeps all real functionality behind the "dev" build tag.
// When the dev tag is absent, this package is a no-op, so production binaries
// don't carry the fsnotify dependency or the Go plugin loader.
package hotreload

import "context"

// WatchHotReload is a no-op when the dev build tag is not set.
func WatchHotReload(_ context.Context, _ Activator, _ Config) error { return nil }
