package plugin

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 names a lock
	"database/sql"
	"encoding/hex"
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
	if db == nil || lockSQLFor(dialect, migrationLockName) == "" {
		return func() {}, nil
	}

	deadline := time.Now().Add(migrationLockWait)
	for attempt := 0; ; attempt++ {
		release, held, err := tryMigrationLock(ctx, db, dialect)
		if err != nil {
			return nil, err
		}
		if held {
			return release, nil
		}

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

// tryMigrationLock makes one attempt at the plugin-migration advisory lock. It
// reports whether the lock was granted and, when it was, returns its release.
//
// A connection is taken per attempt and dropped again unless the lock is
// granted. Holding one across a wait would pin a connection per waiting
// plugin, and plugins migrate in parallel: with many of them waiting on one
// exclusive lock, a connection per waiter exhausts the server's connection
// limit and leaves the losers unable to connect.
func tryMigrationLock(ctx context.Context, db *sql.DB, dialect string) (func(), bool, error) {
	if db == nil || lockSQLFor(dialect, migrationLockName) == "" {
		return func() {}, true, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("plugin migrate: lock connection: %w", err)
	}

	key, err := lockKey(ctx, conn, dialect)
	if err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("plugin migrate: lock key: %w", err)
	}
	locked, err := tryLock(ctx, conn, dialect, key)
	if err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("plugin migrate: acquire lock: %w", err)
	}
	if !locked {
		_ = conn.Close()
		return nil, false, nil
	}
	// The lock must be released explicitly. It is session-scoped and
	// sql.Conn.Close only returns the connection to the pool: the session
	// lives on still holding it, and the next caller handed that same pooled
	// connection waits out the full timeout against itself.
	return func() {
		if q := unlockSQLFor(dialect, key); q != "" {
			if _, err := conn.ExecContext(context.WithoutCancel(ctx), q); err != nil {
				slog.Warn("plugin migrate: releasing the migration lock failed",
					"dialect", dialect, "err", err)
			}
		}
		_ = conn.Close()
	}, true, nil
}

// lockKey returns the resource the lock names on this connection. On MySQL,
// GET_LOCK names are server-wide, so the key carries a digest of the current
// schema: two databases on one server must not block each other. The digest is
// the lowercase hex MD5 the server's MD5(DATABASE()) gave, taken here because
// MySQL 9 has no MD5 function, so an engine that still asks the server for it
// takes the same lock during an upgrade. Hex after a fixed prefix is safe to
// inline in SQL.
func lockKey(ctx context.Context, conn *sql.Conn, dialect string) (string, error) {
	if dialect != "mysql" {
		return migrationLockName, nil
	}
	var schema sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil {
		return "", fmt.Errorf("read current schema: %w", err)
	}
	sum := md5.Sum([]byte(schema.String)) //nolint:gosec // a lock key, not a security control
	return migrationLockName + "_" + hex.EncodeToString(sum[:]), nil
}

// lockSQLFor returns the engine's single-statement try-acquire of key, or ""
// when the engine has no advisory lock.
func lockSQLFor(dialect, key string) string {
	switch dialect {
	case "postgres", "postgresql":
		return "SELECT pg_try_advisory_lock(hashtext('" + key + "'))"
	case "mysql":
		return "SELECT GET_LOCK('" + key + "', 0)"
	case "mssql", "sqlserver":
		return "DECLARE @r INT; EXEC @r = sp_getapplock @Resource='" + key +
			"', @LockMode='Exclusive', @LockOwner='Session', @LockTimeout=0; SELECT @r"
	default:
		return ""
	}
}

// unlockSQLFor returns the engine's release of key, or "" when the engine has
// no advisory lock. It must mirror lockSQLFor exactly.
func unlockSQLFor(dialect, key string) string {
	switch dialect {
	case "postgres", "postgresql":
		return "SELECT pg_advisory_unlock(hashtext('" + key + "'))"
	case "mysql":
		return "SELECT RELEASE_LOCK('" + key + "')"
	case "mssql", "sqlserver":
		return "EXEC sp_releaseapplock @Resource='" + key + "', @LockOwner='Session'"
	default:
		return ""
	}
}

// tryLock runs one non-blocking acquire, reporting whether it was granted.
func tryLock(ctx context.Context, conn *sql.Conn, dialect, key string) (bool, error) {
	q := lockSQLFor(dialect, key)
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
