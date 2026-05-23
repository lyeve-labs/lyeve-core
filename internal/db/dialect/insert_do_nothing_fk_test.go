//go:build !mutest

package dialect_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// InsertDoNothing must swallow a duplicate key and nothing else.
//
// INSERT IGNORE on MySQL would downgrade every error the statement can raise
// to a warning: a foreign key with no parent, a value too long for its column,
// a NULL in a NOT NULL column. Callers would read the resulting zero affected
// rows as "the row was already there" and answer 409 to a write the database
// actually rejected.
func TestInsertDoNothing_OnlySwallowsDuplicates(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dialects := []struct {
		name string
		d    dialect.Dialect
		pool func(*testing.T) db.DB
	}{
		{"postgres", dialect.Postgres{}, func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{"mysql", dialect.MySQL{}, func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{"mssql", dialect.MSSQL{}, func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	}

	for _, dc := range dialects {
		t.Run(dc.name, func(t *testing.T) {
			if !testdb.ShouldTest(dc.name) {
				t.Skipf("CI_DIALECT != %s", dc.name)
			}
			pool := dc.pool(t)
			ctx := context.Background()

			for _, ddl := range createParentChild(dc.name) {
				if _, err := pool.Exec(ctx, ddl); err != nil {
					t.Fatalf("setup %q: %v", firstLine(ddl), err)
				}
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS idn_child")
				_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS idn_parent")
			})

			if _, err := pool.Exec(ctx, "INSERT INTO idn_parent (id) VALUES (1)"); err != nil {
				t.Fatalf("seed parent: %v", err)
			}

			stmt := dialect.InsertDoNothing(dc.d, "idn_child", []string{"id", "parent_id"}, []string{"id"})

			if _, err := pool.Exec(ctx, stmt, 1, 1); err != nil {
				t.Fatalf("first insert: %v", err)
			}

			// A duplicate key is the one thing it is meant to swallow.
			if _, err := pool.Exec(ctx, stmt, 1, 1); err != nil {
				t.Fatalf("duplicate key must be swallowed, got: %v", err)
			}

			// A parent that does not exist is a rejected write, not a duplicate.
			if _, err := pool.Exec(ctx, stmt, 2, 999); err == nil {
				t.Fatal("insert referencing a nonexistent parent reported success; the foreign key violation was swallowed")
			}

			var n int
			row, err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM idn_child")
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if err := row.Scan(&n); err != nil {
				t.Fatalf("scan count: %v", err)
			}
			if n != 1 {
				t.Fatalf("idn_child holds %d rows, want 1", n)
			}
		})
	}
}

func createParentChild(engine string) []string {
	switch engine {
	case "mysql":
		return []string{
			"CREATE TABLE idn_parent (id INT NOT NULL PRIMARY KEY) ENGINE=InnoDB",
			`CREATE TABLE idn_child (
				id INT NOT NULL PRIMARY KEY,
				parent_id INT NOT NULL,
				FOREIGN KEY (parent_id) REFERENCES idn_parent (id)
			) ENGINE=InnoDB`,
		}
	case "mssql":
		return []string{
			"CREATE TABLE idn_parent (id INT NOT NULL PRIMARY KEY)",
			`CREATE TABLE idn_child (
				id INT NOT NULL PRIMARY KEY,
				parent_id INT NOT NULL REFERENCES idn_parent (id)
			)`,
		}
	default:
		return []string{
			"CREATE TABLE idn_parent (id INT NOT NULL PRIMARY KEY)",
			`CREATE TABLE idn_child (
				id INT NOT NULL PRIMARY KEY,
				parent_id INT NOT NULL REFERENCES idn_parent (id)
			)`,
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
