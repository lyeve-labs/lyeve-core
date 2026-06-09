//go:build dev

package hotreload

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// applyDefaults

func TestApplyDefaults_AllFields(t *testing.T) {
	root, pDir, debounce, rTimeout, tmpDir := applyDefaults(Config{})

	if root == "" {
		t.Error("applyDefaults: expected non-empty root")
	}
	if pDir != "lyeve-plugin-*/plugin/" {
		t.Errorf("applyDefaults: pluginsDir = %q, want %q", pDir, "lyeve-plugin-*/plugin/")
	}
	if debounce != 300*time.Millisecond {
		t.Errorf("applyDefaults: debounce = %v, want 300ms", debounce)
	}
	if rTimeout != 30*time.Second {
		t.Errorf("applyDefaults: rebuildTimeout = %v, want 30s", rTimeout)
	}
	if tmpDir == "" {
		t.Error("applyDefaults: expected non-empty tmpDir")
	}
}

func TestApplyDefaults_OverrideRoot(t *testing.T) {
	want := "/custom/project"
	root, _, _, _, _ := applyDefaults(Config{Root: want})
	if root != want {
		t.Errorf("applyDefaults with Root override: got %q, want %q", root, want)
	}
}

func TestApplyDefaults_OverridePluginsDir(t *testing.T) {
	want := "custom-*/src/"
	_, pDir, _, _, _ := applyDefaults(Config{PluginsDir: want})
	if pDir != want {
		t.Errorf("applyDefaults with PluginsDir override: got %q, want %q", pDir, want)
	}
}

func TestApplyDefaults_OverrideDebounce(t *testing.T) {
	want := 500 * time.Millisecond
	_, _, d, _, _ := applyDefaults(Config{Debounce: want})
	if d != want {
		t.Errorf("applyDefaults with Debounce override: got %v, want %v", d, want)
	}
}

func TestApplyDefaults_OverrideRebuildTimeout(t *testing.T) {
	want := 60 * time.Second
	_, _, _, rt, _ := applyDefaults(Config{RebuildTimeout: want})
	if rt != want {
		t.Errorf("applyDefaults with RebuildTimeout override: got %v, want %v", rt, want)
	}
}

func TestApplyDefaults_OverrideTmpDir(t *testing.T) {
	want := "/tmp/custom"
	_, _, _, _, td := applyDefaults(Config{TmpDir: want})
	if td != want {
		t.Errorf("applyDefaults with TmpDir override: got %q, want %q", td, want)
	}
}

func TestApplyDefaults_ZeroDebounceDefaults(t *testing.T) {
	_, _, d, _, _ := applyDefaults(Config{Debounce: 0})
	if d != 300*time.Millisecond {
		t.Errorf("zero debounce should default to 300ms, got %v", d)
	}
}

func TestApplyDefaults_NegativeDebounceDefaults(t *testing.T) {
	_, _, d, _, _ := applyDefaults(Config{Debounce: -1 * time.Second})
	if d != 300*time.Millisecond {
		t.Errorf("negative debounce should default to 300ms, got %v", d)
	}
}

func TestApplyDefaults_ZeroRebuildTimeoutDefaults(t *testing.T) {
	_, _, _, rt, _ := applyDefaults(Config{RebuildTimeout: 0})
	if rt != 30*time.Second {
		t.Errorf("zero rebuildTimeout should default to 30s, got %v", rt)
	}
}

// resolvePluginDirs (table-driven)

