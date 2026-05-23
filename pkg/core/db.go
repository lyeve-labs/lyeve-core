package core

import (
	"context"
	"time"
)

// Querier is a minimal database interface hiding the pgx driver.
// It covers the operations plugins need: read, write, transactions.
//
// QueryRow never returns a nil Row, even alongside a non-nil error:
// implementations return ErrorRow so the error surfaces from Scan. Callers
// rely on that and scan without checking the error first.
// A fake that returns a nil Row breaks the contract and panics them.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) (Row, error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	Begin(ctx context.Context) (Tx, error)
}

// Row is a single-row query result.
type Row interface {
	Scan(dest ...any) error
}

// ErrorRow returns a Row whose Scan reports err. Querier implementations use
// it instead of a nil Row when QueryRow fails, so a caller that scans without
// checking the error gets that error back rather than a nil dereference.
func ErrorRow(err error) Row { return errRow{err: err} }

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

// Rows is a multi-row query result.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// CommandTag holds the result of an Exec call.
type CommandTag struct {
	RowsAffected int64
}

// Tx is a database transaction.
// It embeds Querier so all read/write operations are available within the tx.
type Tx interface {
	Querier
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Config provides read-only access to server configuration.
// Plugins must not cache values: call on each use.
//
// Methods:
//   - String - single string value (e.g. database_url, jwt_secret)
//   - Bool - boolean flag (e.g. secure_cookie, multi_tenant)
//   - Duration - time.Duration value (e.g. cache_ttl)
//   - Strings - comma-separated list (e.g. jwt_secrets for rotation,
//     cors_origins, trusted_issuers)
type Config interface {
	String(key string) string
	Bool(key string) bool
	Duration(key string) time.Duration

	// Strings returns a comma-separated list value as a slice. Used for things like
	// jwt_secrets that support rotation. Returns empty slice when the key is unset
	// or empty.
	Strings(key string) []string
}

// IsolationLevel names a transaction isolation level a caller can ask for.
// Only the levels the engine actually needs are listed.
type IsolationLevel int

const (
	// IsolationDefault leaves the engine's default in place.
	IsolationDefault IsolationLevel = iota
	// IsolationReadCommitted asks for READ COMMITTED. On MySQL this is what
	// stops a range delete gap-locking rows it is not deleting. Postgres and
	// SQL Server already run at this level.
	IsolationReadCommitted
)

// IsolatedTxBeginner is implemented by a Querier that can start a transaction
// at a chosen isolation level. Optional: type-assert for it and fall back to
// Begin when it is absent, which is what a read-only or capability-denied
// Querier will be.
type IsolatedTxBeginner interface {
	BeginIsolated(ctx context.Context, level IsolationLevel) (Tx, error)
}
