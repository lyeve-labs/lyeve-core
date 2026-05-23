// Package dialect abstracts SQL syntax differences between database engines.
// All schema-generating and query-building code should use the Dialect
// interface rather than hard-coding PostgreSQL syntax so that MySQL and MSSQL
// support can be added without forking large blocks of logic.
package dialect

import (
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// Dialect describes the SQL dialect of a target database engine.
//
// It is sqldialect.Syntax and nothing more: the query-building surface the
// kernel's own queries need. Column types and other DDL forms belong to the
// schema engine that generates DDL.
type Dialect interface {
	sqldialect.Syntax
}

// New returns the Dialect implementation for the named driver.
// Supported drivers: "postgres" (default), "mysql", "mssql".
func New(driver string) (Dialect, error) {
	switch driver {
	case "", "postgres", "postgresql":
		return Postgres{}, nil
	case "mysql":
		return MySQL{}, nil
	case "mssql", "sqlserver":
		return MSSQL{}, nil
	default:
		return nil, fmt.Errorf("dialect: unsupported driver %q", driver)
	}
}

// Must is like New but panics on error. Use only during initialization.
//
// Panic is intentional: this is for init-time and test code where the
// driver name is a compile-time constant (e.g. "postgres", "mysql"). If
// the driver comes from user configuration, use New() and handle the error.
func Must(driver string) Dialect {
	d, err := New(driver)
	if err != nil {
		panic(err)
	}
	return d
}
