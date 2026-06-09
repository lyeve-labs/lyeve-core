//go:build dev

// Package hotreload provides plugin hot-reload infrastructure for development.
// Activated by building with: go build -tags dev
//
// WatchHotReload runs a fsnotify watcher that detects changes to plugin
// source files, rebuilds the affected plugin as a Go .so, loads it via the
// Go plugin package, swaps the factory into the core registry, and tells
// the activator to stop the old instance and start the new one.
//
// The route-change callback is wired to a SwappableHandler so the chi
// router rebuilds without dropping in-flight requests.
package hotreload

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"plugin"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	// Aliased: the stdlib "plugin" above owns the unqualified name here, and
	// this package is only needed for the factory registry.
	pluginreg "github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// WatchHotReload runs the fsnotify-powered plugin hot-reload loop.
// Blocks until ctx is canceled. Call once from runtime under the "dev" build tag.
func WatchHotReload(ctx context.Context, activator Activator, cfg Config) error {
	if activator == nil {
		return fmt.Errorf("dev.WatchHotReload: activator must be non-nil")
	}

	root, pluginsDir, debounce, rebuildTimeout, tmpDir := applyDefaults(cfg)

	logger := slog.Default().With("component", "dev/hot-reload")

	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return fmt.Errorf("create tmp dir %s: %w", tmpDir, err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	defer watcher.Close()

	pluginDirs, err := resolvePluginDirs(root, pluginsDir)
	if err != nil {
		return fmt.Errorf("resolve plugin dirs: %w", err)
	}
	if len(pluginDirs) == 0 {
		return fmt.Errorf("no plugin directories found matching %s in %s", pluginsDir, root)
	}

	logger.Info("hot-reload: watching plugin source dirs",
		"root", root,
		"pattern", pluginsDir,
		"dirs", len(pluginDirs),
		"plugin_dirs", pluginDirs,
	)

	for _, d := range pluginDirs {
		if err := watcher.Add(d); err != nil {
			return fmt.Errorf("watch %s: %w", d, err)
		}
	}

	// Event loop
	type pending struct {
		name    string // plugin name
		dir     string // watched dir
		lastHit time.Time
	}

	pendingPlugins := sync.Map{} // plugin name -> *pending

	trigger := make(chan string, 10) // carries plugin name

	// Debounce goroutine: when a plugin has been quiet for debounce, fire it.
	go func() {
		ticker := time.NewTicker(debounce / 4)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				pendingPlugins.Range(func(key, value any) bool {
					p := value.(*pending)
					if now.Sub(p.lastHit) >= debounce {
						pendingPlugins.Delete(key)
						select {
						case trigger <- p.name:
						default:
						}
					}
					return true
				})
			}
		}
	}()

	// Rebuild goroutine: drains trigger channel and rebuilds.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case name := <-trigger:
				dir := findPluginDir(pluginDirs, name)
				if dir == "" {
					logger.Error("hot-reload: watched plugin dir not found", "plugin", name)
					continue
				}
				logger.Info("hot-reload: rebuilding plugin", "plugin", name)
				if err := rebuildAndSwap(ctx, logger, activator, name, dir, tmpDir, rebuildTimeout); err != nil {
					logger.Error("hot-reload: reload failed", "plugin", name, "err", err)
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !strings.HasSuffix(evt.Name, ".go") {
				continue
			}
			if strings.HasSuffix(evt.Name, "_test.go") {
				continue
			}
			name, dir := pluginNameFromPath(evt.Name, pluginDirs)
			if name == "" {
				continue
			}

			p, _ := pendingPlugins.LoadOrStore(name, &pending{name: name, dir: dir})
			pp := p.(*pending)
			pp.lastHit = time.Now()
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			logger.Error("fsnotify error", "err", err)
		}
	}
}

