package config

import (
	"strings"
	"testing"
)

// The metrics token is a static bearer credential that never expires, so a
// placeholder or a short value is refused at boot in every environment. Unset
// stays allowed: the route then needs a super admin.
func TestValidateCritical_RefusesAWeakMetricsToken(t *testing.T) {
	base := Config{
		JWTSecret:     "a-very-long-jwt-secret-key-that-is-secure",
		EncryptionKey: "a-completely-different-encryption-key-32chars",
	}
	for _, token := range []string{"metrics", "short-but-random-9f3a", "change-me-to-a-long-random-metrics-token", "0123456789abcdef0123456789abcde"} {
		c := base
		c.MetricsToken = token
		err := c.ValidateCritical()
		if err == nil {
			t.Errorf("METRICS_TOKEN %q booted", token)
			continue
		}
		if !strings.Contains(err.Error(), "METRICS_TOKEN") || strings.Contains(err.Error(), token) {
			t.Errorf("the refusal must name METRICS_TOKEN and not print it, got: %v", err)
		}
	}
	for _, token := range []string{"", "7f3a9c1e5b8d2046a1c4e7f0b3d6895247ae0c13"} {
		c := base
		c.MetricsToken = token
		if err := c.ValidateCritical(); err != nil {
			t.Errorf("METRICS_TOKEN %q must boot: %v", token, err)
		}
	}
}
