package core_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestFallbackTenant_MarksOnlyTheTenantItSet(t *testing.T) {
	bg := context.Background()
	cases := []struct {
		name     string
		ctx      context.Context
		tenant   string
		fallback bool
	}{
		{"no tenant", bg, "", false},
		{"a mapped tenant", core.WithTenantID(bg, "acme"), "acme", false},
		{"a fallback tenant", core.WithFallbackTenantID(bg, "acme"), "acme", true},
		{"a fallback replaced by another tenant", core.WithTenantID(core.WithFallbackTenantID(bg, "acme"), "globex"), "globex", false},
		{"an empty fallback", core.WithFallbackTenantID(bg, ""), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.tenant, core.TenantIDFromCtx(tc.ctx))
			assert.Equal(t, tc.fallback, core.IsFallbackTenant(tc.ctx))
		})
	}
}
