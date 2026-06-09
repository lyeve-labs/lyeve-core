package plugintest

import (
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest/mockhost"
)

// A test host models an install that set LYEVE_CONSOLE_URL, so a plugin that
// mails links starts on it, and an option models one that did not.
func TestTestHost_ConsoleURL(t *testing.T) {
	t.Setenv("APP_ENV", "production")

	h := &testHost{}
	if got, err := core.ConsoleURL(h.Config()); err != nil || got != TestConsoleURL {
		t.Fatalf("default host: ConsoleURL = %q, %v", got, err)
	}

	unset := &testHost{}
	WithConsoleURL("")(unset)
	if _, err := core.ConsoleURL(unset.Config()); !errors.Is(err, core.ErrConsoleURLUnset) {
		t.Fatalf("unset in production: err = %v, want ErrConsoleURLUnset", err)
	}

	set := &testHost{}
	WithBaseURL("https://api.example.com")(set)
	WithConsoleURL("https://admin.example.com")(set)
	if got := set.Config().String(core.ConfigKeyConsoleURL); got != "https://admin.example.com" {
		t.Fatalf("WithConsoleURL after WithBaseURL: got %q", got)
	}

	m := mockhost.NewMockConfig()
	if got, err := core.ConsoleURL(m); err != nil || got != TestConsoleURL {
		t.Fatalf("mock config: ConsoleURL = %q, %v", got, err)
	}
	m.Set(core.ConfigKeyConsoleURL, "")
	if _, err := core.ConsoleURL(m); !errors.Is(err, core.ErrConsoleURLUnset) {
		t.Fatalf("mock config set empty: err = %v, want ErrConsoleURLUnset", err)
	}
}
