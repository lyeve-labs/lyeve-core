//go:build !dev

package hotreload

import (
	"context"
	"testing"
)

func TestWatchHotReload_NoopBuild(t *testing.T) {
	err := WatchHotReload(context.Background(), nil, Config{})
	if err != nil {
		t.Fatalf("WatchHotReload noop should return nil, got: %v", err)
	}
}
