//go:build !mutest

package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

func jsonColType(engine string) string {
	switch engine {
	case "mysql":
		return "JSON"
	case "mssql":
		return "NVARCHAR(MAX)"
	default:
		return "JSONB"
	}
}

func eachJSONDialect(t *testing.T, fn func(t *testing.T, pool db.DB, table string)) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping multi-dialect JSON integration test in short mode")
	}
	for _, fx := range []struct {
		name string
		open func(t *testing.T) db.DB
	}{
		{"postgres", func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{"mysql", func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{"mssql", func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	} {
		t.Run(fx.name, func(t *testing.T) {
			if !testdb.ShouldTest(fx.name) {
				t.Skipf("CI_DIALECT != %s", fx.name)
			}
			pool := fx.open(t)
			ctx := context.Background()
			table := fmt.Sprintf("json_rt_%d", time.Now().UnixNano()%100000)
			col := jsonColType(pool.Engine())
			ddl := fmt.Sprintf("CREATE TABLE %s (id INT PRIMARY KEY, doc %s)", table, col)
			if pool.Engine() == "mssql" {
				ddl = fmt.Sprintf("CREATE TABLE [%s] (id INT PRIMARY KEY, doc %s)", table, col)
			}
			if _, err := pool.Exec(ctx, ddl); err != nil {
				t.Fatalf("create table: %v", err)
			}
			t.Cleanup(func() {
				drop := fmt.Sprintf("DROP TABLE IF EXISTS %s", table)
				if pool.Engine() == "mssql" {
					drop = fmt.Sprintf("DROP TABLE IF EXISTS [%s]", table)
				}
				pool.Exec(context.Background(), drop) //nolint:errcheck
			})
			fn(t, pool, table)
		})
	}
}

func TestJSONField_RoundTrip_AllDialects(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want any
	}{
		{
			"nested objects, arrays, empty containers, large numbers",
			`{"user":{"name":"Alice","addr":{"city":"Exampleville","zip":"100-0001"}},"tags":["go","rust"],"scores":[10,20.5],"active":true,"empty":[],"big":9007199254740991,"neg":-2147483648}`,
			map[string]any{
				"user":   map[string]any{"name": "Alice", "addr": map[string]any{"city": "Exampleville", "zip": "100-0001"}},
				"tags":   []any{"go", "rust"},
				"scores": []any{float64(10), float64(20.5)}, "active": true, "empty": []any{},
				"big": float64(9007199254740991), "neg": float64(-2147483648),
			},
		},
		{
			"unicode and escapes",
			`{"greeting":"こんにちは","emoji":"🚀","quote":"he said \"hello\"","path":"a\\b"}`,
			map[string]any{"greeting": "こんにちは", "emoji": "🚀", "quote": `he said "hello"`, "path": `a\b`},
		},
		{
			"null values in objects and arrays",
			`{"key":null,"nested":{"deep":null,"present":"yes"},"arr":[1,null,3]}`,
			map[string]any{
				"key": nil, "nested": map[string]any{"deep": nil, "present": "yes"},
				"arr": []any{float64(1), nil, float64(3)},
			},
		},
		{
			"sql null column",
			"",
			"<SQL-NULL>",
		},
	}

	eachJSONDialect(t, func(t *testing.T, pool db.DB, table string) {
		ctx := context.Background()
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				id := int(time.Now().UnixNano() % 10000)
				if tt.doc == "" && tt.want == "<SQL-NULL>" {
					if _, err := pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, doc) VALUES ($1, NULL)", table), id); err != nil {
						t.Fatalf("insert null: %v", err)
					}
					row, qrErr := pool.QueryRow(ctx, fmt.Sprintf("SELECT doc FROM %s WHERE id = $1", table), id)
					if qrErr != nil {
						t.Fatalf("QueryRow: %v", qrErr)
					}
					var ns sql.NullString
					if err := row.Scan(&ns); err != nil {
						t.Fatalf("scan: %v", err)
					}
					if ns.Valid {
						t.Errorf("got non-null after NULL insert: %q", ns.String)
					}
					return
				}
				if _, err := pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, doc) VALUES ($1, $2)", table), id, tt.doc); err != nil {
					t.Fatalf("insert: %v", err)
				}
				row, qrErr := pool.QueryRow(ctx, fmt.Sprintf("SELECT doc FROM %s WHERE id = $1", table), id)
				if qrErr != nil {
					t.Fatalf("QueryRow: %v", qrErr)
				}
				var gotStr string
				if err := row.Scan(&gotStr); err != nil {
					t.Fatalf("scan: %v", err)
				}
				var got any
				if err := json.Unmarshal([]byte(gotStr), &got); err != nil {
					t.Fatalf("unmarshal: %v\nraw=%s", err, gotStr)
				}
				wantJ, _ := json.Marshal(tt.want)
				gotJ, _ := json.Marshal(got)
				if string(wantJ) != string(gotJ) {
					t.Errorf("round-trip mismatch\nwant: %v\ngot:  %v", tt.want, got)
				}
			})
		}
	})
}
