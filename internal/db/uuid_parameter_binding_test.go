//go:build !mutest

package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// uuidSeedRows is large enough that a seek and a scan differ by two orders of
// magnitude in logical reads. Rows are generated server-side so seeding costs
// one round trip rather than one per row.
const uuidSeedRows = 5000

// seekReadCeiling is the logical-read budget for a single-row lookup that
// seeks. A seek on this table costs 3 reads. A scan costs roughly 75. The
// ceiling sits well clear of both so the assertion reports a plan regression,
// not buffer-pool noise.
const seekReadCeiling = 25

func skipUnlessMSSQL(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping MSSQL parameter binding test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}
}

// newUUIDKeyTable creates a table keyed by a UUID held in colType, fills it,
// and returns the table name with one key that is really in it.
func newUUIDKeyTable(t *testing.T, pool db.DB, colType string) (string, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("uuidkey_%d", time.Now().UnixNano()%1_000_000)

	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s (id %s NOT NULL PRIMARY KEY, payload NVARCHAR(200) NOT NULL)",
		table, colType)); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE "+table) // best-effort test cleanup
	})

	// NEWID() renders uppercase. The Go side always renders lowercase. Storing
	// lowercase keeps the comparison independent of the database collation.
	key := "CAST(LOWER(CONVERT(CHAR(36), NEWID())) AS " + colType + ")"
	if colType == "UNIQUEIDENTIFIER" {
		key = "NEWID()"
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s (id, payload) SELECT TOP (%d) %s, N'payload' FROM sys.all_objects a CROSS JOIN sys.all_objects b",
		table, uuidSeedRows, key)); err != nil {
		t.Fatalf("seed %s: %v", table, err)
	}

	row, err := pool.QueryRow(ctx, fmt.Sprintf("SELECT TOP 1 CONVERT(CHAR(36), id) FROM %s", table))
	if err != nil {
		t.Fatalf("read a key from %s: %v", table, err)
	}
	var text string
	if err := row.Scan(&text); err != nil {
		t.Fatalf("scan a key from %s: %v", table, err)
	}
	id, err := uuid.Parse(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("parse key %q: %v", text, err)
	}
	return table, id
}

// planFor returns the logical reads per execution and the distinct physical
// operators SQL Server cached for the statement carrying marker.
func planFor(t *testing.T, pool db.DB, marker string) (int64, string) {
	t.Helper()
	row, err := pool.QueryRow(context.Background(),
		`SELECT TOP 1 qs.total_logical_reads / qs.execution_count,
		        CAST(qp.query_plan AS NVARCHAR(MAX))
		 FROM sys.dm_exec_query_stats qs
		 CROSS APPLY sys.dm_exec_sql_text(qs.sql_handle) st
		 CROSS APPLY sys.dm_exec_query_plan(qs.plan_handle) qp
		 WHERE st.text LIKE $1
		 ORDER BY qs.last_execution_time DESC`, "%"+marker+"%")
	if err != nil {
		t.Fatalf("read plan cache: %v", err)
	}
	var reads int64
	var plan string
	if err := row.Scan(&reads, &plan); err != nil {
		t.Fatalf("scan plan cache for %s: %v", marker, err)
	}
	return reads, physicalOps(plan)
}

// physicalOps lists the distinct physical operators named in a plan.
func physicalOps(plan string) string {
	var ops []string
	seen := map[string]bool{}
	for _, part := range strings.Split(plan, `PhysicalOp="`)[1:] {
		i := strings.Index(part, `"`)
		if i <= 0 {
			continue
		}
		if op := part[:i]; !seen[op] {
			seen[op] = true
			ops = append(ops, op)
		}
	}
	return strings.Join(ops, ",")
}

// TestUUIDParameter_SeeksTextKeyColumn guards the whole defect class.
// go-mssqldb sends a Go string as NVARCHAR, and uuid.UUID is a driver.Valuer
// that yields a string, so a UUID argument arrives as NVARCHAR too. NVARCHAR
// outranks CHAR and VARCHAR in SQL Server's data type precedence, so
// comparing it to a text key converts the column rather than the parameter,
// which rules out a seek on tables of every size while staying invisible on
// small ones.
func TestUUIDParameter_SeeksTextKeyColumn(t *testing.T) {
	skipUnlessMSSQL(t)
	pool := testdb.MSSQL(t)
	ctx := context.Background()

	for _, colType := range []string{"CHAR(36)", "VARCHAR(36)"} {
		t.Run(colType, func(t *testing.T) {
			table, id := newUUIDKeyTable(t, pool, colType)
			marker := fmt.Sprintf("seek_%s_%d", table, time.Now().UnixNano()%100000)

			row, err := pool.QueryRow(ctx, fmt.Sprintf(
				"/*%s*/ SELECT payload FROM %s WHERE id = $1", marker, table), id)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			var payload string
			if err := row.Scan(&payload); err != nil {
				t.Fatalf("a uuid.UUID argument did not match the row it names: %v", err)
			}

			reads, ops := planFor(t, pool, marker)
			if !strings.Contains(ops, "Seek") {
				t.Errorf("lookup by uuid.UUID on a %s key did not seek: plan used %s (%d logical reads)", colType, ops, reads)
			}
			if reads > seekReadCeiling {
				t.Errorf("lookup by uuid.UUID on a %s key cost %d logical reads, want <= %d (plan: %s)",
					colType, reads, seekReadCeiling, ops)
			}
		})
	}
}

