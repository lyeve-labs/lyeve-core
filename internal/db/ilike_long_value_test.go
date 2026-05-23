//go:build !mutest

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// SQL Server caps a LIKE pattern at 8000 bytes, which is 4000 characters of
// NVARCHAR, and the contains form wraps the value in % to take it two over. A
// search for a 4000-character value, such as an erasure looking for a long
// identifier, must still match rather than fail with "String or binary data
// would be truncated".
func TestILike_MatchesValuesTooLongForALikePattern(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// 4000 characters of NVARCHAR is SQL Server's ceiling for both a LIKE
	// pattern and a CHARINDEX needle, and nothing gets past it: LIKE, CHARINDEX
	// and CAST(... AS NVARCHAR(MAX)) all answer "String or binary data would be
	// truncated" at 6000. So the contract is that everything up to the ceiling
	// matches, and the ordinary value is here so a build that matches nothing
	// at all cannot pass either.
	needles := map[string]string{
		"short":        "subject@example.com",
		"at the limit": strings.Repeat("x", 4000),
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

			body := "text"
			switch name {
			case "mysql":
				body = "LONGTEXT"
			case "mssql":
				body = "NVARCHAR(MAX)"
			}
			if _, err := pool.Exec(ctx, `CREATE TABLE ilike_probe (body `+body+`)`); err != nil {
				t.Fatalf("create probe table: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DROP TABLE ilike_probe`)
			})

			for _, v := range needles {
				if _, err := pool.Exec(ctx,
					`INSERT INTO ilike_probe (body) VALUES ($1)`, "prefix-"+v+"-suffix"); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}

			clause := sqldialect.ILike(pool.Engine(), "body", "$1")
			for label, needle := range needles {
				row, err := pool.QueryRow(ctx,
					`SELECT COUNT(*) FROM ilike_probe WHERE `+clause, needle)
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				var n int
				if err := row.Scan(&n); err != nil {
					t.Fatalf("%s: scan: %v", label, err)
				}
				if n != 1 {
					t.Errorf("%s (%d chars): matched %d rows, want 1", label, len(needle), n)
				}
			}
		})
	}
}
