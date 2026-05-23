package db_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

var errProbeFull = errors.New("probe ceiling reached")

// createLockProbe makes the table a ceiling test counts and fills.
func createLockProbe(t *testing.T, pool db.DB) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `CREATE TABLE lock_probe (id VARCHAR(64) NOT NULL PRIMARY KEY)`)
	require.NoError(t, err)
}

func countLockProbe(ctx context.Context, pool db.DB) (int, error) {
	row, err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM lock_probe`)
	if err != nil {
		return 0, err
	}
	var n int
	return n, row.Scan(&n)
}

// insertUnderCeiling is a ceiling write: count, then insert when under limit.
// The pause between the two is the window two unserialized writers both pass
// through, widened so the race reproduces on every run.
func insertUnderCeiling(pool db.DB, limit int) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := countLockProbe(ctx, pool)
		if err != nil {
			return err
		}
		if n >= limit {
			return errProbeFull
		}
		time.Sleep(25 * time.Millisecond)
		_, err = pool.Exec(ctx, `INSERT INTO lock_probe (id) VALUES ($1)`, uuid.NewString())
		return err
	}
}

// Eight writers on a pool of four race for a ceiling of three. Each holds one
// connection while it waits, so the holder never needs a fifth, and exactly
// three rows land.
func TestSerializeWrite_CeilingHoldsUnderConcurrentWriters_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		createLockProbe(t, pool)
		const limit, writers = 3, 8
		var (
			start          = make(chan struct{})
			wg             sync.WaitGroup
			admitted, full atomic.Int32
			unexpected     = make(chan error, writers)
		)
		for range writers {
			wg.Go(func() {
				<-start
				err := db.SerializeWrite(context.Background(), pool, "test.lock_probe", insertUnderCeiling(pool, limit))
				switch {
				case err == nil:
					admitted.Add(1)
				case errors.Is(err, errProbeFull):
					full.Add(1)
				default:
					unexpected <- err
				}
			})
		}
		close(start)
		wg.Wait()
		close(unexpected)
		for err := range unexpected {
			t.Errorf("unexpected error: %v", err)
		}
		assert.EqualValues(t, limit, admitted.Load())
		assert.EqualValues(t, writers-limit, full.Load())
		n, err := countLockProbe(context.Background(), pool)
		require.NoError(t, err)
		assert.Equal(t, limit, n)
	})
}

// A write that fails rolls back what it wrote and gives the lock back, on
// MySQL too, where the lock belongs to the session and the session goes back
// to the pool.
func TestSerializeWrite_FailedWriteRollsBackAndReleases_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		createLockProbe(t, pool)
		ctx := context.Background()
		boom := errors.New("write failed")
		err := db.SerializeWrite(ctx, pool, "test.lock_release", func(ctx context.Context) error {
			if _, err := pool.Exec(ctx, `INSERT INTO lock_probe (id) VALUES ($1)`, "rolled-back"); err != nil {
				return err
			}
			return boom
		})
		require.ErrorIs(t, err, boom)
		n, err := countLockProbe(ctx, pool)
		require.NoError(t, err)
		assert.Zero(t, n, "the failed write's insert must roll back")

		if pool.Engine() == "mysql" {
			name := db.WriteLockName(dbNameOf(pool), "test.lock_release")
			row, err := pool.QueryRow(ctx, `SELECT IS_USED_LOCK($1)`, name)
			require.NoError(t, err)
			var holder *int64
			require.NoError(t, row.Scan(&holder))
			assert.Nil(t, holder, "no session may still hold the lock")
		}

		// Every connection in the pool can take the lock again at once. A
		// lock left on any session would hold one of these past the deadline.
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for range 4 {
			wg.Go(func() {
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				errs <- db.SerializeWrite(wctx, pool, "test.lock_release", func(context.Context) error { return nil })
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			assert.NoError(t, err)
		}
	})
}

// A serialized write runs in one transaction, so it cannot open another or
// take a second lock inside the first.
func TestSerializeWrite_RefusesNestedTransactions_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		err := db.SerializeWrite(ctx, pool, "test.nested", func(ctx context.Context) error {
			if _, err := pool.Begin(ctx); !errors.Is(err, db.ErrNestedWriteLock) {
				return errors.New("Begin inside a serialized write must be refused")
			}
			return db.SerializeWrite(ctx, pool, "test.nested.inner", func(context.Context) error { return nil })
		})
		require.ErrorIs(t, err, db.ErrNestedWriteLock)
	})
}

func dbNameOf(pool db.DB) string {
	if namer, ok := pool.(interface{ DatabaseName() string }); ok {
		return namer.DatabaseName()
	}
	return ""
}

// A lock not granted in time is an outage, whichever mapper a handler uses,
// so the caller retries rather than reading a server fault.
func TestErrWriteLockTimeout_AnswersServiceUnavailable(t *testing.T) {
	wrapped := fmt.Errorf("create flow: %w", db.ErrWriteLockTimeout)
	assert.ErrorIs(t, wrapped, core.ErrServiceUnavailable)
	assert.Equal(t, http.StatusServiceUnavailable, httpx.StatusFor(wrapped))
	assert.Equal(t, http.StatusServiceUnavailable, httpx.StoreStatusFor(wrapped))
}
