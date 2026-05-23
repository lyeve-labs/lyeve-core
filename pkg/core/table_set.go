package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
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

// catalogSchema is the predicate that holds a catalog read to the schema an
// unqualified name resolves to on the connection. On MySQL information_schema
// spans every database on the server, so the read is held to the one the
// connection is using.
func catalogSchema(dialect string) string {
	switch dialect {
	case "mysql":
		return "table_schema = DATABASE()"
	case "mssql":
		return "table_schema = SCHEMA_NAME()"
	default:
		return "table_schema = current_schema()"
	}
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
	rows, err := q.Query(ctx, `SELECT table_name FROM information_schema.tables
WHERE `+catalogSchema(dialect)+` AND LOWER(table_name) IN (`+strings.Join(marks, ", ")+`)`, args...)
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
	var args []any
	var marks []string
	for _, c := range columns {
		if k := strings.ToLower(c.table); !tableSeen[k] {
			tableSeen[k] = true
			args = append(args, k)
			marks = append(marks, fmt.Sprintf("$%d", len(args)))
		}
	}
	rows, err := q.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
WHERE `+catalogSchema(dialect)+` AND LOWER(table_name) IN (`+strings.Join(marks, ", ")+`)`, args...)
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
