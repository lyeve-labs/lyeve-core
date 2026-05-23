package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// writeLockWait bounds how long a write waits behind another holding the same
// lock. A ceiling write holds it for a count and an insert, so a wait this long
// means the holder is stuck, and the caller is answered with an outage rather
// than queued behind it.
const writeLockWait = 10 * time.Second

// writeLockRelease bounds the MySQL release, which runs on its own context so a
// canceled request still gives the lock back.
const writeLockRelease = 5 * time.Second

// ErrWriteLockTimeout is returned when the lock was not granted within
// writeLockWait. It wraps core.ErrServiceUnavailable, so httpx.StatusFor and
// httpx.StoreStatusFor both answer 503: the holder is stuck, and the caller
// may retry.
var ErrWriteLockTimeout = fmt.Errorf("write lock: not granted in time: %w", core.ErrServiceUnavailable)

// ErrNestedWriteLock is returned when a serialized write asks for a second lock
// or a transaction of its own. The lock already holds a transaction on the
// context, and none of the three engines nests one inside another.
var ErrNestedWriteLock = errors.New("write lock: a serialized write cannot open another transaction")

type writeTxKey struct{}

// writeTx returns the transaction a serialized write holds on ctx, or nil.
func writeTx(ctx context.Context) *sql.Tx {
	tx, _ := ctx.Value(writeTxKey{}).(*sql.Tx)
	return tx
}

// SerializeWrite runs fn inside a transaction that holds an exclusive lock
// named key, so two callers with the same key run one after the other on every
// replica of the install. It is how a ceiling holds: the count and the insert
// both run in fn, and the next caller counts only after the first committed.
//
// Every query fn makes through this pool with the context it is handed runs in
// that transaction, on one connection, so fn needs no querier of its own and a
// pool of N connections serves N waiting writers without starving the holder.
// That connection is the request's tenant connection when there is one, so the
// tenancy the request selected still applies.
//
// The lock lives as long as the transaction:
//
//   - Postgres takes pg_advisory_xact_lock, released by commit or rollback.
//   - SQL Server takes sp_getapplock with @LockOwner='Transaction', released
//     the same way, in the engine's own database whichever one the
//     connection has switched to.
//   - MySQL has no transaction-scoped lock, so GET_LOCK is held by the session
//     and released after the transaction ends, on a context of its own. A
//     release that fails discards the connection, because a pooled session
//     keeps its locks.
//
// fn's error rolls the transaction back and is returned as it is. fn must not
// open a transaction of its own: Begin answers ErrNestedWriteLock.
func SerializeWrite(ctx context.Context, pool DB, key string, fn func(context.Context) error) error {
	if writeTx(ctx) != nil {
		return ErrNestedWriteLock
	}
	return runWriteTx(ctx, pool, key, fn)
}

// WriteInTransaction runs fn in one transaction under SerializeWrite's rules
// but takes no lock: every query fn makes through this pool with the context
// it is handed runs in that transaction, on one connection, and Begin inside
// fn answers ErrNestedWriteLock. fn's error rolls back everything fn wrote.
//
// It is for a write of several statements that must land together and needs
// no ceiling held still. Called inside a serialized write, fn joins that
// write's transaction, and the outer write's commit or rollback decides it.
func WriteInTransaction(ctx context.Context, pool DB, fn func(context.Context) error) error {
	if writeTx(ctx) != nil {
		return fn(ctx)
	}
	return runWriteTx(ctx, pool, "", fn)
}

