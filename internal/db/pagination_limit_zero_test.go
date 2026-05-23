//go:build !mutest

package db_test

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// A limit of 0 means "no rows" under this engine's pagination contract, and
// reqparse accepts it. SQL Server rejects FETCH NEXT 0 ROWS ONLY at execution
// time, so the clause has to bind 0 in a form all three accept.
func TestLimitOffsetPlaceholders_BindsZeroLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	cases := []struct {
		limit, offset int
		want          int
	}{
		{limit: 0, offset: 0, want: 0},
		{limit: 2, offset: 1, want: 2},
		{limit: 1, offset: 0, want: 1},
		{limit: 100, offset: 0, want: 3},
	}

	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			var pool db.DB
			switch name {
			case "mysql":
				pool = testdb.MySQL(t)
			case "mssql":
				pool = testdb.MSSQL(t)
			default:
				pool = testdb.Postgres(t)
			}
			ctx := context.Background()

			if _, err := pool.Exec(ctx, `CREATE TABLE page_probe (id INT NOT NULL)`); err != nil {
				t.Fatalf("create probe table: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DROP TABLE page_probe`)
			})
			for _, id := range []int{1, 2, 3} {
				if _, err := pool.Exec(ctx, `INSERT INTO page_probe (id) VALUES ($1)`, id); err != nil {
					t.Fatalf("seed %d: %v", id, err)
				}
			}

			clause := sqldialect.LimitOffsetPlaceholders(pool.Engine(), 1, 2)
			for _, tc := range cases {
				rows, err := pool.Query(ctx,
					`SELECT id FROM page_probe ORDER BY id `+clause, tc.limit, tc.offset)
				if err != nil {
					t.Fatalf("limit=%d offset=%d: %v", tc.limit, tc.offset, err)
				}
				n := 0
				for rows.Next() {
					var id int
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						t.Fatalf("scan: %v", err)
					}
					n++
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					t.Fatalf("limit=%d offset=%d rows: %v", tc.limit, tc.offset, err)
				}
				if n != tc.want {
					t.Errorf("limit=%d offset=%d returned %d rows, want %d", tc.limit, tc.offset, n, tc.want)
				}
			}
		})
	}
}
