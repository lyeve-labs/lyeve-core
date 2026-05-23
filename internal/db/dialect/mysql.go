package dialect

import (
	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// MySQL implements Dialect for MySQL 8+ / MariaDB 10.3+. It has no TIMESTAMPTZ,
// so timestamps use DATETIME and store UTC. UUIDs use CHAR(36) with UUID() in
// place of gen_random_uuid().
type MySQL struct{}

// Name returns the canonical driver name for MySQL.
func (MySQL) Name() string { return "mysql" }

// Placeholder returns a ? for MySQL positional placeholders (not numbered).
func (MySQL) Placeholder(_ int) string { return "?" }

// NowFunc returns a SQL expression for the current MySQL timestamp.
func (MySQL) NowFunc() string { return "CURRENT_TIMESTAMP(6)" }

// QuoteIdentifier wraps a MySQL identifier in backticks.
// Delegates to sqldialect.QuoteIdentifier for the mysql dialect.
func (MySQL) QuoteIdentifier(name string) string {
	return sqldialect.QuoteIdentifier(sqldialect.DialectMySQL, name)
}