func TestResolvePluginDirs(t *testing.T) {
	tests := []struct {
		name      string
		setupRoot func() string
		pattern   string
		wantCount int
		wantErr   string // substring expected in error, empty means no error
	}{
		{
			name:      "non-existent root returns empty",
			setupRoot: func() string { return "/nonexistent/XXXX" },
			pattern:   "lyeve-plugin-*/plugin/",
			wantCount: 0,
			wantErr:   "",
		},
		{
			name:      "no matching pattern returns empty",
			setupRoot: func() string { return t.TempDir() },
			pattern:   "does-not-match-*/",
			wantCount: 0,
			wantErr:   "",
		},
		{
			name: "finds matching plugin dirs",
			setupRoot: func() string {
				root := t.TempDir()
				for _, name := range []string{"lyeve-plugin-foo", "lyeve-plugin-bar"} {
					dir := filepath.Join(root, name, "plugin")
					if err := os.MkdirAll(dir, 0755); err != nil {
						t.Fatal(err)
					}
				}
				// File with matching name to test directory filtering.
				file := filepath.Join(root, "lyeve-plugin-notadir", "plugin")
				if err := os.MkdirAll(root+"/lyeve-plugin-notadir", 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("not a dir"), 0644); err != nil {
					t.Fatal(err)
				}
				return root
			},
			pattern:   "lyeve-plugin-*/plugin/",
			wantCount: 2,
			wantErr:   "",
		},
		{
			name:      "malformed glob pattern errors",
			setupRoot: func() string { return t.TempDir() },
			pattern:   "[",
			wantCount: 0,
			wantErr:   "syntax error in pattern",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.setupRoot()
			dirs, err := resolvePluginDirs(root, tt.pattern)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(dirs) != tt.wantCount {
				t.Errorf("len(dirs) = %d, want %d", len(dirs), tt.wantCount)
			}
			for _, d := range dirs {
				if !filepath.IsAbs(d) {
					t.Errorf("expected absolute path, got %q", d)
				}
			}
		})
	}
}

// pluginNameFromPath (table-driven)

func TestPluginNameFromPath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		watchedDirs []string
		wantName    string
		wantDir     string
	}{
		{
			name:        "matches plugin dir prefix",
			path:        "/root/lyeve-plugin-foo/plugin/main.go",
			watchedDirs: []string{"/root/lyeve-plugin-foo/plugin", "/root/lyeve-plugin-bar/plugin"},
			wantName:    "lyeve-plugin-foo",
			wantDir:     "/root/lyeve-plugin-foo/plugin",
		},
		{
			name:        "no match returns empty",
			path:        "/some/other/path/main.go",
			watchedDirs: []string{"/root/lyeve-plugin-foo/plugin"},
			wantName:    "",
			wantDir:     "",
		},
		{
			name:        "partial path prefix not enough",
			path:        "/root/lyeve-plugin-foo/main.go",
			watchedDirs: []string{"/root/lyeve-plugin-foo/plugin"},
			wantName:    "",
			wantDir:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotDir := pluginNameFromPath(tt.path, tt.watchedDirs)
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
			if gotDir != tt.wantDir {
				t.Errorf("dir = %q, want %q", gotDir, tt.wantDir)
			}
		})
	}
}

// findPluginDir (table-driven)

func TestFindPluginDir(t *testing.T) {
	tests := []struct {
		name   string
		dirs   []string
		plugin string
		want   string
	}{
		{
			name:   "found in list",
			dirs:   []string{"/root/lyeve-plugin-foo/plugin", "/root/lyeve-plugin-bar/plugin"},
			plugin: "lyeve-plugin-bar",
			want:   "/root/lyeve-plugin-bar/plugin",
		},
		{
			name:   "not found returns empty",
			dirs:   []string{"/root/lyeve-plugin-foo/plugin"},
			plugin: "nonexistent",
			want:   "",
		},
		{
			name:   "empty dirs list returns empty",
			dirs:   nil,
			plugin: "test",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findPluginDir(tt.dirs, tt.plugin)
			if got != tt.want {
				t.Errorf("findPluginDir(%v, %q) = %q, want %q", tt.dirs, tt.plugin, got, tt.want)
			}
		})
	}
}

// findModuleRoot (table-driven)

func TestFindModuleRoot(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T) string
		wantSuffix string
		wantEmpty  bool
	}{
		{
			name: "finds go.mod in parent dir",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				pluginDir := filepath.Join(root, "lyeve-plugin-foo", "plugin")
				if err := os.MkdirAll(pluginDir, 0755); err != nil {
					t.Fatal(err)
				}
				gomod := filepath.Join(root, "lyeve-plugin-foo", "go.mod")
				if err := os.WriteFile(gomod, []byte("module test\n"), 0644); err != nil {
					t.Fatal(err)
				}
				return pluginDir
			},
			wantSuffix: "lyeve-plugin-foo",
		},
		{
			name: "no go.mod anywhere returns empty",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				pluginDir := filepath.Join(root, "lyeve-plugin-foo", "plugin")
				if err := os.MkdirAll(pluginDir, 0755); err != nil {
					t.Fatal(err)
				}
				return pluginDir
			},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srcDir := tt.setup(t)
			got := findModuleRoot(srcDir)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("findModuleRoot(%q) = %q, want empty", srcDir, got)
				}
				return
			}
			if got == "" {
				t.Fatal("expected non-empty, got empty")
			}
			if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("findModuleRoot(%q) = %q, want suffix %q", srcDir, got, tt.wantSuffix)
			}
		})
	}
}

