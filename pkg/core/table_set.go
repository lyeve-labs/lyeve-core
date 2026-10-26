package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// TableSetState is what the catalog says about a TableSet.
type TableSetState int

const (
	// TableSetAbsent means none of the tables exists, because the plugin
	// that owns them never ran here.
	TableSetAbsent TableSetState = iota
	// TableSetUnkeyed means the tables exist and none of the set's columns
	// does, because the plugin last ran before the migration that added them.
	TableSetUnkeyed
	// TableSetReady means every table and every column exists.
	TableSetReady
)

// TableSet is a group of tables one migration created, together with the
// columns a later migration added to them. Code that runs against a plugin's
// tables whether or not the plugin starts reads the set first, because the
// plugin may never have run here, or may have last run at an older version.
type TableSet struct {
	tables  []string
	columns []tableColumn
}

type tableColumn struct{ table, column string }

// NewTableSet builds a set from table names and from columns written as
// "table.column". A column's table belongs to the set whether or not tables
// names it. Duplicates are dropped, and names compare without case.
func NewTableSet(tables, columns []string) TableSet {
	var s TableSet
	seen := make(map[string]bool, len(tables)+len(columns))
	add := func(t string) {
		if k := strings.ToLower(t); t != "" && !seen[k] {
			seen[k] = true
			s.tables = append(s.tables, t)
		}
	}
	for _, t := range tables {
		add(t)
	}
	seenCol := make(map[tableColumn]bool, len(columns))
	for _, c := range columns {
		table, column, ok := strings.Cut(c, ".")
		if !ok || table == "" || column == "" {
			continue
		}
		add(table)
		key := tableColumn{strings.ToLower(table), strings.ToLower(column)}
		if !seenCol[key] {
			seenCol[key] = true
			s.columns = append(s.columns, tableColumn{table, column})
		}
	}
	return s
}

// Tables returns the set's tables, including those its columns name.
func (s TableSet) Tables() []string { return append([]string(nil), s.tables...) }

// Check reads the catalog on q and reports the set's state. A set with some
// of its tables absent, or some of its columns absent, is an error naming
// them: code written for the whole set would fail partway on the rest.
//
// Existence is read rather than a failed statement caught, because on
// Postgres a failed statement aborts the whole transaction around it.
func (s TableSet) Check(ctx context.Context, q Querier, dialect string) (TableSetState, error) {
	if len(s.tables) == 0 {
		return TableSetAbsent, nil
	}
	present, err := presentTables(ctx, q, dialect, s.tables)
	if err != nil {
		return TableSetAbsent, err
	}
	if len(present) == 0 {
		return TableSetAbsent, nil
	}
	if len(present) < len(s.tables) {
		var missing []string
		for _, t := range s.tables {
			if !present[strings.ToLower(t)] {
				missing = append(missing, t)
			}
		}
		sort.Strings(missing)
		return TableSetAbsent, fmt.Errorf("%s absent beside %d present, so the set cannot be read whole",
			strings.Join(missing, ", "), len(present))
	}
	if len(s.columns) == 0 {
		return TableSetReady, nil
	}
	found, err := presentColumns(ctx, q, dialect, s.columns)
	if err != nil {
		return TableSetAbsent, err
	}
	switch len(found) {
	case 0:
		return TableSetUnkeyed, nil
	case len(s.columns):
		return TableSetReady, nil
	}
	var missing []string
	for _, c := range s.columns {
		if !found[tableColumn{strings.ToLower(c.table), strings.ToLower(c.column)}] {
			missing = append(missing, c.table+"."+c.column)
		}
	}
	sort.Strings(missing)
	return TableSetAbsent, fmt.Errorf("%s absent beside %d present, so the set cannot be read whole",
		strings.Join(missing, ", "), len(found))
}

// catalogRead builds the MySQL and SQL Server read of an information_schema
// view for tables, holding each name to the database a statement naming it
// reaches. A tenant-scoped connection there has switched to the tenant's
// database, and the querier qualifies every engine table (sys_* and the
// registered shared tables) with the engine's own database, so an engine
// table is read in that database and every other table in the current one.
// A querier that cannot name the engine's database reads every table in the
// current one. On MySQL information_schema spans every database on the
// server, so the read names the database. On SQL Server each database has its
// own information_schema, so the read is made in the engine's.
func catalogRead(q Querier, dialect, columns, view string, tables []string) (string, []any) {
	sysDB := sysDatabaseOf(q)
	var engine, current []string
	for _, t := range tables {
		if sysDB != "" && engineTable(t) {
			engine = append(engine, t)
		} else {
			current = append(current, t)
		}
	}
	var args []any
	marks := func(names []string) string {
		m := make([]string, len(names))
		for i, n := range names {
			args = append(args, strings.ToLower(n))
			m[i] = fmt.Sprintf("$%d", len(args))
		}
		return strings.Join(m, ", ")
	}
	var parts []string
	if len(engine) > 0 {
		from, schema := "information_schema."+view, "table_schema = SCHEMA_NAME()"
		if dialect == "mysql" {
			args = append(args, sysDB)
			schema = fmt.Sprintf("table_schema = $%d", len(args))
		} else {
			from = "[" + strings.ReplaceAll(sysDB, "]", "]]") + "]." + from
		}
		parts = append(parts, `SELECT `+columns+` FROM `+from+catalogHint(dialect)+`
WHERE `+schema+` AND LOWER(table_name) IN (`+marks(engine)+`)`)
	}
	if len(current) > 0 {
		schema := "table_schema = SCHEMA_NAME()"
		if dialect == "mysql" {
			schema = "table_schema = DATABASE()"
		}
		parts = append(parts, `SELECT `+columns+` FROM information_schema.`+view+catalogHint(dialect)+`
WHERE `+schema+` AND LOWER(table_name) IN (`+marks(current)+`)`)
	}
	return strings.Join(parts, "\nUNION ALL\n"), args
}

