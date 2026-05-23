package enginehost

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Plugins that mail a link to the console read its URL through the adapter, so
// the value the engine validated at boot is the one they build on.
func TestConfigAdapter_ConsoleURL(t *testing.T) {
	t.Parallel()

	c := &configAdapter{cfg: &config.Config{ConsoleURL: "https://admin.example.com", BaseURL: "https://api.example.com"}}
	if got := c.String(core.ConfigKeyConsoleURL); got != "https://admin.example.com" {
		t.Errorf("String(%s) = %q, want the console URL", core.ConfigKeyConsoleURL, got)
	}
	got, err := core.ConsoleURL(c)
	if err != nil || got != "https://admin.example.com" {
		t.Errorf("ConsoleURL = %q, %v", got, err)
	}
}
