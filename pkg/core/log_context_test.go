package core_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestLogContextAbout_FilesALineAboutAnotherTenantUnderNone(t *testing.T) {
	acme := core.WithTenantID(context.Background(), "acme")
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		about  string
		tenant string
	}{
		{"own tenant", acme, "acme", "acme"},
		{"another tenant", acme, "globex", ""},
		{"another spelling of the slug", acme, "ACME", ""},
		{"every tenant at once", acme, "", ""},
		{"no request tenant", context.Background(), "globex", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.tenant, core.TenantIDFromCtx(core.LogContextAbout(tc.ctx, tc.about)))
		})
	}
}