// sysDatabaseOf names the database the engine's own tables live in, or "" when
// q cannot say. A querier that wraps another passes the question through.
func sysDatabaseOf(q Querier) string {
	if n, ok := q.(interface{ SysDatabase() string }); ok {
		return n.SysDatabase()
	}
	return ""
}

// engineTable reports whether the querier qualifies a statement's reference
// to name with the engine's own database.
func engineTable(name string) bool {
	return (strings.HasPrefix(name, "sys_") && len(name) > len("sys_")) || sqlx.IsSharedTable(name)
}

// pgTables and pgColumns read the PostgreSQL catalog for the relation each
// unqualified name resolves to. A tenant-scoped connection searches the
// tenant's schema and then public, so current_schema() names the tenant's
// schema while an unqualified sys_* table resolves to public, and a read held
// to current_schema() reported the engine's tables absent. A relation is
// visible when it is the first one of its name on the search path, which is
// the one an unqualified statement reaches.
const (
	pgTables = `SELECT c.relname FROM pg_catalog.pg_class c
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND pg_catalog.pg_table_is_visible(c.oid) AND LOWER(c.relname) IN (%s)`
	pgColumns = `SELECT c.relname, a.attname FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND pg_catalog.pg_table_is_visible(c.oid) AND a.attnum > 0 AND NOT a.attisdropped AND LOWER(c.relname) IN (%s)`
)

// catalogHint is the table hint a catalog read takes. On SQL Server a catalog
// read takes locks that row versioning does not cover, so it waits on a table
// another transaction created and has not committed. A tenant delete reads
// the catalog while it holds the tenant's rows, and a schema change that then
// waits on one of those rows closes a cycle the server breaks by failing one
// of the two, often the delete. The read asks only whether tables a migration
// created long before exist, which no uncommitted change decides, so on SQL
// Server it reads without those locks.
func catalogHint(dialect string) string {
	if dialect == "mssql" {
		return " WITH (NOLOCK)"
	}
	return ""
}

// presentTables reports which of tables exist in the schema an unqualified
// name resolves to on q.
func presentTables(ctx context.Context, q Querier, dialect string, tables []string) (map[string]bool, error) {
	args := make([]any, len(tables))
	marks := make([]string, len(tables))
	for i, t := range tables {
		args[i] = strings.ToLower(t)
		marks[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf(pgTables, strings.Join(marks, ", "))
	if dialect == "mysql" || dialect == "mssql" {
		query, args = catalogRead(q, dialect, "table_name", "tables", tables)
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	present := make(map[string]bool, len(tables))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tables: %w", err)
		}
		present[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return present, nil
}

// presentColumns reports which of columns exist, keyed in lower case.
func presentColumns(ctx context.Context, q Querier, dialect string, columns []tableColumn) (map[tableColumn]bool, error) {
	tableSeen := make(map[string]bool, len(columns))
	var tables []string
	var args []any
	var marks []string
	for _, c := range columns {
		if k := strings.ToLower(c.table); !tableSeen[k] {
			tableSeen[k] = true
			tables = append(tables, c.table)
			args = append(args, k)
			marks = append(marks, fmt.Sprintf("$%d", len(args)))
		}
	}
	query := fmt.Sprintf(pgColumns, strings.Join(marks, ", "))
	if dialect == "mysql" || dialect == "mssql" {
		query, args = catalogRead(q, dialect, "table_name, column_name", "columns", tables)
	}
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list columns: %w", err)
	}
	defer rows.Close()
	want := make(map[tableColumn]bool, len(columns))
	for _, c := range columns {
		want[tableColumn{strings.ToLower(c.table), strings.ToLower(c.column)}] = true
	}
	found := make(map[tableColumn]bool, len(columns))
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("scan columns: %w", err)
		}
		if k := (tableColumn{strings.ToLower(table), strings.ToLower(column)}); want[k] {
			found[k] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list columns: %w", err)
	}
	return found, nil
}
