//go:build !mutest

package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// A MySQL migration that adds an index has to run cleanly against a database
// that already holds it. MySQL 8 has no CREATE INDEX IF NOT EXISTS, so a bare
// statement fails the second run with a duplicate key name, while the other
// two dialects guard theirs.
func TestMySQLMigration_UsersTenantColumnRunsTwice(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT selects another dialect")
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "migrations", "mysql", "002_sys_users_tenant_id.up.sql"))
	require.NoError(t, err)

	sqlDB := testdb.MySQL(t).SQLDB()
	ctx := context.Background()

	// The pool arrives migrated, so this is the second run of the script.
	_, err = sqlDB.ExecContext(ctx, string(script))
	require.NoError(t, err)

	var indexes int
	require.NoError(t, sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT index_name) FROM information_schema.statistics
		 WHERE table_schema = DATABASE() AND table_name = 'sys_users' AND index_name = 'idx_sys_users_tenant'`).Scan(&indexes))
	assert.Equal(t, 1, indexes)
}
