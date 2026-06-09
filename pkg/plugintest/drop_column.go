package plugintest

import (
	"context"
	"errors"
	"regexp"

	"github.com/go-sql-driver/mysql"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// mysqlIndexNeededByForeignKey is the error MySQL returns for dropping the
// only index a foreign key can use.
const mysqlIndexNeededByForeignKey = 1553

var plainIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DropColumn removes column from table on host, with every index and
// constraint that names it, so a test can stand where an install stood
// before the migration that added the column. Code that runs against a
// plugin's tables whether or not the plugin starts has to cope with tables
// at an older version, and this is how a test puts them there.
//
// Postgres drops every index that names the column with it. MySQL only
// narrows a composite index, which would leave a unique key the old schema
// never had, and SQL Server refuses while an index, a default or a check
// names the column, so on both those go first. An index a foreign key
// needs cannot go on MySQL, so the column drop narrows that one instead.
func DropColumn(t T, host core.Host, table, column string) {
	t.Helper()
	if !plainIdent.MatchString(table) || !plainIdent.MatchString(column) {
		t.Fatalf("DropColumn: %q.%q is not a plain identifier", table, column)
		return
	}
	ctx := context.Background()
	q := host.Querier(ctx)
	if host.Dialect() == "mysql" {
		rows, err := q.Query(ctx, `SELECT DISTINCT index_name FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = $1 AND column_name = $2 AND index_name <> 'PRIMARY'`, table, column)
		if err != nil {
			t.Fatalf("DropColumn: list indexes on %s.%s: %v", table, column, err)
			return
		}
		var indexes []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				t.Fatalf("DropColumn: scan index: %v", err)
				return
			}
			indexes = append(indexes, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatalf("DropColumn: list indexes on %s.%s: %v", table, column, err)
			return
		}
		for _, name := range indexes {
			_, err := q.Exec(ctx, "ALTER TABLE "+table+" DROP INDEX `"+name+"`")
			var myErr *mysql.MySQLError
			if errors.As(err, &myErr) && myErr.Number == mysqlIndexNeededByForeignKey {
				continue
			}
			if err != nil {
				t.Fatalf("DropColumn: drop index %s: %v", name, err)
				return
			}
		}
	}
	if host.Dialect() == "mssql" {
		batch := `DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql = @sql + N'DROP INDEX ' + QUOTENAME(i.name) + N' ON ` + table + `;'
FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = OBJECT_ID(N'` + table + `') AND c.name = N'` + column + `'
  AND i.is_primary_key = 0 AND i.is_unique_constraint = 0;
SELECT @sql = @sql + N'ALTER TABLE ` + table + ` DROP CONSTRAINT ' + QUOTENAME(k.name) + N';'
FROM sys.key_constraints k
JOIN sys.index_columns ic ON ic.object_id = k.parent_object_id AND ic.index_id = k.unique_index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE k.parent_object_id = OBJECT_ID(N'` + table + `') AND c.name = N'` + column + `';
SELECT @sql = @sql + N'ALTER TABLE ` + table + ` DROP CONSTRAINT ' + QUOTENAME(d.name) + N';'
FROM sys.default_constraints d
JOIN sys.columns c ON c.object_id = d.parent_object_id AND c.column_id = d.parent_column_id
WHERE d.parent_object_id = OBJECT_ID(N'` + table + `') AND c.name = N'` + column + `';
SELECT @sql = @sql + N'ALTER TABLE ` + table + ` DROP CONSTRAINT ' + QUOTENAME(k.name) + N';'
FROM sys.check_constraints k
JOIN sys.columns c ON c.object_id = k.parent_object_id AND c.column_id = k.parent_column_id
WHERE k.parent_object_id = OBJECT_ID(N'` + table + `') AND c.name = N'` + column + `';
EXEC sp_executesql @sql;`
		if _, err := q.Exec(ctx, batch); err != nil {
			t.Fatalf("DropColumn: drop what names %s.%s: %v", table, column, err)
			return
		}
	}
	if _, err := q.Exec(ctx, `ALTER TABLE `+table+` DROP COLUMN `+column); err != nil {
		t.Fatalf("DropColumn: %s.%s: %v", table, column, err)
	}
}
