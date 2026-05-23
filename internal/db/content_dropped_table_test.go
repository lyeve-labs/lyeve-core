//go:build !mutest

package db_test

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// TestContentStore_ReadAfterTableDropped_ReportsMissingTable pins the error a
// content read produces once the generated table is gone, which is what an API
// caller hits between a schema delete and its next request. The read is a
// client asking for something that no longer exists, so the error has to be
// recognizable as a missing table on every dialect. Left unrecognized it takes
// the store-error default of 503 and tells the caller to retry a request that
// can never succeed.
//
// The store is built fresh after the drop on purpose. A store that has already
// served the table remembers that its tenant column is present and reads
// straight through to the SELECT; a cold one first runs the conditional ALTER
// that adds the column, and that is the statement an API process meets when the
// schema is deleted before anything reads it.
func TestContentStore_ReadAfterTableDropped_ReportsMissingTable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dialects := []struct {
		name string
		pool func(*testing.T) db.DB
	}{
		{"postgres", func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{"mysql", func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{"mssql", func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.pool(t)
			ctx := context.Background()

			const schemaName = "dropped_table_read"
			setupContentStore(t, pool, schemaName)

			// Drop the generated table the way a schema delete does, leaving the
			// schema row behind so the read gets past the registry lookup and
			// reaches the table itself.
			if _, err := pool.Exec(ctx, "DROP TABLE "+domain.TableName(schemaName)); err != nil {
				t.Fatalf("drop generated table: %v", err)
			}

			cold := db.NewContentStore(pool, newTestRegistry(t, pool).Source())

			for _, tc := range []struct {
				name string
				call func() error
			}{
				{"list", func() error {
					_, err := cold.List(ctx, schemaName, 25, 0, nil)
					return err
				}},
				{"insert", func() error {
					_, err := cold.Insert(ctx, schemaName, map[string]any{"title": "after"})
					return err
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					err := tc.call()
					if err == nil {
						t.Fatal("call against a dropped table returned no error")
					}
					t.Logf("error: %v", err)
					if !plugin.IsTableNotExistError(err) {
						t.Errorf("plugin.IsTableNotExistError = false, want true; error was %v", err)
					}
					if !sqlx.IsUndefinedTable(err) {
						t.Errorf("sqlx.IsUndefinedTable = false, want true; error was %v", err)
					}
				})
			}
		})
	}
}
