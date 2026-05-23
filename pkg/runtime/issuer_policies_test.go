package runtime

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
)

func TestIssuersPinnedOffRoster(t *testing.T) {
	policies := []auth.IssuerPolicy{
		{Issuer: "https://a.test", Tenant: "default"},
		{Issuer: "https://b.test", Tenant: "agency"},
		{Issuer: "https://c.test", Tenant: "gone"},
	}
	roster := map[string]bool{"default": true, "agency": true}
	lookup := func(slug string) (bool, error) { return roster[slug], nil }

	tests := []struct {
		name        string
		multiTenant bool
		want        []string
	}{
		{"single tenant serves default alone", false, []string{"https://b.test", "https://c.test"}},
		{"multi tenant serves the roster", true, []string{"https://c.test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{MultiTenant: tt.multiTenant, TrustedIssuerPolicies: policies}
			got, err := issuersPinnedOffRoster(cfg, lookup)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIssuersPinnedOffRoster_SingleTenantReadsNoRoster(t *testing.T) {
	cfg := &config.Config{TrustedIssuerPolicies: []auth.IssuerPolicy{{Issuer: "https://a.test", Tenant: "default"}}}
	got, err := issuersPinnedOffRoster(cfg, func(string) (bool, error) {
		t.Fatal("a single-tenant install has no roster to read")
		return false, nil
	})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestIssuersPinnedOffRoster_RosterFailureIsReported(t *testing.T) {
	cfg := &config.Config{MultiTenant: true, TrustedIssuerPolicies: []auth.IssuerPolicy{{Issuer: "https://a.test", Tenant: "agency"}}}
	down := errors.New("connection refused")
	_, err := issuersPinnedOffRoster(cfg, func(string) (bool, error) { return false, down })
	assert.ErrorIs(t, err, down)
}
