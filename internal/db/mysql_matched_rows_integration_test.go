package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// An update that writes the values a row already holds has to look like an
// update that found its row, not like one that found nothing.
//
// MySQL counts changed rows by default, so it reports zero for the no-op case,
// and callers read RowsAffected == 0 as "no such row". Without the driver flag
// a second, identical write would be indistinguishable from a write against a
// deleted row, and a handler would answer 404 for a row it had just loaded.
func TestUpdate_NoOpStillReportsTheRowItMatched(t *testing.T) {
	for name, open := range map[string]func(*testing.T) db.DB{
		"postgres": testdb.Postgres,
		"mysql":    testdb.MySQL,
	} {
		t.Run(name, func(t *testing.T) {
			pool := open(t)
			ctx := context.Background()

			_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS rows_affected_probe (
				id VARCHAR(64) NOT NULL PRIMARY KEY, note VARCHAR(64) NOT NULL)`)
			require.NoError(t, err, "create probe table")
			t.Cleanup(func() { _, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS rows_affected_probe`) })

			_, err = pool.Exec(ctx, `DELETE FROM rows_affected_probe WHERE id = $1`, "probe-1")
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO rows_affected_probe (id, note) VALUES ($1, $2)`, "probe-1", "same")
			require.NoError(t, err, "seed probe row")

			// First write changes the value.
			affected := func(res sql.Result) int64 {
				t.Helper()
				n, err := res.RowsAffected()
				require.NoError(t, err)
				return n
			}

			res, err := pool.Exec(ctx, `UPDATE rows_affected_probe SET note = $1 WHERE id = $2`, "changed", "probe-1")
			require.NoError(t, err)
			assert.EqualValues(t, 1, affected(res), "a changing update matches one row")

			// Second write is identical. The row is still there, so the count
			// must still be one.
			res, err = pool.Exec(ctx, `UPDATE rows_affected_probe SET note = $1 WHERE id = $2`, "changed", "probe-1")
			require.NoError(t, err)
			assert.EqualValues(t, 1, affected(res),
				"a no-op update must not look like a missing row")

			// A genuinely absent row still reports zero, or the signal is useless.
			res, err = pool.Exec(ctx, `UPDATE rows_affected_probe SET note = $1 WHERE id = $2`, "x", "no-such-row")
			require.NoError(t, err)
			assert.EqualValues(t, 0, affected(res), "a missing row must still report zero")
		})
	}
}
