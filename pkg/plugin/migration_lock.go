package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Plugin migrations need a lock the core migration lock does not provide: the
// core runner holds its lock only for the engine's own schema, and each plugin
// then migrates itself during Start.
const (
	// migrationLockName keys the lock. One name for all plugin migrations
	// rather than one per plugin: the loop is short, cross-plugin ordering is
	// not something callers rely on, and a single name keeps the MySQL
	// GET_LOCK identifier well inside its 64-character limit.
	migrationLockName = "lyeve_plugin_migration"

	// migrationLockWait bounds the wait for another instance's plugin
	// migrations. Generous, because it covers every plugin's scripts on a cold
	// database, while still turning a genuinely stuck lock into a boot failure.
	migrationLockWait = 5 * time.Minute

	// migrationLockPoll is the gap between acquisition attempts. Each engine
	// offers only a single-statement try-acquire, so the wait is a poll.
	migrationLockPoll = 250 * time.Millisecond
)

// acquireMigrationLock blocks until the plugin-migration advisory lock is held,
// returning a release function. The lock is session-scoped, so it is pinned to a
// dedicated connection and released by closing it.
//
// An engine with no advisory-lock primitive gets a no-op rather than a hang.
func acquireMigrationLock(ctx context.Context, db *sql.DB, dialect string) (func(), error) {
	if db == nil || lockSQLFor(dialect) == "" {
		return func() {}, nil
	}

	deadline := time.Now().Add(migrationLockWait)
	for attempt := 0; ; attempt++ {
		// A connection is taken per attempt and dropped again unless the lock is
		// granted. Holding one across the wait would pin a connection per
		// waiting plugin, and plugins migrate in parallel: with many of them
		// waiting on one exclusive lock, a connection per waiter exhausts the
		// server's connection limit and leaves the losers unable to connect.
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("plugin migrate: lock connection: %w", err)
		}

		locked, err := tryLock(ctx, conn, dialect)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("plugin migrate: acquire lock: %w", err)
		}
		if locked {
			// The lock must be released explicitly. It is session-scoped and
			// sql.Conn.Close only returns the connection to the pool: the
			// session lives on still holding it, and the next caller handed
			// that same pooled connection waits out the full timeout against
			// itself.
			return func() {
				if q := unlockSQLFor(dialect); q != "" {
					if _, err := conn.ExecContext(context.WithoutCancel(ctx), q); err != nil {
						slog.Warn("plugin migrate: releasing the migration lock failed",
							"dialect", dialect, "err", err)
					}
				}
				_ = conn.Close()
			}, nil
		}
		_ = conn.Close()

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("plugin migrate: lock held by another instance for longer than %s", migrationLockWait)
		}
		if attempt == 0 {
			slog.Info("plugin migrate: waiting for another instance to finish migrating",
				"dialect", dialect, "timeout", migrationLockWait.String())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(migrationLockPoll):
		}
	}
}

// lockSQLFor returns the engine's single-statement try-acquire, or "" when the
// engine has no advisory lock.
func lockSQLFor(dialect string) string {
	switch dialect {
	case "postgres", "postgresql":
		return "SELECT pg_try_advisory_lock(hashtext('" + migrationLockName + "'))"
	case "mysql":
		// GET_LOCK names are server-wide, so the key includes a digest of the
		// current schema: two databases on one server must not block each other.
		return "SELECT GET_LOCK(CONCAT('" + migrationLockName + "_', MD5(DATABASE())), 0)"
	case "mssql", "sqlserver":
		return "DECLARE @r INT; EXEC @r = sp_getapplock @Resource='" + migrationLockName +
			"', @LockMode='Exclusive', @LockOwner='Session', @LockTimeout=0; SELECT @r"
	default:
		return ""
	}
}

// unlockSQLFor returns the engine's release statement, or "" when the engine
// has no advisory lock. It must mirror lockSQLFor's key exactly.
func unlockSQLFor(dialect string) string {
	switch dialect {
	case "postgres", "postgresql":
		return "SELECT pg_advisory_unlock(hashtext('" + migrationLockName + "'))"
	case "mysql":
		return "SELECT RELEASE_LOCK(CONCAT('" + migrationLockName + "_', MD5(DATABASE())))"
	case "mssql", "sqlserver":
		return "EXEC sp_releaseapplock @Resource='" + migrationLockName + "', @LockOwner='Session'"
	default:
		return ""
	}
}

// tryLock runs one non-blocking acquire, reporting whether it was granted.
func tryLock(ctx context.Context, conn *sql.Conn, dialect string) (bool, error) {
	q := lockSQLFor(dialect)
	if q == "" {
		return true, nil
	}
	switch dialect {
	case "mssql", "sqlserver":
		// sp_getapplock returns >= 0 on success, negative on failure or timeout.
		var code int
		if err := conn.QueryRowContext(ctx, q).Scan(&code); err != nil {
			return false, err
		}
		return code >= 0, nil
	case "mysql":
		// GET_LOCK yields 1, 0 on timeout, or NULL on error.
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, q).Scan(&got); err != nil {
			return false, err
		}
		return got.Valid && got.Int64 == 1, nil
	default:
		var locked bool
		if err := conn.QueryRowContext(ctx, q).Scan(&locked); err != nil {
			return false, err
		}
		return locked, nil
	}
}
