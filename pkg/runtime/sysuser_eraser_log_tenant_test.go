package runtime

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/logstream"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// A super admin acting in acme erases an address that holds an account in
// acme and one in globex, and acme's admin has raised acme's level to Debug.
// The erasure reaches both tenants, so its lines, the revocation failures
// included, go under no tenant, and nothing acme's admins read tells them how
// many accounts the address holds across the install.
func TestSysUserEraser_LinesCarryNoTenant(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		home, other := "acme-"+uuid.NewString()[:8], "globex-"+uuid.NewString()[:8]
		address := "erased-" + uuid.NewString()[:8] + "@example.com"
		ctx := core.WithTenantID(context.Background(), home)
		_, err := host.Querier(context.Background()).Exec(context.Background(),
			`INSERT INTO sys_users (email, password_hash, tenant_id) VALUES ($1, $2, $3)`, address, "hashed", home)
		require.NoError(t, err)
		_, created, err := host.(core.TenantAccountProvider).TenantAccounts().CreateTenantAccount(context.Background(),
			core.NewTenantAccount{TenantID: other, Address: address, PasswordHash: "hashed", ProviderSeen: true})
		require.NoError(t, err)
		require.True(t, created)

		levels := observability.NewLogLevelMap(slog.LevelInfo)
		levels.SetTenantLevel(home, slog.LevelDebug)
		ring := logstream.NewBuffer(256)
		base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
		enrich := logging.NewEnrichHandler(logstream.NewInterceptor(base, ring), levels)
		prev := slog.Default()
		slog.SetDefault(enrich.Logger())
		t.Cleanup(func() { slog.SetDefault(prev) })

		n, err := newSysUserEraser(failingRevokerHost{Host: host}).EraseSubject(ctx, address)
		require.NoError(t, err)
		require.EqualValues(t, 2, n, "both accounts are erased")

		ref := compliance.SubjectRef(address)
		lines := ring.Snapshot(func(e logstream.Entry) bool { return e.Attrs["subject_ref"] == ref }, 0)
		require.Len(t, lines, 3, "two revocation failures and the erasure line are logged")
		for _, e := range lines {
			assert.Empty(t, e.TenantID, "%s: an install-wide erasure line is stamped %q", e.Message, e.TenantID)
		}
	})
}