// findProjectRoot

// The walk stops at the nearest go.work above the working directory. The test
// builds its own tree, so it does not depend on where the checkout sits.
func TestFindProjectRoot_FindsTheNearestGoWork(t *testing.T) {
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chdir(origDir)
	}()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}

	// The temp dir may sit behind a symlink, so compare resolved paths.
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(findProjectRoot())
	if err != nil {
		t.Fatalf("findProjectRoot from %s returned a path that does not resolve: %v", nested, err)
	}
	if got != want {
		t.Errorf("findProjectRoot from %s = %q, want %q", nested, got, want)
	}
}

func TestFindProjectRoot_NotFound(t *testing.T) {
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Chdir(origDir)
	}()

	isolated := t.TempDir()
	if err := os.Chdir(isolated); err != nil {
		t.Fatal(err)
	}

	root := findProjectRoot()
	if root != "" {
		t.Errorf("findProjectRoot from isolated dir = %q, want empty", root)
	}
}

// WatchHotReload error paths

type noopActivator struct{}

func (n *noopActivator) ReloadPlugin(ctx context.Context, name string) ([]plugin.RouteDecl, error) {
	return nil, nil
}
func (n *noopActivator) SetRoutesChangeCallback(f func([]plugin.PluginRoutes)) {}

func TestWatchHotReload_NilActivator(t *testing.T) {
	err := WatchHotReload(context.Background(), nil, Config{})
	if err == nil {
		t.Fatal("expected error for nil activator")
	}
}

func TestWatchHotReload_BadRootPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := WatchHotReload(ctx, &noopActivator{}, Config{
		Root:       "/nonexistent/XXXX/deep/invalid",
		PluginsDir: "*/plugin/",
	})
	if err == nil {
		t.Fatal("expected error for unreachable root with non-matching glob")
	}
}

func TestWatchHotReload_NoPluginDirs(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := WatchHotReload(ctx, &noopActivator{}, Config{
		Root:       root,
		PluginsDir: "lyeve-plugin-*/plugin/",
	})
	if err == nil {
		t.Fatal("expected error for no matching plugin directories")
	}
}

func TestWatchHotReload_MkdirAllError(t *testing.T) {
	root := t.TempDir()

	// Create a plugin dir so the function gets past the "no plugin dirs" check.
	pluginDir := filepath.Join(root, "lyeve-plugin-test", "plugin")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a TmpDir that will fail MkdirAll. /dev/null is a file, so MkdirAll
	// under it will fail with ENOTDIR.
	err := WatchHotReload(ctx, &noopActivator{}, Config{
		Root:       root,
		PluginsDir: "lyeve-plugin-*/plugin/",
		TmpDir:     "/dev/null/lyeve-dev-test",
	})
	if err == nil {
		t.Fatal("expected error for MkdirAll failure on /dev/null path")
	}
	if !strings.Contains(err.Error(), "create tmp dir") {
		t.Errorf("error = %q, want substring %q", err.Error(), "create tmp dir")
	}
}

// rebuildAndSwap error paths

func TestRebuildAndSwap_ModRootNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "nonexistent", "plugin")

	err := rebuildAndSwap(
		context.Background(),
		slog.Default(),
		&noopActivator{},
		"test-plugin",
		srcDir,
		tmpDir,
		time.Second,
	)
	if err == nil {
		t.Fatal("expected error for srcDir with no go.mod parent, got nil")
	}
	if !strings.Contains(err.Error(), "cannot find go.mod") {
		t.Errorf("error = %q, want substring %q", err.Error(), "cannot find go.mod")
	}
}

func TestWatchHotReload_CanceledContext(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "lyeve-plugin-test", "plugin")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	err := WatchHotReload(ctx, &noopActivator{}, Config{
		Root:       root,
		PluginsDir: "lyeve-plugin-*/plugin/",
	})
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}
