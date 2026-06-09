package plugintest

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// createTableRe matches the table name in a CREATE TABLE statement, with or
// without IF NOT EXISTS, and through each dialect's identifier quoting.
var createTableRe = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `\[]?([A-Za-z0-9_]+)`)

// AssertSharedTablesRegistered checks that every table the plugin's migrations
// create is one the engine will qualify on the database-per-tenant engines.
//
// A plugin's catalog tables are created once, in the engine's own database,
// and isolated by a tenant_id column. A tenant-scoped request on MySQL or SQL
// Server has run USE tenant_<slug>, and neither engine falls through to
// another database, so an unqualified reference looks for the table inside the
// tenant's database and fails there. The engine qualifies sys_* names on its
// own and any other name registered with sqlx.RegisterSharedTable, so a table
// that is neither is unreachable from every tenant-scoped request: on MySQL
// and MSSQL only, which is why it survives a Postgres-only test run.
//
// Call it with the plugin's own embedded migrations:
//
//	plugintest.AssertSharedTablesRegistered(t, migrations.Files)
func AssertSharedTablesRegistered(t T, migrationsFS fs.FS) {
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}

	var missing []string
	seen := map[string]bool{}

	err := fs.WalkDir(migrationsFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path.Base(p), ".up.sql") {
			return err
		}
		body, readErr := fs.ReadFile(migrationsFS, p)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
		}
		for _, m := range createTableRe.FindAllStringSubmatch(stripSQLComments(string(body)), -1) {
			name := strings.ToLower(m[1])
			if seen[name] || strings.HasPrefix(name, "sys_") || sqlx.IsSharedTable(name) {
				continue
			}
			seen[name] = true
			missing = append(missing, name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk migrations: %v", err)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("migrations create %v, which no sqlx.RegisterSharedTable call covers; "+
			"every tenant-scoped query against them fails on MySQL and MSSQL", missing)
	}
}

// stripSQLComments removes -- line comments so a table name mentioned in prose
// is not read as a CREATE TABLE.
func stripSQLComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
