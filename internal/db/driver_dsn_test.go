package db

import (
	"strings"
	"testing"
)

// Migration files carry several statements each. Without multiStatements the
// driver sends only the first and MySQL rejects the next as a syntax error, so
// a stock DATABASE_URL could not migrate.
//
// clientFoundRows rides along on the same DSN: it makes MySQL report matched
// rows rather than changed ones, which is what the callers' RowsAffected == 0
// checks assume. Each case carries it because both flags are unconditional.
func TestDriverDSN_MySQLForcesMultiStatements(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "no existing parameters",
			dsn:  "user:pw@tcp(db:3306)/lyeve",
			want: "user:pw@tcp(db:3306)/lyeve?multiStatements=true&clientFoundRows=true",
		},
		{
			name: "existing parameters",
			dsn:  "user:pw@tcp(db:3306)/lyeve?parseTime=true",
			want: "user:pw@tcp(db:3306)/lyeve?parseTime=true&multiStatements=true&clientFoundRows=true",
		},
		{
			name: "scheme is stripped and the flag still added",
			dsn:  "mysql://user:pw@tcp(db:3306)/lyeve",
			want: "user:pw@tcp(db:3306)/lyeve?multiStatements=true&clientFoundRows=true",
		},
		{
			name: "already set is left alone",
			dsn:  "user:pw@tcp(db:3306)/lyeve?multiStatements=true",
			want: "user:pw@tcp(db:3306)/lyeve?multiStatements=true&clientFoundRows=true",
		},
		{
			name: "explicitly disabled is not silently flipped",
			dsn:  "user:pw@tcp(db:3306)/lyeve?multiStatements=false",
			want: "user:pw@tcp(db:3306)/lyeve?multiStatements=false&clientFoundRows=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := driverDSN("mysql", tt.dsn); got != tt.want {
				t.Errorf("driverDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The content read path takes every stored datetime to be UTC, so a DSN that
// asks the driver to write the host's wall clock instead is held to UTC. One
// that names no location already gets the driver's UTC default.
func TestDriverDSN_MySQLHoldsTheLocationAtUTC(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "local is rewritten",
			dsn:  "user:pw@tcp(db:3306)/lyeve?parseTime=true&loc=Local",
			want: "user:pw@tcp(db:3306)/lyeve?parseTime=true&loc=UTC&multiStatements=true&clientFoundRows=true",
		},
		{
			name: "a named zone is rewritten",
			dsn:  "user:pw@tcp(db:3306)/lyeve?loc=Etc%2FGMT%2B3",
			want: "user:pw@tcp(db:3306)/lyeve?loc=UTC&multiStatements=true&clientFoundRows=true",
		},
		{
			name: "no location is left to the default",
			dsn:  "user:pw@tcp(db:3306)/lyeve?parseTime=true",
			want: "user:pw@tcp(db:3306)/lyeve?parseTime=true&multiStatements=true&clientFoundRows=true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := driverDSN("mysql", tt.dsn); got != tt.want {
				t.Errorf("driverDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Only MySQL needs the flag. Adding it elsewhere would corrupt the DSN.
func TestDriverDSN_OtherEnginesUntouched(t *testing.T) {
	for _, engine := range []string{"postgres", "mssql"} {
		t.Run(engine, func(t *testing.T) {
			dsn := "postgres://user:pw@db:5432/lyeve?sslmode=require"
			got := driverDSN(engine, dsn)
			if got != dsn {
				t.Errorf("driverDSN(%s) = %q, want it unchanged", engine, got)
			}
			if strings.Contains(got, "multiStatements") {
				t.Errorf("driverDSN(%s) added multiStatements to a non-MySQL DSN", engine)
			}
		})
	}
}
