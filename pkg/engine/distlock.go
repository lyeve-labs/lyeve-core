package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DistLock provides cross-instance leader election via database advisory
// locks, on all three database dialects.
//
// PostgreSQL: pg_try_advisory_lock(hashtext('lyeve_{name}'))
// MySQL:      GET_LOCK('lyeve_{name}', 0)
// MSSQL:      sp_getapplock @Resource='lyeve_{name}'
//
// All three are session-scoped: the server releases the lock when the session
// ends, so a crashed replica cannot keep it. The session outlives the
// *sql.Conn wrapper, though. Closing a Conn returns the session to the pool
// with the lock still held, which is why Release unlocks explicitly and, when
// it cannot, discards the session instead of pooling it.
type DistLock struct {
	mu       sync.Mutex
	conn     *sql.Conn
	sessions *sql.DB
	name     string
	dialect  string
	held     bool
}

// sessionWait bounds how long TryAcquire waits for a connection to hold the
// lock on. A try that cannot get a session reports an error the caller logs
// and retries, rather than blocking the plugin Start or tick that called it.
const sessionWait = 5 * time.Second

// NewDistLock creates a DistLock that holds its session on the pool passed
// to TryAcquire. Call TryAcquire to start.
func NewDistLock(name string) *DistLock {
	return &DistLock{name: fmt.Sprintf("lyeve_%s", name)}
}

// NewDistLockOn creates a DistLock that holds its session on sessions and
// ignores the pool passed to TryAcquire. The engine host issues these, so a
// held lock never occupies a connection of the small pool plugins query
// through. A nil sessions behaves as NewDistLock.
func NewDistLockOn(name string, sessions *sql.DB) *DistLock {
	return &DistLock{name: fmt.Sprintf("lyeve_%s", name), sessions: sessions}
}

// TryAcquire attempts to acquire the advisory lock on db, or on the lock's
// own session pool when it has one. Returns true when the lock is acquired.
// When already held by another instance, TryAcquire returns false without
// blocking. When no connection frees up within sessionWait it returns an
// error.
//
// The lock is held on one session until Release. A session that ends
// (crash, lost connection) frees the lock on the server.
func (l *DistLock) TryAcquire(ctx context.Context, db *sql.DB, dialect string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.held {
		// A held flag proves nothing once the session is gone: the server
		// freed the lock and another replica may already hold it.
		if l.conn.PingContext(ctx) == nil {
			return true, nil
		}
		l.dropLocked()
	}

	if l.sessions != nil {
		db = l.sessions
	}
	connCtx, cancel := context.WithTimeout(ctx, sessionWait)
	conn, err := db.Conn(connCtx)
	cancel()
	if err != nil {
		return false, fmt.Errorf("distlock acquire conn: %w", err)
	}

	var acquired bool
	switch dialect {
	case "postgres":
		err = conn.QueryRowContext(ctx,
			"SELECT pg_try_advisory_lock(hashtext($1))", l.name,
		).Scan(&acquired)
	case "mysql":
		var result sql.NullInt64
		err = conn.QueryRowContext(ctx,
			"SELECT GET_LOCK(?, 0)", l.name,
		).Scan(&result)
		acquired = result.Valid && result.Int64 == 1
	case "mssql":
		// sp_getapplock reports through a return code, not a result set, so it
		// has to be captured and projected. 0 and 1 both mean granted. Only a
		// negative code is a failure, which is why this cannot scan into a bool.
		var code int
		err = conn.QueryRowContext(ctx,
			"DECLARE @rc INT; EXEC @rc = sp_getapplock @Resource=@p1, @LockMode='Exclusive', @LockOwner='Session', @LockTimeout=0; SELECT @rc",
			l.name,
		).Scan(&code)
		acquired = code >= 0
	default:
		conn.Close()
		return false, fmt.Errorf("distlock: unsupported dialect %q", dialect)
	}

	if err != nil {
		conn.Close()
		return false, fmt.Errorf("distlock acquire: %w", err)
	}
	if !acquired {
		conn.Close()
		return false, nil
	}

	l.conn = conn
	l.dialect = dialect
	l.held = true
	return true, nil
}

// Release unlocks and gives the connection back. After Release, TryAcquire
// can be called again.
func (l *DistLock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.held {
		return nil
	}
	conn := l.conn
	l.held = false
	l.conn = nil

	// The unlock gets its own deadline because Release runs from deferred and
	// shutdown paths whose context may already be canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := unlock(ctx, conn, l.dialect, l.name); err != nil {
		discard(conn)
		return fmt.Errorf("distlock release: %w", err)
	}
	return conn.Close()
}

// Check reports whether the lock is still held, by pinging the session that
// holds it. A dead session has already lost the lock on the server, so Check
// clears the local state and returns false.
func (l *DistLock) Check(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return false
	}
	if l.conn.PingContext(ctx) == nil {
		return true
	}
	l.dropLocked()
	return false
}

// dropLocked forgets a lock whose session is gone. The caller holds l.mu.
func (l *DistLock) dropLocked() {
	discard(l.conn)
	l.conn = nil
	l.held = false
}

func unlock(ctx context.Context, conn *sql.Conn, dialect, name string) error {
	switch dialect {
	case "postgres":
		var released bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock(hashtext($1))", name).Scan(&released); err != nil {
			return err
		}
		if !released {
			return errors.New("lock was not held by this session")
		}
		return nil
	case "mysql":
		var result sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&result); err != nil {
			return err
		}
		if !result.Valid || result.Int64 != 1 {
			return errors.New("lock was not held by this session")
		}
		return nil
	case "mssql":
		var code int
		if err := conn.QueryRowContext(ctx,
			"DECLARE @rc INT; EXEC @rc = sp_releaseapplock @Resource=@p1, @LockOwner='Session'; SELECT @rc",
			name,
		).Scan(&code); err != nil {
			return err
		}
		if code < 0 {
			return fmt.Errorf("sp_releaseapplock returned %d", code)
		}
		return nil
	default:
		return fmt.Errorf("unsupported dialect %q", dialect)
	}
}

// discard closes conn so that its session is not returned to the pool. When
// the unlock could not be confirmed, a pooled session could still hold the
// lock and no replica would take it again until the pool recycled it.
func discard(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// IsHeld reports whether the lock is currently held.
func (l *DistLock) IsHeld() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// RunWhenLeader acquires the lock and executes fn while holding it.
// Releases the lock when fn returns. A typical usage pattern for
// singleton background jobs.
//
// Returns an error when the lock cannot be acquired (another instance
// is the leader) or fn fails.
func (l *DistLock) RunWhenLeader(ctx context.Context, db *sql.DB, dialect string, fn func(context.Context) error) error {
	acquired, err := l.TryAcquire(ctx, db, dialect)
	if err != nil {
		return fmt.Errorf("run when leader: %w", err)
	}
	if !acquired {
		return fmt.Errorf("run when leader: lock %q held by another instance", l.name)
	}
	defer l.Release()
	return fn(ctx)
}

// DistLockConfig names a leader election: the lock, the dialect it runs on,
// and how long a caller waits before trying again. Nothing in this package
// reads it or applies a default retry period.
type DistLockConfig struct {
	Name        string
	Dialect     string
	RetryPeriod time.Duration
}
