package sqlx

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

// DefaultTxAttempts is how many times RetryOnTxConflict runs an operation
// before giving up. Contention here lasts as long as the transaction that won,
// so a handful of short attempts covers it.
const DefaultTxAttempts = 6

// backoffStep is the base unit of the wait between attempts. The actual wait
// is a random point inside the attempt's window rather than the window's
// width, because every contender for a row hits the deadlock at the same
// moment: a wait computed from the attempt number alone puts them all back on
// the row together, and they collide again on every attempt.
const backoffStep = 20 * time.Millisecond

func retryBackoff(attempt int) time.Duration {
	window := time.Duration(attempt) * backoffStep
	return window/2 + time.Duration(rand.Int64N(int64(window/2)+1))
}

// IsRetryableTxError reports whether err is a transient concurrency failure the
// database expects the client to retry, matched via each driver's native error
// type rather than message text:
//
//   - PostgreSQL (pgx): pgconn.PgError with Code 40001 or 40P01
//   - MySQL: mysql.MySQLError with Number 1213 or 1205
//   - MSSQL: mssql.Error with Number 1205
//
// All three engines treat these as the caller's to rerun, and say so: MySQL
// asks the client to restart the transaction, MSSQL names the loser the
// deadlock victim. An operation that returns one of these has not happened, so
// running it again is not a duplicate.
func IsRetryableTxError(err error) bool {
	// PostgreSQL: 40001 = serialization_failure, 40P01 = deadlock_detected
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01") {
		return true
	}

	// MySQL: 1213 = deadlock found, 1205 = lock wait timeout exceeded
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && (myErr.Number == 1213 || myErr.Number == 1205) {
		return true
	}

	// MSSQL: 1205 = chosen as the deadlock victim. Value receiver, so the
	// wrapped error is an mssql.Error and never a *mssql.Error.
	var msErr mssql.Error
	if errors.As(err, &msErr) && msErr.Number == 1205 {
		return true
	}

	return false
}

// RetryOnTxConflict runs fn, repeating it while it returns a retryable
// concurrency failure, with a short linear backoff. Anything else returns
// immediately, as does success.
//
// fn must be safe to run more than once. A deadlock victim's transaction was
// rolled back by the engine, so the work did not land, but an fn that mutates
// state outside that transaction would repeat that part.
//
// label names the operation in the error returned when every attempt failed.
func RetryOnTxConflict(ctx context.Context, label string, fn func() error) error {
	var lastErr error
	for attempt := 1; attempt <= DefaultTxAttempts; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !IsRetryableTxError(err) {
			return err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryBackoff(attempt)):
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", label, DefaultTxAttempts, lastErr)
}
