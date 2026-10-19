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
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// A super admin acting in acme deletes globex, and acme's admin has raised
// acme's level to Debug. The kernel's purge lines name globex, so none of
// them reaches a record stamped acme. The same purge of acme itself keeps
// acme's stamp, and the rows go either way.
func TestKernelPurge_LinesAboutAnotherTenantCarryNoTenant(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		home, other := "acme-"+uuid.NewString()[:8], "globex-"+uuid.NewString()[:8]
		levels := observability.NewLogLevelMap(slog.LevelInfo)
		levels.SetTenantLevel(home, slog.LevelDebug)
		ring := logstream.NewBuffer(256)
		base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
		enrich := logging.NewEnrichHandler(logstream.NewInterceptor(base, ring), levels)
		prev := slog.Default()
		slog.SetDefault(enrich.Logger())
		t.Cleanup(func() { slog.SetDefault(prev) })

		ctx := core.WithTenantID(context.Background(), home)
		q := host.Querier(context.Background())
		_, err := q.Exec(context.Background(),
			`INSERT INTO sys_plugin_config (tenant_id, plugin_name, config) VALUES ($1, $2, $3)`,
			other, "purge-log-probe", "{}")
		require.NoError(t, err)

		purge := func() {
			t.Helper()
			require.NoError(t, deleteSysUsersRows(ctx, q, other))
			require.NoError(t, deleteCoreTableRows(ctx, q, other, coreOwnedTenantTables))
		}
		purge()
		var left int
		row, err := q.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM sys_plugin_config WHERE tenant_id = $1`, other)
		require.NoError(t, err)
		require.NoError(t, row.Scan(&left))
		assert.Zero(t, left, "the purge deleted globex's rows")
		assert.Empty(t, ring.Snapshot(func(e logstream.Entry) bool { return e.TenantID == home }, 0),
			"acme's raised level reaches no line about globex")

		// At an install-wide Debug the lines are all there, with no tenant.
		levels.SetDefaultLevel(slog.LevelDebug)
		purge()
		lines := ring.Snapshot(nil, 0)
		require.Len(t, lines, 1+len(coreOwnedTenantTables), "every purge line is still logged")
		for _, e := range lines {
			assert.Empty(t, e.TenantID, "%s: a line about globex is stamped %q", e.Message, e.TenantID)
		}

		require.NoError(t, deleteSysUsersRows(ctx, q, home))
		own := ring.Snapshot(func(e logstream.Entry) bool { return e.Attrs["slug"] == home }, 0)
		require.Len(t, own, 1)
		assert.Equal(t, home, own[0].TenantID, "a line about the requester's own tenant keeps its stamp")
	})
}
