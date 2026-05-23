//go:build dev

package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/internal/hotreload"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// wireHotReload starts the fsnotify-based hot-reload watcher. Only compiled
// under -tags dev. Returns non-nil only on genuine startup failures.
//
// *plugin.Activator already satisfies hotreload.Activator, so the watcher takes
// it directly. Keep this signature identical to the one in dev_noop.go: the two
// are the same function under opposite build tags, and drift between them means
// one of the two builds stops compiling.
func wireHotReload(ctx context.Context, activator *plugin.Activator) error {
	if activator == nil {
		return fmt.Errorf("runtime: wireHotReload requires non-nil activator")
	}

	logger := slog.Default().With("component", "runtime/dev")
	logger.Info("dev hot-reload wired - watching plugin source files for changes")
	return hotreload.WatchHotReload(ctx, activator, hotreload.Config{})
}
