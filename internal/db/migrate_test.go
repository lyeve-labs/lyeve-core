package db

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestEngineFromDSN(t *testing.T) {
	cases := []struct {
		name, dsn, want string
	}{
		{"postgres url", "postgres://u:p@host:5432/db?sslmode=disable", "postgres"},
		{"postgresql url", "postgresql://u:p@host/db", "postgres"},
		{"postgres keyword dsn", "host=localhost user=cms dbname=cms sslmode=disable", "postgres"},
		{"mysql url", "mysql://u:p@tcp(host:3306)/db", "mysql"},
		{"sqlserver url", "sqlserver://u:p@host:1433?database=db", "mssql"},
		{"mssql alias", "mssql://u:p@host/db", "mssql"},
		{"uppercase scheme", "POSTGRES://u@host/db", "postgres"},
		{"empty falls back to postgres", "", "postgres"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EngineFromDSN(tc.dsn); got != tc.want {
				t.Errorf("EngineFromDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestEngineDir(t *testing.T) {
	cases := map[string]string{
		"postgres": "psql",
		"mysql":    "mysql",
		"mssql":    "mssql",
		"unknown":  "psql", // unknown engines use the postgres layout
	}
	for engine, want := range cases {
		if got := engineDir(engine); got != want {
			t.Errorf("engineDir(%q) = %q, want %q", engine, got, want)
		}
	}
}

// TestDriverNameFor locks the engine -> database/sql driver name mapping.
// Postgres uses pgx via pgx/v5/stdlib. Unknown engines fall back to pgx
// (matches engineDir's behavior).
func TestDriverNameFor(t *testing.T) {
	cases := map[string]string{
		"postgres": "pgx",
		"mysql":    "mysql",
		"mssql":    "sqlserver",
		"unknown":  "pgx",
	}
	for engine, want := range cases {
		if got := driverNameFor(engine); got != want {
			t.Errorf("driverNameFor(%q) = %q, want %q", engine, got, want)
		}
	}
}

// TestMigrationParity locks the three engine migration sets to the same
// version list so a forgotten up/down or a typo in one folder fails the build
// instead of silently leaving an engine behind. Listing them via os.ReadDir
// keeps the test honest: if someone adds a new file in psql/ without
// mirroring it, the diff surfaces here.
func TestMigrationParity(t *testing.T) {
	engines := []string{"psql", "mysql", "mssql"}
	sets := make(map[string][]string, len(engines))
	for _, e := range engines {
		dir := filepath.Join("..", "..", "migrations", e)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s migrations: %v", dir, err)
		}
		var files []string
		for _, ent := range entries {
			if ent.IsDir() || filepath.Ext(ent.Name()) != ".sql" {
				continue
			}
			files = append(files, ent.Name())
		}
		sort.Strings(files)
		sets[e] = files
	}
	if len(sets["psql"]) == 0 {
		t.Fatal("psql migrations directory is empty - sanity check failed")
	}
	for _, e := range []string{"mysql", "mssql"} {
		if diff := stringSetDiff(sets["psql"], sets[e]); diff != "" {
			t.Errorf("migration parity drift psql vs %s:\n%s", e, diff)
		}
	}
}

// stringSetDiff returns a human-readable diff of two sorted file lists, or ""
// when they match exactly. Used by TestMigrationParity for clear failures.
func stringSetDiff(a, b []string) string {
	in := func(set []string, s string) bool {
		for _, x := range set {
			if x == s {
				return true
			}
		}
		return false
	}
	var missingFromB, missingFromA []string
	for _, s := range a {
		if !in(b, s) {
			missingFromB = append(missingFromB, s)
		}
	}
	for _, s := range b {
		if !in(a, s) {
			missingFromA = append(missingFromA, s)
		}
	}
	if len(missingFromA) == 0 && len(missingFromB) == 0 {
		return ""
	}
	out := ""
	if len(missingFromB) > 0 {
		out += "  missing from second set: " + joinComma(missingFromB) + "\n"
	}
	if len(missingFromA) > 0 {
		out += "  missing from first set: " + joinComma(missingFromA) + "\n"
	}
	return out
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}
