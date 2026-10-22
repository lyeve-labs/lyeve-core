package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// A lock wait timeout arrives only after the script waited out
// innodb_lock_wait_timeout, so trying again would stall the boot that long
// once more for every attempt.
func TestRetryMigration_LockWaitTimeoutIsTriedOnce(t *testing.T) {
	calls := 0
	err := retryMigration(context.Background(), "probe_migrations", "001_init", func() error {
		calls++
		return &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	})
	require.Error(t, err)
	require.Equal(t, 1, calls)
}

// A deadlock on every attempt is tried maxMigrationAttempts times, and the
// last failure returns without the wait meant for a next attempt.
func TestRetryMigration_LastDeadlockReturnsWithoutWaiting(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retryMigration(context.Background(), "probe_migrations", "001_init", func() error {
		calls++
		return &mysql.MySQLError{Number: 1213, Message: "Deadlock found"}
	})
	elapsed := time.Since(start)
	require.ErrorContains(t, err, "failed after 4 attempts")
	require.Equal(t, maxMigrationAttempts, calls)
	// The waits between attempts sum to 600 ms, and a wait after the fourth
	// would add 400 ms more.
	require.Less(t, elapsed, 900*time.Millisecond)
}
