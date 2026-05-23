package db

import (
	"context"
	"database/sql"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// NoDatabase is the DB of an engine running in stateless mode. Every call
// fails with core.ErrNoDatabase, so a plugin store that was not replaced
// reports the mode instead of dereferencing a nil pool.
func NoDatabase() DB { return noDatabase{} }

type noDatabase struct{}

var _ DB = noDatabase{}

func (noDatabase) QueryRow(context.Context, string, ...any) (*sql.Row, error) {
	return nil, core.ErrNoDatabase
}

func (noDatabase) Query(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, core.ErrNoDatabase
}

func (noDatabase) Exec(context.Context, string, ...any) (sql.Result, error) {
	return nil, core.ErrNoDatabase
}

func (noDatabase) Begin(context.Context) (*sql.Tx, error) { return nil, core.ErrNoDatabase }

func (noDatabase) Conn(context.Context) (*sql.Conn, error) { return nil, core.ErrNoDatabase }

func (noDatabase) Ping(context.Context) error { return core.ErrNoDatabase }

func (noDatabase) Close() error { return nil }

func (noDatabase) Stats() sql.DBStats { return sql.DBStats{} }

// Engine is empty: no dialect applies, and a plugin that branches on one
// takes none of its branches.
func (noDatabase) Engine() string { return "" }

func (noDatabase) SQLDB() *sql.DB { return nil }

func (noDatabase) QuerierRO(context.Context) (ReadOnlyQuerier, error) {
	return nil, core.ErrNoDatabase
}
