package runtime

import (
	"os"
	"strings"
	"testing"
)

// TestWithKeyStore_InAllRouterBuilds verifies that api.WithKeyStore(encKeyStore)
// is wired in BOTH the admin router option builder (adminBaseOpts) AND the
// API router option builder (apiBaseOpts). Both closures are used in the
// initial construction AND the hot-reload callback, so the key store is
// wired in all 4 router builds through shared closures.
//
// Both router builds must carry the key store. The test reads runtime.go and
// counts the call sites.
func TestWithKeyStore_InAllRouterBuilds(t *testing.T) {
	// Read runtime.go from source: we need the raw file, not compiled-in
	// because we're checking the source contains the call in both closures.
	data, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	src := string(data)

	const withKeyStoreCall = "api.WithKeyStore(encKeyStore)"
	count := strings.Count(src, withKeyStoreCall)
	// WithKeyStore lives in the adminBaseOpts and apiBaseOpts closures
	// (2 occurrences), which are reused by both the initial construction and
	// the hot-reload callback.
	if count < 2 {
		t.Errorf(
			"api.WithKeyStore(encKeyStore) appears %d times in runtime.go, want >= 2.\n"+
				"Expected: adminBaseOpts closure + apiBaseOpts closure. "+
				"These closures are used by both initial construction and hot-reload paths.",
			count,
		)
	}

	// Also verify it appears before the hot-reload section: the closures
	// must be defined before the hot-reload callback.
	lines := strings.Split(src, "\n")
	beforeHotReload := 0
	for _, line := range lines {
		if strings.Contains(line, "hot-reload: rebuilding") {
			break
		}
		if strings.Contains(line, withKeyStoreCall) {
			beforeHotReload++
		}
	}
	if beforeHotReload < 2 {
		t.Errorf(
			"api.WithKeyStore(encKeyStore) appears %d times before hot-reload section, want >= 2.\n"+
				"The initial admin and API routers would omit WithKeyStore.",
			beforeHotReload,
		)
	}
}

// TestNewAdminRouter_ReceivesKeyStoreOption verifies api.WithKeyStore is
// wired in runtime.go by scanning the source file for the call site.
func TestNewAdminRouter_ReceivesKeyStoreOption(t *testing.T) {
	// Source-level assertion: runtime.go must reference api.WithKeyStore.
	// The call must appear in the option closures.
	const want = `api.WithKeyStore`
	data, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("runtime.go does not contain %q - WithKeyStore option is not wired", want)
	}
}
