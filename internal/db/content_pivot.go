package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// pivotTable returns the table the many_to_many f of schemaName keeps its
// pairs in, unqualified: the one the field names, or the one
// core.ResolvePivotTable resolves, which is the one the schema engine
// created. A long pair costs a catalog read, which tells a table an older
// install created under the full name from one created under the bounded
// name.
func (s *ContentStore) pivotTable(ctx context.Context, schemaName string, f domain.SchemaField) (string, error) {
	if f.RelationThrough != "" {
		return f.RelationThrough, nil
	}
	name, err := core.ResolvePivotTable(schemaName, f.RelationTo, func(natural string) (bool, error) {
		return s.pivotPresent(ctx, natural, pivotColumn(schemaName), pivotColumn(f.RelationTo))
	})
	if err != nil {
		return "", fmt.Errorf("pivot of %s.%s: %w", schemaName, f.Name, err)
	}
	return name, nil
}

// pivotColumn is the key column a pivot gives the type schemaName.
func pivotColumn(schemaName string) string {
	return strings.TrimPrefix(domain.TableName(schemaName), "_") + "_id"
}

// pivotPresent reports whether the engine's own database holds table with
// both key columns of the pair. PostgreSQL stored a long name under its first
// 63 bytes, so the columns tell this pair's table from one another pair's
// name was cut to. It reads the engine's database by name on MySQL and SQL
// Server, where a tenant connection may have switched to another one.
func (s *ContentStore) pivotPresent(ctx context.Context, table, aCol, bCol string) (bool, error) {
	s.dbNameOnce.Do(func() { s.dbName = DatabaseNameOf(s.pool) })
	qualified := safeDatabaseNameRe.MatchString(s.dbName)
	want := 2
	if aCol == bCol {
		want = 1
	}
	var (
		q    string
		args []any
	)
	switch s.pool.Engine() {
	case "mysql":
		q = `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = $1 AND COLUMN_NAME IN ($2, $3)`
		args = []any{table, aCol, bCol}
		if qualified {
			q = `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = $1 AND TABLE_NAME = $2 AND COLUMN_NAME IN ($3, $4)`
			args = []any{s.dbName, table, aCol, bCol}
		}
	case "mssql":
		obj, cat := table, "sys.columns"
		if qualified {
			obj, cat = s.dbName+".."+table, quoteMSSQLIdentifier(s.dbName)+".sys.columns"
		}
		q = `SELECT COUNT(*) FROM ` + cat + ` WHERE object_id = OBJECT_ID($1, N'U') AND name IN ($2, $3)`
		args = []any{obj, aCol, bCol}
	default:
		args = []any{postgresStored(table), postgresStored(aCol), postgresStored(bCol)}
		q = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name IN ($2, $3)`
	}
	row, err := s.pool.QueryRow(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("read pivot %s: %w", table, err)
	}
	var n int
	if err := row.Scan(&n); err != nil {
		return false, fmt.Errorf("read pivot %s: %w", table, err)
	}
	return n >= want, nil
}

// postgresStored is name as PostgreSQL keeps it: its first 63 bytes.
func postgresStored(name string) string {
	if len(name) > core.PivotNameLimit {
		return name[:core.PivotNameLimit]
	}
	return name
}