// rebuildAndSwap compiles the plugin as a .so, loads it, swaps the factory
// in the core registry, and triggers an activator reload.
func rebuildAndSwap(
	ctx context.Context,
	logger *slog.Logger,
	activator Activator,
	name, srcDir, tmpDir string,
	rebuildTimeout time.Duration,
) error {
	soPath := filepath.Join(tmpDir, name+".so")

	// Remove old .so so plugin.Open doesn't reuse a stale handle.
	_ = os.Remove(soPath) // err suppressed: best-effort cleanup of temporary .so file

	buildCtx, cancel := context.WithTimeout(ctx, rebuildTimeout)
	defer cancel()

	// A plugin module's go.mod sits above its source dir: example/plugin/
	// builds from example/.
	modRoot := findModuleRoot(srcDir)
	if modRoot == "" {
		return fmt.Errorf("cannot find go.mod for %s", srcDir)
	}

	logger.Info("hot-reload: building plugin .so",
		"plugin", name,
		"mod_root", modRoot,
		"so_path", soPath,
	)

	cmd := exec.CommandContext(buildCtx,
		"go", "build",
		"-buildmode=plugin",
		"-o", soPath,
		"./plugin/",
	)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=1",     // Go plugins require cgo
		"GOFLAGS=-tags=dev", // pass dev tag through
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build plugin: %w\n%s", err, string(output))
	}

	p, err := plugin.Open(soPath)
	if err != nil {
		return fmt.Errorf("plugin.Open %s: %w", soPath, err)
	}

	sym, err := p.Lookup("New")
	if err != nil {
		return fmt.Errorf("plugin.Lookup(New) in %s: %w", soPath, err)
	}
	factory, ok := sym.(func() core.Plugin)
	if !ok {
		return fmt.Errorf("symbol New in %s has type %T, expected func() core.Plugin", soPath, sym)
	}

	wrappedFactory := func() core.Plugin { return factory() }

	old := pluginreg.ReplacePlugin(name, wrappedFactory)
	if old != nil {
		logger.Info("hot-reload: factory swapped", "plugin", name)
	} else {
		logger.Info("hot-reload: factory registered", "plugin", name)
	}

	routes, err := activator.ReloadPlugin(ctx, name)
	if err != nil {
		return fmt.Errorf("ReloadPlugin(%s): %w", name, err)
	}
	logger.Info("hot-reload: plugin reloaded",
		"plugin", name,
		"routes", len(routes),
	)

	return nil
}

// applyDefaults returns Config fields with zero-value fallbacks applied.
func applyDefaults(cfg Config) (root, pluginsDir string, debounce, rebuildTimeout time.Duration, tmpDir string) {
	root = cfg.Root
	if root == "" {
		root = findProjectRoot()
	}
	pluginsDir = cfg.PluginsDir
	if pluginsDir == "" {
		pluginsDir = "lyeve-plugin-*/plugin/"
	}
	debounce = cfg.Debounce
	if debounce <= 0 {
		debounce = 300 * time.Millisecond
	}
	rebuildTimeout = cfg.RebuildTimeout
	if rebuildTimeout <= 0 {
		rebuildTimeout = 30 * time.Second
	}
	tmpDir = cfg.TmpDir
	if tmpDir == "" {
		tmpDir = filepath.Join(os.TempDir(), "lyeve-dev-plugins")
	}
	return
}

// resolvePluginDirs expands the glob pattern under root and returns absolute
// paths for each matched plugin source directory.
func resolvePluginDirs(root, pattern string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	glob := filepath.Join(absRoot, pattern)
	matches, err := filepath.Glob(glob)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || !fi.IsDir() {
			continue
		}
		abs, err := filepath.Abs(m)
		if err != nil {
			continue
		}
		dirs = append(dirs, abs)
	}
	return dirs, nil
}

// pluginNameFromPath extracts the plugin name from a file path given the
// set of watched directories. Returns ("", "") when no watch dir matches.
func pluginNameFromPath(path string, watchedDirs []string) (name, dir string) {
	for _, d := range watchedDirs {
		if strings.HasPrefix(path, d) {
			// d is e.g. /abs/path/example/plugin/
			// Strip d -> relative path inside plugin dir.
			// Name is the last component of the parent of plugin/.
			parent := filepath.Dir(d)
			return filepath.Base(parent), d
		}
	}
	return "", ""
}

// findPluginDir returns the watched directory matching the given plugin name, or "".
func findPluginDir(dirs []string, name string) string {
	for _, d := range dirs {
		if filepath.Base(filepath.Dir(d)) == name {
			return d
		}
	}
	return ""
}

// findModuleRoot walks up from srcDir to find the nearest go.mod. Returns "" if none found.
func findModuleRoot(srcDir string) string {
	dir := filepath.Dir(srcDir) // strip "plugin/"
	for {
		gomod := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(gomod); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// findProjectRoot returns the nearest directory above cwd that holds go.work.
// It is the default Config.Root when none is specified.
func findProjectRoot() string {
	cwd, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(cwd, "go.work")); err == nil {
			return cwd
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return ""
		}
		cwd = parent
	}
}