// runWriteTx runs fn in a transaction on the request's tenant connection or a
// pooled one, holding the write lock named key for the transaction's life. An
// empty key takes no lock.
func runWriteTx(ctx context.Context, pool DB, key string, fn func(context.Context) error) error {
	label := "write transaction"
	if key != "" {
		label = fmt.Sprintf("write lock %q", key)
	}
	conn, err := tenantConn(ctx)
	if err != nil {
		return fmt.Errorf("%s: tenant conn: %w", label, err)
	}
	if conn == nil {
		conn, err = pool.Conn(ctx)
		if err != nil {
			return fmt.Errorf("%s: conn: %w", label, err)
		}
		defer func() { _ = conn.Close() }()
	}
	engine := pool.Engine()
	dbName := ""
	if namer, ok := pool.(interface{ DatabaseName() string }); ok {
		dbName = namer.DatabaseName()
	}
	name := writeLockName(dbName, key)
	locked := key != ""

	if locked && engine == "mysql" {
		if err := mysqlGetLock(ctx, conn, name); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		// Registered before the transaction's rollback, so it runs after it.
		defer mysqlReleaseLock(ctx, conn, name)
	}

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("%s: begin: %w", label, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	switch {
	case !locked, engine == "mysql":
	case engine == "mssql":
		if err := mssqlAppLock(ctx, tx, dbName, name); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	default:
		if err := postgresXactLock(ctx, tx, name); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}

	if err := fn(context.WithValue(ctx, writeTxKey{}, tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", label, err)
	}
	committed = true
	return nil
}

// savepoint runs fn so that a statement failing inside it leaves a serialized
// write's transaction usable. Postgres aborts the whole transaction on any
// failed statement, so without it a write that recovers from a failure, such
// as an insert that lost a race and reads the winner's row back, has every
// later statement refused. MySQL and SQL Server roll back only the statement,
// and take the savepoint all the same so the three behave alike. Outside a
// serialized write fn runs as it is, because each statement is its own
// transaction there.
//
// name must be a plain identifier: it is part of the statement.
func savepoint(ctx context.Context, engine, name string, fn func() error) error {
	tx := writeTx(ctx)
	if tx == nil {
		return fn()
	}
	set, undo, release := "SAVEPOINT "+name, "ROLLBACK TO SAVEPOINT "+name, "RELEASE SAVEPOINT "+name
	if engine == "mssql" {
		// SQL Server names a savepoint through SAVE TRANSACTION and has no
		// release: the savepoint ends with the transaction.
		set, undo, release = "SAVE TRANSACTION "+name, "ROLLBACK TRANSACTION "+name, ""
	}
	if _, err := tx.ExecContext(ctx, set); err != nil {
		return fmt.Errorf("savepoint %s: %w", name, err)
	}
	if err := fn(); err != nil {
		if _, undoErr := tx.ExecContext(ctx, undo); undoErr != nil {
			return errors.Join(err, fmt.Errorf("roll back to savepoint %s: %w", name, undoErr))
		}
		return err
	}
	if release != "" {
		if _, err := tx.ExecContext(ctx, release); err != nil {
			return fmt.Errorf("release savepoint %s: %w", name, err)
		}
	}
	return nil
}

// writeLockName is the lock's name on the server. It is a digest because MySQL
// caps a lock name at 64 characters and shares one namespace across every
// database on the server, so the engine's database is part of what is hashed.
func writeLockName(dbName, key string) string {
	sum := sha256.Sum256([]byte(dbName + "\x00" + key))
	return "lyeve_w_" + hex.EncodeToString(sum[:20])
}

// postgresXactLock waits for the transaction-scoped advisory lock. Advisory
// locks are keyed by a number, taken from the same digest as the name.
func postgresXactLock(ctx context.Context, tx *sql.Tx, name string) error {
	sum := sha256.Sum256([]byte(name))
	k := int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // the bits are the key, the sign is irrelevant
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", writeLockWait.Milliseconds())); err != nil {
		return fmt.Errorf("lock timeout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", k); err != nil { //nolint:raw-tx-placeholder // the Postgres branch only
		var coded interface{ SQLState() string }
		if errors.As(err, &coded) && coded.SQLState() == "55P03" {
			return ErrWriteLockTimeout
		}
		return fmt.Errorf("acquire: %w", err)
	}
	return nil
}

// mssqlAppLock waits for the transaction-owned application lock.
// sp_getapplock answers through its return code: 0 and 1 are granted, -1 is
// the timeout, anything else a refusal.
func mssqlAppLock(ctx context.Context, tx *sql.Tx, dbName, name string) error {
	proc := "sp_getapplock"
	if dbName != "" && safeDatabaseNameRe.MatchString(dbName) {
		// Application locks belong to the current database, and a tenant
		// connection has switched away from the engine's.
		proc = quoteMSSQLIdentifier(dbName) + ".sys.sp_getapplock"
	}
	var rc int
	err := tx.QueryRowContext(ctx,
		"DECLARE @rc INT; EXEC @rc = "+proc+" @Resource=@p1, @LockMode='Exclusive', @LockOwner='Transaction', @LockTimeout=@p2; SELECT @rc",
		name, writeLockWait.Milliseconds()).Scan(&rc)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	switch {
	case rc >= 0:
		return nil
	case rc == -1:
		return ErrWriteLockTimeout
	default:
		return fmt.Errorf("acquire: sp_getapplock returned %d", rc)
	}
}

// mysqlGetLock waits for the session lock. GET_LOCK answers 1 when granted, 0
// on the timeout and NULL on an error.
func mysqlGetLock(ctx context.Context, conn *sql.Conn, name string) error {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, int(writeLockWait.Seconds())).Scan(&got); err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return ErrWriteLockTimeout
	}
	return nil
}

// mysqlReleaseLock gives the session lock back. When that cannot be shown to
// have worked the connection is discarded, so the next borrower is never
// handed a session that still holds it.
func mysqlReleaseLock(ctx context.Context, conn *sql.Conn, name string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeLockRelease)
	defer cancel()
	var released sql.NullInt64
	err := conn.QueryRowContext(rctx, "SELECT RELEASE_LOCK(?)", name).Scan(&released)
	if err == nil && released.Valid && released.Int64 == 1 {
		return
	}
	slog.WarnContext(ctx, "write lock: release failed, discarding the connection", "err", err)
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}
