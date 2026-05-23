package dialect

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// Quoting is spelled twice: as a method on each engine, and as a package
// function keyed by name. They have to agree, because a caller that holds a
// Dialect and a caller that holds only a name must produce the same SQL.
func TestQuoteIdentifier_MethodAndFunctionAgree(t *testing.T) {
	names := []string{"articles", "weird\"name", "back`tick", "bracket]name"}

	for _, d := range []Dialect{Postgres{}, MySQL{}, MSSQL{}} {
		for _, n := range names {
			method := d.QuoteIdentifier(n)
			fn := sqldialect.QuoteIdentifier(d.Name(), n)
			if method != fn {
				t.Errorf("%s: method quoted %q as %s, function as %s", d.Name(), n, method, fn)
			}
		}
	}
}
