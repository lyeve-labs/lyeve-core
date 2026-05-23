package db_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// newestEngineMigration is the highest version in the migrations tree, read
// from the file names so the test follows the tree as it grows.
func newestEngineMigration(t *testing.T) uint {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations", "psql"))
	require.NoError(t, err)
	var newest uint
	for _, e := range entries {
		head, _, _ := strings.Cut(e.Name(), "_")
		if v, err := strconv.Atoi(head); err == nil && uint(v) > newest {
			newest = uint(v)
		}
	}
	require.NotZero(t, newest)
	return newest
}

type seededDialect struct {
	name string
	open func(t *testing.T) (db.DB, string)
}

func seededDialects() []seededDialect {
	return []seededDialect{
		{"postgres", testdb.PostgresWithDSN},
		{"mysql", testdb.MySQLWithDSN},
		{"mssql", testdb.MSSQLWithDSN},
	}
}

func bookkeeping(t *testing.T, conn db.DB) (version int64, dirty bool) {
	t.Helper()
	row, err := conn.QueryRow(context.Background(), `SELECT version, dirty FROM `+db.MigrationsTable)
	require.NoError(t, err)
	require.NoError(t, row.Scan(&version, &dirty))
	return version, dirty
}

// An install whose schema is current but whose bookkeeping table is empty
// takes the version the seed reports and applies nothing, on every dialect.
func TestMigrate_SeedFillsAnEmptyBookkeepingTable(t *testing.T) {
	newest := newestEngineMigration(t)
	for _, d := range seededDialects() {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skip("dialect not selected")
			}
			conn, dsn := d.open(t)
			_, err := conn.Exec(context.Background(), `DROP TABLE `+db.MigrationsTable)
			require.NoError(t, err)

			var asked string
			seed := func(_ context.Context, raw *sql.DB, engine string) (uint, bool, error) {
				require.NotNil(t, raw)
				asked = engine
				return newest, true, nil
			}
			n, err := db.Migrate(dsn, filepath.Join("..", "..", "migrations"), seed)
			require.NoError(t, err)
			assert.Zero(t, n, "a seeded current schema applies nothing")
			assert.Equal(t, d.name, asked, "the seed is told which engine it reads")

			version, dirty := bookkeeping(t, conn)
			assert.Equal(t, int64(newest), version)
			assert.False(t, dirty)
		})
	}
}

// A table that already records a version is the engine's own, so the seed is
// never asked, and a seed that fails stops the boot rather than guessing.
func TestMigrate_SeedIsAskedOnlyForAnEmptyTable(t *testing.T) {
	conn, dsn := testdb.PostgresWithDSN(t)
	root := filepath.Join("..", "..", "migrations")

	never := func(context.Context, *sql.DB, string) (uint, bool, error) {
		t.Fatal("the seed was asked although the table records a version")
		return 0, false, nil
	}
	_, err := db.Migrate(dsn, root, never)
	require.NoError(t, err)

	_, err = conn.Exec(context.Background(), `DROP TABLE `+db.MigrationsTable)
	require.NoError(t, err)
	failing := func(context.Context, *sql.DB, string) (uint, bool, error) {
		return 0, false, errors.New("the old record is dirty")
	}
	_, err = db.Migrate(dsn, root, failing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seed schema version")
}
