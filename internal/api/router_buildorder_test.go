package api

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readSourceFile returns the full text of a source file relative to this
// test package's directory.
func readSourceFile(t *testing.T, relPath string) string {
	t.Helper()

	_, thisFile, _, _ := runtime.Caller(0)
	srcDir := filepath.Dir(thisFile)
	path := filepath.Join(srcDir, relPath)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s: %v", relPath, err)
	}
	return string(data)
}

// Admin router options live in one adminBaseOpts closure used by both the
// initial construction and the hot-reload callback. Verifies the closure is
// referenced in both call sites, guaranteeing option parity by construction.
func TestRouter_AdminBuildOrderConsistentAcrossPasses(t *testing.T) {
	runtimeSrc := readSourceFile(t, "../../pkg/runtime/runtime.go")
	lines := strings.Split(runtimeSrc, "\n")

	beforeCount := 0
	afterCount := 0
	inBefore := true

	for _, line := range lines {
		if strings.Contains(line, "hot-reload: rebuilding routers") {
			inBefore = false
		}
		if strings.Contains(line, "adminBaseOpts(") {
			if inBefore {
				beforeCount++
			} else {
				afterCount++
			}
		}
	}

	if beforeCount < 1 {
		t.Error("adminBaseOpts not used in the initial admin router build")
	}
	if afterCount < 1 {
		t.Error("adminBaseOpts not used in the hot-reload rebuild callback")
	}

	t.Logf("adminBaseOpts: %d uses before hot-reload, %d uses after (both must be >=1)",
		beforeCount, afterCount)
}

func TestRouter_AdminBuildOrder_BothBuildsCompile(t *testing.T) {
	runtimeSrc := readSourceFile(t, "../../pkg/runtime/runtime.go")

	count := strings.Count(runtimeSrc, "NewAdminRouter(")
	if count < 2 {
		t.Errorf(
			"runtime.go has %d NewAdminRouter calls, want >= 2 (initial build + hot-reload rebuild)",
			count,
		)
	}
}

// Every router build passes a lifetime, and the hot reload takes a fresh one
// per rebuild. The schema cache's poller runs until its context ends, so a
// build that passed none would leave a poller behind on every plugin route
// change.
func TestRouter_EveryBuildPassesALifetime(t *testing.T) {
	runtimeSrc := readSourceFile(t, "../../pkg/runtime/runtime.go")

	builds := strings.Count(runtimeSrc, "NewAdminRouter(") + strings.Count(runtimeSrc, "NewAPIRouter(")
	lifetimes := strings.Count(runtimeSrc, "api.WithLifetime(")
	if lifetimes != builds {
		t.Errorf("runtime.go builds %d routers and passes %d lifetimes; every build takes one", builds, lifetimes)
	}
	if !strings.Contains(runtimeSrc, "newRouterLifetime()") {
		t.Error("runtime.go does not end the previous build's background work")
	}
}

// The default is the process: a caller that builds one router for the life
// of the program says nothing.
func TestRouterOptions_LifetimeDefaultsToTheProcess(t *testing.T) {
	if got := (&routerOptions{}).lifetimeCtx(); got != context.Background() {
		t.Errorf("default lifetime is %v, want context.Background()", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := &routerOptions{}
	WithLifetime(ctx)(o)
	if o.lifetimeCtx() != ctx {
		t.Error("WithLifetime did not set the router's lifetime")
	}
}
