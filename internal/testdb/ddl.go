package testdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// Registered DDL is how a suite gets a table that no migration it runs
// creates, such as a table another plugin owns that the code under test
// writes to. Each registration runs on every database this package creates,
// after the engine's migrations, so a plugin's own migrations can already
// refer to the table.

// ddlEntry is one registration: the owner of the tables, and the statements
// that create them on a dialect.
type ddlEntry struct {
	owner string
	ddl   func(dialect string) []string
}

var registeredDDL struct {
	mu      sync.Mutex
	entries []ddlEntry
	owners  map[string]bool
}

// RegisterDDL adds statements that create tables for owner, the plugin that
// owns them. ddl returns the statements for a dialect, "postgres", "mysql"
// or "mssql", and nil for a dialect it does not cover. Every statement must
// be safe to run on a database that already has the table, because two
// owners may declare the same one.
//
// An owner registers once. A later registration under the same owner is
// ignored, so two suites of one binary that need one plugin's tables can
// both declare them. Register from init or TestMain, so the statements land
// in the Postgres template every test database is cloned from. A later
// registration still reaches every database created after it.
//
// It panics on an empty owner or a nil ddl.
func RegisterDDL(owner string, ddl func(dialect string) []string) {
	if owner == "" || ddl == nil {
		panic("testdb.RegisterDDL: owner and ddl are both required")
	}
	registeredDDL.mu.Lock()
	defer registeredDDL.mu.Unlock()
	if registeredDDL.owners[owner] {
		return
	}
	if registeredDDL.owners == nil {
		registeredDDL.owners = map[string]bool{}
	}
	registeredDDL.owners[owner] = true
	registeredDDL.entries = append(registeredDDL.entries, ddlEntry{owner: owner, ddl: ddl})
}

// registeredDDLSince returns the registrations made after the first from.
func registeredDDLSince(from int) []ddlEntry {
	registeredDDL.mu.Lock()
	defer registeredDDL.mu.Unlock()
	if from >= len(registeredDDL.entries) {
		return nil
	}
	return slices.Clone(registeredDDL.entries[from:])
}

// applyRegisteredDDL runs every registration after the first from on db and
// returns how many registrations db now carries.
//
// MySQL has no CREATE INDEX IF NOT EXISTS, so on MySQL an index that already
// exists is skipped rather than failed. That is the one way a statement safe
// to repeat on the other dialects cannot be written on MySQL.
func applyRegisteredDDL(ctx context.Context, db *sql.DB, dialect string, from int) (int, error) {
	entries := registeredDDLSince(from)
	for _, e := range entries {
		for _, stmt := range e.ddl(dialect) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				if dialect == "mysql" && isDuplicateIndexName(err) {
					continue
				}
				return 0, fmt.Errorf("registered DDL of %s on %s: %w\nSQL: %s", e.owner, dialect, err, stmt)
			}
		}
	}
	return from + len(entries), nil
}

// applyRegisteredDDLAt is applyRegisteredDDL on a database named by DSN. The
// connection is closed before it returns, because a Postgres template cannot
// be cloned while anyone is connected to it.
func applyRegisteredDDLAt(driver, dsn, dialect string) (int, error) {
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		return 0, fmt.Errorf("open %s for registered DDL: %w", dialect, err)
	}
	defer conn.Close()
	return applyRegisteredDDL(context.Background(), conn, dialect, 0)
}

// isDuplicateIndexName reports MySQL's refusal of an index name the table
// already carries, error 1061.
func isDuplicateIndexName(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1061
}
