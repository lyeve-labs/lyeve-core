package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The MySQL key moved from the server to the engine because MySQL 9 has no MD5
// function. An engine still asking the server must take the same lock.
func TestLockKey_MySQLMatchesTheServersMD5(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("mysql dialect not enabled")
	}
	ctx := context.Background()
	conn, err := testdb.MySQL(t).SQLDB().Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	var fromServer string
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT CONCAT('"+migrationLockName+"_', MD5(DATABASE()))").Scan(&fromServer))
	got, err := lockKey(ctx, conn, "mysql")
	require.NoError(t, err)
	assert.Equal(t, fromServer, got)

	locked, err := tryLock(ctx, conn, "mysql", got)
	require.NoError(t, err)
	assert.True(t, locked, "a free lock is granted")
	_, err = conn.ExecContext(ctx, unlockSQLFor("mysql", got))
	require.NoError(t, err)
}

func TestLockKey_OtherDialectsUseTheFixedName(t *testing.T) {
	for _, d := range []string{"postgres", "mssql"} {
		got, err := lockKey(context.Background(), nil, d)
		require.NoError(t, err)
		assert.Equal(t, migrationLockName, got)
	}
}
