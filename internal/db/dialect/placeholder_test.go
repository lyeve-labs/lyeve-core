// Exact-string assertions for each engine's placeholder.
//
// Each case asserts the verbatim string the implementation produces for
// Postgres, MySQL, and MSSQL, so a change to any engine's output is caught
// immediately.

package dialect_test

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

func TestPlaceholder_AllDialects(t *testing.T) {
	tests := []struct {
		name string
		d    dialect.Dialect
		n    int
		want string
	}{
		{"postgres n=1", dialect.Postgres{}, 1, "$1"},
		{"postgres n=42", dialect.Postgres{}, 42, "$42"},
		{"mysql n=1 constant", dialect.MySQL{}, 1, "?"},
		{"mysql n=99 ignores index", dialect.MySQL{}, 99, "?"},
		{"mssql n=1", dialect.MSSQL{}, 1, "@p1"},
		{"mssql n=7", dialect.MSSQL{}, 7, "@p7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.Placeholder(tt.n); got != tt.want {
				t.Errorf("%s.Placeholder(%d) = %q, want %q", tt.d.Name(), tt.n, got, tt.want)
			}
		})
	}
}
