package plugintest

import (
	"os"
	"testing"
)

// The harness has to answer this exactly as the engine does, which does not
// trim. A harness that trimmed would read " production" as production while
// the engine reads it as development, so a plugin test would assert the
// hardened path while the engine took the other one.
func TestIsProductionEnv_MatchesTheEngine(t *testing.T) {
	setEnv := func(t *testing.T, value string, set bool) {
		t.Helper()
		old, had := os.LookupEnv("APP_ENV")
		t.Cleanup(func() {
			if had {
				_ = os.Setenv("APP_ENV", old)
				return
			}
			_ = os.Unsetenv("APP_ENV")
		})
		if set {
			_ = os.Setenv("APP_ENV", value)
			return
		}
		_ = os.Unsetenv("APP_ENV")
	}

	for _, tc := range []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{"unset defaults to production", "", false, true},
		{"explicit empty reads as unset", "", true, true},
		{"production", "production", true, true},
		{"the engine lowercases", "Production", true, true},
		{"shouting is still production", "PRODUCTION", true, true},
		{"the engine does not trim", " production", true, false},
		{"development", "development", true, false},
		{"staging", "staging", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.value, tc.set)
			if got := IsProductionEnv(); got != tc.want {
				t.Errorf("APP_ENV=%q (set=%v) -> %v, want %v", tc.value, tc.set, got, tc.want)
			}
		})
	}
}
