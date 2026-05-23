package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

func TestLoad_IPAllowlist(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    []string
		wantErr string
	}{
		{name: "ranges", value: "10.0.0.0/8, 192.168.1.0/24", want: []string{"10.0.0.0/8", "192.168.1.0/24"}},
		{name: "bare addresses become one-host ranges", value: "203.0.113.7,2001:db8::1", want: []string{"203.0.113.7/32", "2001:db8::1/128"}},
		{name: "empty entries are skipped", value: "10.0.0.0/8,,", want: []string{"10.0.0.0/8"}},
		// One bad entry stops the boot rather than switching the whole list off.
		{name: "an invalid prefix among valid ones", value: "10.0.0.0/8, 192.168.1.0/33", wantErr: `"192.168.1.0/33"`},
		{name: "a host name", value: "office.example.com", wantErr: `"office.example.com"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://localhost/cms")
			t.Setenv("JWT_SECRET", "test-secret-16+chars!")
			t.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")
			t.Setenv("IP_ALLOWLIST", tc.value)
			cfg, err := config.Load()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "IP_ALLOWLIST")
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, cfg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.IPAllowlist)
		})
	}
}
