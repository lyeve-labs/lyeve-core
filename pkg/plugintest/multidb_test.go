package plugintest

import (
	"os"
	"testing"
)

// DialectNames

func TestDialectNames_AllEnabledByDefault(t *testing.T) {
	// When CI_DIALECT is unset, all 3 dialects should be active.
	orig := os.Getenv("CI_DIALECT")
	os.Unsetenv("CI_DIALECT")
	defer os.Setenv("CI_DIALECT", orig)

	names := DialectNames()
	if len(names) != 3 {
		t.Errorf("expected 3 active dialects, got %d: %v", len(names), names)
	}
}

func TestDialectNames_FilteredByEnv(t *testing.T) {
	orig := os.Getenv("CI_DIALECT")
	os.Setenv("CI_DIALECT", "postgres")
	defer os.Setenv("CI_DIALECT", orig)

	names := DialectNames()
	if len(names) != 1 {
		t.Fatalf("expected 1 active dialect, got %d: %v", len(names), names)
	}
	if names[0] != "postgres" {
		t.Errorf("expected 'postgres', got %q", names[0])
	}
}

func TestDialectNames_EmptyWhenNoMatch(t *testing.T) {
	orig := os.Getenv("CI_DIALECT")
	os.Setenv("CI_DIALECT", "nosql")
	defer os.Setenv("CI_DIALECT", orig)

	names := DialectNames()
	if len(names) != 0 {
		t.Errorf("expected 0 active dialects, got %d: %v", len(names), names)
	}
}

// supportedDialects order

func TestSupportedDialects_Order(t *testing.T) {
	// The order must be postgres -> mysql -> mssql so PG runs first
	// (PG is most permissive, so failures there surface earlier).
	want := []string{"postgres", "mysql", "mssql"}
	for i, d := range want {
		if i >= len(supportedDialects) {
			t.Fatalf("supportedDialects shorter than expected at index %d", i)
		}
		if supportedDialects[i] != d {
			t.Errorf("supportedDialects[%d] = %q, want %q", i, supportedDialects[i], d)
		}
	}
}

// dialectHostFactory coverage

func TestDialectHostFactory_HasAllDialects(t *testing.T) {
	for _, d := range supportedDialects {
		if _, ok := dialectHostFactory[d]; !ok {
			t.Errorf("dialectHostFactory missing entry for %q", d)
		}
	}
}

func TestDialectHostFactory_NoExtras(t *testing.T) {
	for d := range dialectHostFactory {
		found := false
		for _, sd := range supportedDialects {
			if d == sd {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("dialectHostFactory has extra entry %q not in supportedDialects", d)
		}
	}
}
