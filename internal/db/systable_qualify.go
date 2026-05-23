package db

import (
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// sysTablePrefix is the prefix shared by every catalog table the engine and
// its plugins own. Those tables are created once, in the engine's own
// database, and are isolated by a tenant_id column rather than by schema.
const sysTablePrefix = "sys_"

// QualifySysTables names the engine's own database explicitly on every sys_*
// table reference, for the engines whose tenancy strategy switches databases.
//
// A tenant-scoped connection on MySQL has run `USE tenant_<slug>`, and on SQL
// Server `USE [tenant_<slug>]`. Neither engine has anything behind that to fall
// through to, so an unqualified sys_* reference resolves inside the tenant
// database, where no sys_* table was ever created:
//
//	Error 1146 (42S02): Table 'tenant_acme.sys_users' doesn't exist
//	Msg 208: Invalid object name 'sys_users'.
//
// PostgreSQL never hit this because its Apply sets `search_path =
// "tenant_<slug>", public` and the same reference falls through to public.
// Qualifying is what buys MySQL and SQL Server that fall-through, and it is the
// only thing that can: the tables genuinely live in one database, and the USE
// is what holds the isolation boundary for the dynamic per-tenant tables, so it
// cannot be dropped.
//
// Applied to every statement, not only the tenant-scoped ones. Off the tenant
// path the connection is already on dbName, so the qualified name and the bare
// one address the same table. Running one rule everywhere means the rewrite is
// exercised by every MySQL and MSSQL test rather than only by the multi-tenant
// ones. PostgreSQL and an unknown database name are both passthrough.
//
// Only the reference in table position is rewritten:
//
//   - An identifier preceded by a dot is already qualified by its writer.
//   - An identifier followed by a dot is a column reference (sys_users.email).
//     It keeps resolving untouched, because a table addressed by a qualified
//     name still exposes its own name as the correlation name for its columns.
//
// String literals and comments are copied verbatim, so a table name that
// appears as data (`WHERE table_name = 'sys_users'`, which is how the
// information_schema probes read) is left alone.
func QualifySysTables(q, engine, dbName string) string {
	prefix, ok := sysQualifier(engine, dbName)
	if !ok || (!strings.Contains(q, sysTablePrefix) && !sqlx.HasSharedTables()) {
		return q
	}

	var b strings.Builder
	b.Grow(len(q) + len(prefix))
	// prevSignificant is the last non-whitespace byte written, which is how a
	// reference somebody else already qualified is recognized.
	var prevSignificant byte
	i := 0
	for i < len(q) {
		c := q[i]
		switch {
		case c == '\'':
			j := scanQuoted(q, i, '\'')
			b.WriteString(q[i:j])
			i, prevSignificant = j, '\''
		case c == '"':
			// Ambiguous: an identifier on SQL Server, a string literal on MySQL
			// unless ANSI_QUOTES is set. Copied verbatim either way, which
			// costs nothing because no runtime statement writes a sys_* name
			// that way.
			j := scanQuoted(q, i, '"')
			b.WriteString(q[i:j])
			i, prevSignificant = j, '"'
		case c == '`' || (c == '[' && engine == "mssql"):
			closer := byte('`')
			if c == '[' {
				closer = ']'
			}
			j := scanQuoted(q, i, closer)
			// The delimiters and any doubled-delimiter escapes inside are
			// reused verbatim rather than re-quoted, so the writer's own
			// escaping survives untouched.
			if closed(q, i, j, closer) && qualifiable(q[i+1:j-1], prevSignificant, q, j) {
				b.WriteString(prefix)
			}
			b.WriteString(q[i:j])
			i, prevSignificant = j, closer
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			j := i
			for j < len(q) && q[j] != '\n' {
				j++
			}
			b.WriteString(q[i:j])
			i = j
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			j := i + 2
			for j+1 < len(q) && (q[j] != '*' || q[j+1] != '/') {
				j++
			}
			end := j + 2
			if j+1 >= len(q) {
				end = len(q)
			}
			b.WriteString(q[i:end])
			i = end
		case isIdentStart(c):
			j := i + 1
			for j < len(q) && isIdentPart(q[j]) {
				j++
			}
			name := q[i:j]
			if qualifiable(name, prevSignificant, q, j) {
				b.WriteString(prefix)
				b.WriteString(quoteSysIdentifier(name, engine))
			} else {
				b.WriteString(name)
			}
			i, prevSignificant = j, name[len(name)-1]
		default:
			b.WriteByte(c)
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				prevSignificant = c
			}
			i++
		}
	}
	return b.String()
}

// sysQualifier returns the text to put in front of a sys_* table name, and
// whether this engine and database name are ones that need it.
//
// MySQL takes `db`.`table`. SQL Server takes [db]..[table] rather than
// [db].[dbo].[table]: the empty middle part resolves against the caller's own
// default schema exactly as an unqualified name would, so the rewrite does not
// have to assume every deployment creates its tables under dbo.
//
// The database name goes through safeDatabaseNameRe before it reaches SQL, the
// same gate the tenancy strategies put it behind. An unreportable name (the
// server refused to answer at boot) leaves every statement unrewritten rather
// than failing it.
func sysQualifier(engine, dbName string) (string, bool) {
	if !safeDatabaseNameRe.MatchString(dbName) {
		return "", false
	}
	switch engine {
	case "mysql":
		return quoteMySQLIdentifier(dbName) + ".", true
	case "mssql":
		return quoteMSSQLIdentifier(dbName) + "..", true
	default:
		return "", false
	}
}

func quoteSysIdentifier(name, engine string) string {
	if engine == "mssql" {
		return quoteMSSQLIdentifier(name)
	}
	return quoteMySQLIdentifier(name)
}

// closed reports whether the region q[i:j] is a delimited identifier that
// actually reached its closing delimiter, rather than one scanQuoted ran off
// the end of the input looking for.
func closed(q string, i, j int, closer byte) bool {
	return j-i >= 2 && q[j-1] == closer
}

// qualifiable reports whether name, ending at index end in q, is a shared
// table reference in table position: a sys_* catalog table, or a plugin table
// registered via sqlx.RegisterSharedTable. Identifiers that are already
// qualified (preceded by a dot) or column references (followed by a dot) are
// never rewritten.
func qualifiable(name string, prevSignificant byte, q string, end int) bool {
	if prevSignificant == '.' {
		return false
	}
	if end < len(q) && q[end] == '.' {
		return false
	}
	if strings.HasPrefix(name, sysTablePrefix) && len(name) > len(sysTablePrefix) {
		return true
	}
	return sqlx.IsSharedTable(name)
}

// scanQuoted returns the index one past the closing delimiter of the region
// starting at i, whose opening delimiter q[i] is closed by closer. A doubled
// closer is the SQL escape for a literal one and does not end the region. An
// unterminated region runs to the end of the input, which keeps a malformed
// statement byte-identical rather than half-rewritten.
func scanQuoted(q string, i int, closer byte) int {
	for j := i + 1; j < len(q); j++ {
		if q[j] != closer {
			continue
		}
		if j+1 < len(q) && q[j+1] == closer {
			j++
			continue
		}
		return j + 1
	}
	return len(q)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isASCIIDigit(c) }
