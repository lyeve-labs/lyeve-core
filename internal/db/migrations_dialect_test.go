package db

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const migrationsPath = "../../migrations"

var engineDirs = []string{"psql", "mysql", "mssql"}

// TestMigrations_UpDownPairing ensures every .up.sql has a matching .down.sql
// in every dialect directory. Missing down scripts block rollbacks silently.
func TestMigrations_UpDownPairing(t *testing.T) {
	for _, dir := range engineDirs {
		entries, err := os.ReadDir(filepath.Join(migrationsPath, dir))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".up.sql") {
				continue
			}
			down := strings.Replace(e.Name(), ".up.sql", ".down.sql", 1)
			if _, err := os.Stat(filepath.Join(migrationsPath, dir, down)); os.IsNotExist(err) {
				t.Errorf("%s: missing down migration %q", dir, down)
			}
		}
	}
}

// tableRE matches CREATE TABLE [IF NOT EXISTS] <name> across dialects.
var tableRE = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` + "`" + `?\[?(\w+)\]?` + "`" + `?`)

// commentRE matches a line comment and the rest of its line.
var commentRE = regexp.MustCompile(`--[^\n]*`)

// withoutComments strips line comments so the parity check reads DDL the
// engine would run rather than DDL somebody wrote about.
//
// A migration may print an example CREATE TABLE inside a comment block. If
// the check read it, a table that never exists would count on every dialect
// that carries the example, and removing the example from one dialect would
// fail the check for a table no migration creates.
func withoutComments(sql []byte) []byte {
	return commentRE.ReplaceAll(sql, nil)
}

// TestMigrations_CrossDialectTableParity verifies that every migration version
// creates the same set of tables across all three dialects, regardless of
// DDL syntax. A migration written for Postgres but missing from MySQL or MSSQL
// would silently skip table creation on that engine at deploy time.
func TestMigrations_CrossDialectTableParity(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(migrationsPath, "psql"))
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			versions = append(versions, strings.TrimSuffix(e.Name(), ".up.sql"))
		}
	}
	sort.Strings(versions)

	sets := make(map[string]map[string][]string) // dir -> version -> tables
	for _, dir := range engineDirs {
		sets[dir] = make(map[string][]string)
		for _, v := range versions {
			data, err := os.ReadFile(filepath.Join(migrationsPath, dir, v+".up.sql"))
			if err != nil {
				t.Fatal(err)
			}
			var tables []string
			for _, m := range tableRE.FindAllStringSubmatch(string(withoutComments(data)), -1) {
				name := strings.ToLower(m[1])
				if name != "if" && name != "not" {
					tables = append(tables, name)
				}
			}
			sort.Strings(tables)
			sets[dir][v] = tables
		}
	}

	for v, want := range sets["psql"] {
		if len(want) == 0 {
			continue
		}
		for _, dir := range []string{"mysql", "mssql"} {
			got := sets[dir][v]
			if len(got) == 0 {
				t.Errorf("%s/%s.up.sql: no tables; psql creates %v", dir, v, want)
			} else if !strSliceEq(want, got) {
				t.Errorf("%s/%s.up.sql table mismatch: psql=%v, %s=%v", dir, v, want, dir, got)
			}
		}
	}
}

// TestPlaceholderRewrite_Dialects verifies $N placeholder translation for all
// three engines. Postgres is passthrough. MySQL replaces $N with ? and reorders
// args to positional order. MSSQL replaces $N with @pN. $N inside string
// literals and comments must not be rewritten.
func TestPlaceholderRewrite_Dialects(t *testing.T) {
	tests := []struct {
		name     string
		engine   string
		sql      string
		args     []any
		wantSQL  string
		wantArgs []any
	}{
		// Postgres: passthrough.
		{"pg passthrough", "postgres", "SELECT $1, $2", []any{1, "x"}, "SELECT $1, $2", []any{1, "x"}},
		// MySQL: $N -> ?, args reordered by appearance.
		{"mysql basic", "mysql", "SELECT $1", []any{42}, "SELECT ?", []any{42}},
		{"mysql out-of-order", "mysql", "SET x=$2 WHERE id=$1", []any{99, "v"}, "SET x=? WHERE id=?", []any{"v", 99}},
		// MSSQL: $N -> @pN, args unchanged.
		{"mssql multi", "mssql", "SELECT $1, $2, $3", []any{1, 2, 3}, "SELECT @p1, @p2, @p3", []any{1, 2, 3}},
		// Literals and comments must not be rewritten.
		{"mysql string literal", "mysql", "SELECT '$1'", nil, "SELECT '$1'", nil},
		{"mysql line comment", "mysql", "SELECT $1 -- $2", nil, "SELECT ? -- $2", nil},
		{"mysql block comment", "mysql", "SELECT /* $1 */ $2", nil, "SELECT /* $1 */ ?", nil},
		{"mssql string literal", "mssql", "SELECT '$1', $2", nil, "SELECT '$1', @p2", nil},
		// Unknown engine: passthrough.
		{"unknown engine", "oracle", "SELECT $1, $2", nil, "SELECT $1, $2", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotSQL, gotArgs := rewritePlaceholders(tc.sql, tc.engine, tc.args)
			if gotSQL != tc.wantSQL {
				t.Errorf("SQL: got %q, want %q", gotSQL, tc.wantSQL)
			}
			if len(gotArgs) != len(tc.wantArgs) {
				t.Fatalf("args len %d, want %d", len(gotArgs), len(tc.wantArgs))
			}
			for i := range gotArgs {
				if gotArgs[i] != tc.wantArgs[i] {
					t.Errorf("args[%d]: got %v, want %v", i, gotArgs[i], tc.wantArgs[i])
				}
			}
		})
	}
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The parity check must read DDL the engine runs, not DDL a comment describes,
// so an example CREATE TABLE inside a comment block counts as no table.
func TestMigrations_ParityIgnoresCommentedDDL(t *testing.T) {
	commented := []byte(`-- CREATE TABLE IF NOT EXISTS _pivot_{a}_{b} (
--   a_id UUID NOT NULL
-- );
CREATE TABLE sys_real (id UUID PRIMARY KEY);`)

	var got []string
	for _, m := range tableRE.FindAllStringSubmatch(string(withoutComments(commented)), -1) {
		got = append(got, strings.ToLower(m[1]))
	}
	if !strSliceEq(got, []string{"sys_real"}) {
		t.Errorf("read %v; the commented example must not count as a table", got)
	}
}
