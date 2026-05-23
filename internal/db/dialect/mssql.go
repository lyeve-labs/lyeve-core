package dialect

import (
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// MSSQL implements Dialect for Microsoft SQL Server 2019+ / Azure SQL.
// Parameter placeholders are @p1, @p2, ...
// UUID storage uses UNIQUEIDENTIFIER. Default is NEWID().
type MSSQL struct{}

// Name returns the canonical driver name for MSSQL.
func (MSSQL) Name() string { return "mssql" }

// Placeholder returns an MSSQL-style named parameter (@pN).
func (MSSQL) Placeholder(n int) string { return fmt.Sprintf("@p%d", n) }

// NowFunc returns a SQL expression for the current UTC timestamp in MSSQL.
func (MSSQL) NowFunc() string { return "GETUTCDATE()" }

// QuoteIdentifier wraps an MSSQL identifier in square brackets.
// Delegates to sqldialect.QuoteIdentifier for the mssql dialect.
func (MSSQL) QuoteIdentifier(name string) string {
	return sqldialect.QuoteIdentifier(sqldialect.DialectMSSQL, name)
}
