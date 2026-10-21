package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The migrator's MySQL session has to carry the settings every other engine
// pool does: a migration that reads the clock runs in UTC, and an UPDATE that
// matches a row without changing it counts the row, the way the running engine
// counts it. The probe migration records both from inside the migrator's own
// session.
func TestMigrate_MySQLSessionMatchesTheEnginePools(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("mysql not selected")
	}
	dsn := testdb.MySQLStockDatabase(t)

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "mysql"), 0o755))
	probe := `CREATE TABLE migrator_session_probe (id INT PRIMARY KEY, v INT, tz VARCHAR(64), matched INT);
INSERT INTO migrator_session_probe (id, v) VALUES (1, 1);
UPDATE migrator_session_probe SET v = 1 WHERE id = 1;
UPDATE migrator_session_probe SET matched = ROW_COUNT(), tz = @@session.time_zone WHERE id = 1;
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "mysql", "1_probe.up.sql"), []byte(probe), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "mysql", "1_probe.down.sql"), []byte("DROP TABLE migrator_session_probe;\n"), 0o644))

	n, err := db.Migrate(dsn, root, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	conn, err := db.Connect(context.Background(), dsn, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	row, err := conn.QueryRow(context.Background(), `SELECT tz, matched FROM migrator_session_probe WHERE id = 1`)
	require.NoError(t, err)
	var tz string
	var matched int
	require.NoError(t, row.Scan(&tz, &matched))
	require.Equal(t, "+00:00", tz, "the migrator's session runs in the server's time zone")
	require.Equal(t, 1, matched, "the migrator's session counts changed rows, not matched ones")
}
