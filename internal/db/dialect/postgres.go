package dialect

import (
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// Postgres implements Dialect for PostgreSQL (the primary supported engine).
type Postgres struct{}

// Name returns the canonical driver name for PostgreSQL.
func (Postgres) Name() string { return "postgres" }

// Placeholder returns the Postgres-style positional bind parameter ($n).
func (Postgres) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }

// NowFunc returns a SQL expression for the current PostgreSQL timestamp.
func (Postgres) NowFunc() string { return "NOW()" }

// QuoteIdentifier wraps a Postgres identifier in double quotes.
// Delegates to sqldialect.QuoteIdentifier for the postgres dialect.
func (Postgres) QuoteIdentifier(name string) string {
	return sqldialect.QuoteIdentifier(sqldialect.DialectPostgres, name)
}
