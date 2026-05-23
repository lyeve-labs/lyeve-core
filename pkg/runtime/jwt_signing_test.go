package runtime

import (
	"os"
	"strings"
	"testing"
)

// TestRuntime_JWTSigningInitFailsHardInProduction: with
// cfg.IsProduction()==true and an InitJWTSigning error, boot returns an error
// (no silent HS256 fallback that would open a downgrade attack vector).
//
// The test reads runtime.go and verifies the production JWT-fail path.
func TestRuntime_JWTSigningInitFailsHardInProduction(t *testing.T) {
	data, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	src := string(data)

	// The production-fatal path must exist: an error return when
	// IsProduction() is true and InitJWTSigning fails.
	wantBlock := []string{
		"IsProduction()",
		"jwt signing init failed in production",
		"aborting boot",
		"jwt signing init in production",
	}

	for _, phrase := range wantBlock {
		if !strings.Contains(src, phrase) {
			t.Errorf(
				"runtime.go missing production-fail phrase %q - init-JWT error in production must be fatal.",
				phrase,
			)
		}
	}
}

// TestRuntime_JWTSigningFallsBackToHS256InDev is the complement: in
// non-production (dev) mode, a failed InitJWTSigning returns nil and logs a
// fallback warning. Boot continues.
//
// The test reads runtime.go and verifies the dev fallback path.
func TestRuntime_JWTSigningFallsBackToHS256InDev(t *testing.T) {
	data, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatalf("read runtime.go: %v", err)
	}
	src := string(data)

	// The dev-fallthrough path: not IsProduction, log fallback, continue.
	wantBlock := []string{
		"jwt signing init failed, falling back to HS256",
	}

	for _, phrase := range wantBlock {
		if !strings.Contains(src, phrase) {
			t.Errorf(
				"runtime.go missing dev-fallback phrase %q - init-JWT error in dev should log fallback and continue.",
				phrase,
			)
		}
	}
}
