package hotreload

import (
	"context"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// Activator is the subset of the activator API used by the hot-reload system.
// Avoids importing the concrete type to keep the dependency edge clean.
type Activator interface {
	ReloadPlugin(ctx context.Context, name string) ([]plugin.RouteDecl, error)
	SetRoutesChangeCallback(func([]plugin.PluginRoutes))
}

// Config controls how the hot-reload watcher behaves.
type Config struct {
	// Root is the directory PluginsDir is resolved against. Defaults to the
	// nearest directory above the working directory that holds a go.work
	// file.
	Root string

	// PluginsDir is a glob of plugin source directories under Root. Each
	// match is treated as a separate plugin.
	PluginsDir string

	// Debounce is the quiet period required after the last file change
	// before triggering a rebuild. Default: 300ms.
	Debounce time.Duration

	// RebuildTimeout is the max time allowed for `go build -buildmode=plugin`.
	// Default: 30s.
	RebuildTimeout time.Duration

	// TmpDir is where .so files are written. Default: os.TempDir()/lyeve-dev-plugins.
	TmpDir string
}
