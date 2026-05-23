package enginehost

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// Plugins that mint tokens of their own read jwt_expiry_secs to match the
// engine. The key has to resolve through the adapter, not fall through to the
// raw environment: an instance that never set the variable still has a
// lifetime, and the plugins have to see it.
func TestConfigAdapter_JWTExpirySecs(t *testing.T) {
	t.Parallel()

	c := &configAdapter{cfg: &config.Config{JWTExpirySecs: 900}}

	if got := c.String("jwt_expiry_secs"); got != "900" {
		t.Errorf("String(jwt_expiry_secs) = %q, want \"900\"", got)
	}

	// Duration is the wrong reader for a seconds-valued key: it parses Go
	// duration syntax, so a bare integer yields zero and the
	// caller silently takes its own fallback instead.
	if got := c.Duration("jwt_expiry_secs"); got != 0 {
		t.Errorf("Duration(jwt_expiry_secs) = %v, want 0 - read it with String", got)
	}
}
