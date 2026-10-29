package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// Migration 013 reverses: the down migration drops the erasure marker and
// the up migration adds it back, on every dialect.
func TestMigration013_DownDropsTheErasureMarkerAndUpRestoresIt(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		tree := pool.Engine()
		if tree == "postgres" {
			tree = "psql"
		}
		read := func(name string) string {
			b, err := os.ReadFile(filepath.Join("..", "..", "migrations", tree, name))
			require.NoError(t, err)
			return string(b)
		}
		ctx := context.Background()
		sqlDB := pool.SQLDB()
		probe := func() error {
			rows, err := sqlDB.QueryContext(ctx, `SELECT erasure_pending_at FROM sys_users WHERE 1 = 0`)
			if err != nil {
				return err
			}
			return rows.Close()
		}
		require.NoError(t, probe(), "the engine's migrations add the marker")

		_, err := sqlDB.ExecContext(ctx, read("013_sys_users_erasure_pending_at.down.sql"))
		require.NoError(t, err)
		assert.Error(t, probe(), "the down migration drops the marker")

		_, err = sqlDB.ExecContext(ctx, read("013_sys_users_erasure_pending_at.up.sql"))
		require.NoError(t, err)
		assert.NoError(t, probe(), "the up migration adds it back")
	})
}
