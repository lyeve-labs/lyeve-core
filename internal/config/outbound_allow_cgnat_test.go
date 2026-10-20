package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestLoad_OutboundAllowCGNAT(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db?sslmode=disable")
	t.Setenv("JWT_SECRET", "a-very-long-secret-key-that-is-secure")
	t.Setenv("ENCRYPTION_KEY", "a-separate-encryption-key-of-32-chars-or-more")
	t.Setenv("APP_ENV", "development")

	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"false", false},
		{"yes please", false},
		{"true", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("LYEVE_OUTBOUND_ALLOW_CGNAT", tc.value)
			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.OutboundAllowCGNAT)
		})
	}
}

// The setting widens the outbound guard and is read once at boot, so the
// admin layer may not store it and the configuration page explains it.
func TestOutboundAllowCGNAT_IsOperatorOnlyAndDescribed(t *testing.T) {
	assert.True(t, core.IsOperatorOnlyKey("LYEVE_OUTBOUND_ALLOW_CGNAT"))
	d, ok := DescribeSetting("LYEVE_OUTBOUND_ALLOW_CGNAT")
	require.True(t, ok)
	assert.Contains(t, d.Description, "100.64.0.0/10")
}
