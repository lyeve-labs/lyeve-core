//go:build !mutest

package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lyeve-labs/lyeve-core/internal/testdialect"
)

// The driver reads every DATETIME as UTC, so the times MySQL produces itself
// have to be UTC too. A server set to another zone evaluates NOW() and column
// defaults in that zone unless the session says otherwise, and every such time
// would read back off by the offset. The shared harness server runs in UTC,
// which hides it, so this test starts a server five hours east.
func TestConnect_MySQLClockIsUTCOnAServerInAnotherZone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MySQL integration test in short mode")
	}
	testdialect.Require(t, "mysql")

	ctx := context.Background()
	ctr, err := mysql.Run(ctx,
		containerImage("mysql:8"),
		mysql.WithDatabase("lyeve_test"),
		mysql.WithUsername("lyeve"),
		mysql.WithPassword("secret"),
		testcontainers.WithCmd("--default-time-zone=+05:00"),
		testcontainers.WithTmpfs(map[string]string{"/var/lib/mysql": "rw"}),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(90*time.Second),
		),
	)
	require.NoError(t, err, "start mysql container")
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	connStr, err := ctr.ConnectionString(ctx)
	require.NoError(t, err, "connection string")
	pool, err := Connect(ctx, connStr, 2)
	require.NoError(t, err, "connect to MySQL")
	t.Cleanup(func() { pool.Close() })

	var now time.Time
	row, err := pool.QueryRow(ctx, `SELECT NOW(6)`)
	require.NoError(t, err)
	require.NoError(t, row.Scan(&now))
	require.WithinDuration(t, time.Now(), now, time.Minute, "NOW() read back as UTC")

	_, err = pool.Exec(ctx, `CREATE TABLE clock_probe (
		id INT NOT NULL PRIMARY KEY,
		created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6))`)
	require.NoError(t, err, "create probe table")
	_, err = pool.Exec(ctx, `INSERT INTO clock_probe (id) VALUES (1)`)
	require.NoError(t, err, "insert probe row")

	var created time.Time
	row, err = pool.QueryRow(ctx, `SELECT created_at FROM clock_probe WHERE id = 1`)
	require.NoError(t, err)
	require.NoError(t, row.Scan(&created))
	require.WithinDuration(t, time.Now(), created, time.Minute, "a column default read back as UTC")
}