// TestUUIDParameter_LocksOneRowUnderHoldlock measures what a single-row
// MERGE ... WITH (HOLDLOCK) upsert holds. One that cannot seek takes a range
// lock over every row its scan visits, so a single-row upsert locks the table
// and concurrent upserts serialize against each other.
func TestUUIDParameter_LocksOneRowUnderHoldlock(t *testing.T) {
	skipUnlessMSSQL(t)
	pool := testdb.MSSQL(t)
	ctx := context.Background()

	table, id := newUUIDKeyTable(t, pool, "CHAR(36)")

	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // the transaction only exists to hold locks

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`MERGE %s WITH (HOLDLOCK) AS t
		 USING (SELECT @p1 AS id) AS s ON t.id = s.id
		 WHEN MATCHED THEN UPDATE SET t.payload = N'merged'
		 WHEN NOT MATCHED THEN INSERT (id, payload) VALUES (s.id, N'inserted');`, table), id); err != nil {
		t.Fatalf("merge: %v", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT resource_type, COUNT(*) FROM sys.dm_tran_locks
		 WHERE request_session_id = @@SPID GROUP BY resource_type`)
	if err != nil {
		t.Fatalf("read held locks: %v", err)
	}
	defer rows.Close()
	held := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			t.Fatalf("scan held locks: %v", err)
		}
		held[kind] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("held locks: %v", err)
	}

	t.Logf("locks held by a single-row MERGE: %v", held)
}

// TestUUIDParameter_RoundTripsThroughUniqueIdentifier proves the binding
// change is correct for the other shape a UUID takes in this codebase. Core's
// own tables and every schema-engine generated table key on
// UNIQUEIDENTIFIER, not on text.
func TestUUIDParameter_RoundTripsThroughUniqueIdentifier(t *testing.T) {
	skipUnlessMSSQL(t)
	pool := testdb.MSSQL(t)
	ctx := context.Background()

	table := fmt.Sprintf("uuidnative_%d", time.Now().UnixNano()%1_000_000)
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s (id UNIQUEIDENTIFIER NOT NULL PRIMARY KEY, payload NVARCHAR(100) NOT NULL)", table)); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE "+table) // best-effort test cleanup
	})

	want := uuid.New()
	if _, err := pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, payload) VALUES ($1, N'native')", table), want); err != nil {
		t.Fatalf("insert a uuid.UUID into UNIQUEIDENTIFIER: %v", err)
	}

	row, err := pool.QueryRow(ctx, fmt.Sprintf("SELECT id, payload FROM %s WHERE id = $1", table), want)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	var got uuid.UUID
	var payload string
	if err := row.Scan(core.ScanUUID("mssql", &got), &payload); err != nil {
		t.Fatalf("a uuid.UUID argument did not match the UNIQUEIDENTIFIER row it wrote: %v", err)
	}
	if got != want {
		t.Errorf("UNIQUEIDENTIFIER round trip returned %s, want %s", got, want)
	}
}

// TestUnicodeParameter_SurvivesNVarCharRoundTrip is the guard on the binding
// change. Sending every string as a non-Unicode type would look like the same
// fix and would silently mangle any character outside the collation's code
// page, so the conversion is confined to UUID values and ordinary strings must
// keep reaching NVARCHAR intact.
func TestUnicodeParameter_SurvivesNVarCharRoundTrip(t *testing.T) {
	skipUnlessMSSQL(t)
	pool := testdb.MSSQL(t)
	ctx := context.Background()

	table := fmt.Sprintf("unicode_%d", time.Now().UnixNano()%1_000_000)
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE %s (id UNIQUEIDENTIFIER NOT NULL PRIMARY KEY, title NVARCHAR(200) NOT NULL, body NVARCHAR(MAX) NOT NULL)",
		table)); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE "+table) // best-effort test cleanup
	})

	cases := []struct{ name, text string }{
		{"cjk", "日本語のタイトル"},
		{"emoji", "release 🚀 shipped 🎉"},
		{"cyrillic", "Заголовок статьи"},
		{"arabic", "عنوان المقالة"},
		{"mixed", "café * 北京 * 🌍 * Ω"},
		{"astral plane", "𝄞 clef and 𝟘𝟙 digits"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := uuid.New()
			if _, err := pool.Exec(ctx, fmt.Sprintf(
				"INSERT INTO %s (id, title, body) VALUES ($1, $2, $3)", table), id, c.text, c.text); err != nil {
				t.Fatalf("insert %q: %v", c.text, err)
			}
			row, err := pool.QueryRow(ctx, fmt.Sprintf("SELECT title, body FROM %s WHERE id = $1", table), id)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			var title, body string
			if err := row.Scan(&title, &body); err != nil {
				t.Fatalf("scan %q: %v", c.text, err)
			}
			if title != c.text {
				t.Errorf("NVARCHAR title round trip mangled the value: got %q, want %q", title, c.text)
			}
			if body != c.text {
				t.Errorf("NVARCHAR(MAX) body round trip mangled the value: got %q, want %q", body, c.text)
			}
		})
	}
}
