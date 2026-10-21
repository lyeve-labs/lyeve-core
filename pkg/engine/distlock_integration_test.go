package engine_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

func TestDistLockTryAcquire_Postgres(t *testing.T) {
	testDistLockTryAcquire(t, plugintest.Postgres(t))
}
func TestDistLockTryAcquire_MySQL(t *testing.T) { testDistLockTryAcquire(t, plugintest.MySQL(t)) }
func TestDistLockTryAcquire_MSSQL(t *testing.T) { testDistLockTryAcquire(t, plugintest.MSSQL(t)) }

// testDistLockTryAcquire runs the leader-election contract against a real
// database. Every dialect reports lock acquisition differently -  a boolean on
// Postgres, 1/0 on MySQL, a signed return code on MSSQL -  and only a real
// server exercises that translation.
func testDistLockTryAcquire(t *testing.T, host core.Host) {
	t.Helper()
	ctx := context.Background()
	db := host.RawDB()
	dialect := host.Dialect()

	leader := engine.NewDistLock("leader_election")
	got, err := leader.TryAcquire(ctx, db, dialect)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !got {
		t.Fatal("first TryAcquire did not take the lock")
	}
	if !leader.IsHeld() {
		t.Error("IsHeld is false after a successful acquire")
	}

	// A second holder of the same name must be turned away rather than block.
	follower := engine.NewDistLock("leader_election")
	got, err = follower.TryAcquire(ctx, db, dialect)
	if err != nil {
		t.Fatalf("contended TryAcquire: %v", err)
	}
	if got {
		t.Error("second TryAcquire took a lock already held")
		_ = follower.Release()
	}

	// A different name is independent.
	other := engine.NewDistLock("other_job")
	got, err = other.TryAcquire(ctx, db, dialect)
	if err != nil {
		t.Fatalf("TryAcquire on a second name: %v", err)
	}
	if !got {
		t.Error("an unrelated lock name was refused")
	}
	if err := other.Release(); err != nil {
		t.Errorf("Release other: %v", err)
	}

	if err := leader.Release(); err != nil {
		t.Fatalf("Release leader: %v", err)
	}
	if leader.IsHeld() {
		t.Error("IsHeld is true after Release")
	}

	// The name is free again once released.
	got, err = follower.TryAcquire(ctx, db, dialect)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if !got {
		t.Fatal("lock was not reacquirable after release")
	}
	if err := follower.Release(); err != nil {
		t.Errorf("Release follower: %v", err)
	}
}

func TestDistLockRelease_FreesTheLockOnTheServer_Postgres(t *testing.T) {
	testDistLockReleaseFreesTheLock(t, plugintest.Postgres(t))
}
func TestDistLockRelease_FreesTheLockOnTheServer_MySQL(t *testing.T) {
	testDistLockReleaseFreesTheLock(t, plugintest.MySQL(t))
}
func TestDistLockRelease_FreesTheLockOnTheServer_MSSQL(t *testing.T) {
	testDistLockReleaseFreesTheLock(t, plugintest.MSSQL(t))
}

// testDistLockReleaseFreesTheLock asks a second session, pinned for the whole
// test, whether it could take the lock after Release. The reacquire case above
// cannot see a leaked lock: the pool tends to hand the released session back,
// and a session that still holds the lock is granted it again.
func testDistLockReleaseFreesTheLock(t *testing.T, host core.Host) {
	t.Helper()
	ctx := context.Background()
	db := host.RawDB()
	dialect := host.Dialect()

	probe, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("probe conn: %v", err)
	}
	defer probe.Close()

	lock := engine.NewDistLock("release_frees")
	got, err := lock.TryAcquire(ctx, db, dialect)
	if err != nil || !got {
		t.Fatalf("TryAcquire = %v, %v", got, err)
	}
	if probeTakes(ctx, t, probe, dialect, "lyeve_release_frees") {
		t.Fatal("a second session took a lock that is held")
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !probeTakes(ctx, t, probe, dialect, "lyeve_release_frees") {
		t.Fatal("after Release another session still cannot take the lock")
	}
}

// probeTakes tries the lock on conn and, when it gets it, gives it back.
func probeTakes(ctx context.Context, t *testing.T, conn *sql.Conn, dialect, name string) bool {
	t.Helper()
	switch dialect {
	case "postgres":
		var ok bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", name).Scan(&ok); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if ok {
			_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtext($1))", name)
		}
		return ok
	case "mysql":
		var r sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&r); err != nil {
			t.Fatalf("probe: %v", err)
		}
		ok := r.Valid && r.Int64 == 1
		if ok {
			_, _ = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", name)
		}
		return ok
	case "mssql":
		var code int
		if err := conn.QueryRowContext(ctx,
			"DECLARE @rc INT; EXEC @rc = sp_getapplock @Resource=@p1, @LockMode='Exclusive', @LockOwner='Session', @LockTimeout=0; SELECT @rc",
			name).Scan(&code); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if code >= 0 {
			_, _ = conn.ExecContext(ctx, "EXEC sp_releaseapplock @Resource=@p1, @LockOwner='Session'", name)
		}
		return code >= 0
	}
	t.Fatalf("unsupported dialect %q", dialect)
	return false
}

func TestDistLockCheck_KeepsTheLockWhenTheCallerDeadlinePassed_Postgres(t *testing.T) {
	testDistLockCheckKeepsTheLockOnDeadline(t, plugintest.Postgres(t))
}
func TestDistLockCheck_KeepsTheLockWhenTheCallerDeadlinePassed_MySQL(t *testing.T) {
	testDistLockCheckKeepsTheLockOnDeadline(t, plugintest.MySQL(t))
}
func TestDistLockCheck_KeepsTheLockWhenTheCallerDeadlinePassed_MSSQL(t *testing.T) {
	testDistLockCheckKeepsTheLockOnDeadline(t, plugintest.MSSQL(t))
}

// testDistLockCheckKeepsTheLockOnDeadline checks with a context that has
// already run out. The ping fails for the caller's reason, not the session's,
// so the session has to stay open: closing it would free the lock on the
// server while the work it covers is still running.
func testDistLockCheckKeepsTheLockOnDeadline(t *testing.T, host core.Host) {
	t.Helper()
	ctx := context.Background()
	db := host.RawDB()
	dialect := host.Dialect()

	probe, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("probe conn: %v", err)
	}
	defer probe.Close()

	lock := engine.NewDistLock("check_deadline")
	got, err := lock.TryAcquire(ctx, db, dialect)
	if err != nil || !got {
		t.Fatalf("TryAcquire = %v, %v", got, err)
	}
	defer lock.Release()

	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	if lock.Check(expired) {
		t.Error("Check confirmed the lock without reaching the database")
	}
	if !lock.IsHeld() {
		t.Error("Check dropped the lock because the caller's deadline passed")
	}
	if probeTakes(ctx, t, probe, dialect, "lyeve_check_deadline") {
		t.Fatal("another session took the lock after a check whose deadline had passed")
	}
	if !lock.Check(ctx) {
		t.Error("a check with a live context does not find the lock held")
	}
}

func TestDistLockCheck_ReportsTheLockLostWithItsSession_Postgres(t *testing.T) {
	testDistLockCheckReportsALostSession(t, plugintest.Postgres(t))
}
func TestDistLockCheck_ReportsTheLockLostWithItsSession_MySQL(t *testing.T) {
	testDistLockCheckReportsALostSession(t, plugintest.MySQL(t))
}
func TestDistLockCheck_ReportsTheLockLostWithItsSession_MSSQL(t *testing.T) {
	testDistLockCheckReportsALostSession(t, plugintest.MSSQL(t))
}

// testDistLockCheckReportsALostSession ends the holding session from another
// one, the way a dropped connection or a server restart would, and expects
// Check to report the lock lost.
func testDistLockCheckReportsALostSession(t *testing.T, host core.Host) {
	t.Helper()
	ctx := context.Background()
	db := host.RawDB()
	dialect := host.Dialect()
	const name = "lyeve_check_lost"

	probe, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("probe conn: %v", err)
	}
	defer probe.Close()

	lock := engine.NewDistLock("check_lost")
	got, err := lock.TryAcquire(ctx, db, dialect)
	if err != nil || !got {
		t.Fatalf("TryAcquire = %v, %v", got, err)
	}
	defer lock.Release()

	holder, ok := lockHolder(ctx, t, probe, dialect, name)
	if !ok {
		t.Fatal("no session holds the lock after TryAcquire")
	}
	killSession(ctx, t, probe, dialect, holder)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := lockHolder(ctx, t, probe, dialect, name); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server still holds the lock after its session was killed")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if lock.Check(ctx) {
		t.Error("Check reports the lock held after its session ended")
	}
	if lock.IsHeld() {
		t.Error("IsHeld is true after Check found the session gone")
	}
}

// lockHolder returns the server id of the session holding the named lock.
func lockHolder(ctx context.Context, t *testing.T, conn *sql.Conn, dialect, name string) (int64, bool) {
	t.Helper()
	var id sql.NullInt64
	var err error
	switch dialect {
	case "postgres":
		// A bigint advisory key keeps its low 32 bits in objid.
		err = conn.QueryRowContext(ctx, `SELECT pid FROM pg_locks
			WHERE locktype = 'advisory' AND granted
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND objid::bigint = (hashtext($1)::bigint & 4294967295)`, name).Scan(&id)
	case "mysql":
		err = conn.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?)", name).Scan(&id)
	case "mssql":
		err = conn.QueryRowContext(ctx, `SELECT TOP 1 request_session_id FROM sys.dm_tran_locks
			WHERE resource_type = 'APPLICATION' AND request_owner_type = 'SESSION'
			  AND request_status = 'GRANT' AND resource_database_id = DB_ID()
			  AND resource_description LIKE '%' + @p1 + '%'`, name).Scan(&id)
	default:
		t.Fatalf("unsupported dialect %q", dialect)
	}
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("lock holder: %v", err)
	}
	return id.Int64, id.Valid
}

// killSession ends another session on the server.
func killSession(ctx context.Context, t *testing.T, conn *sql.Conn, dialect string, id int64) {
	t.Helper()
	var err error
	switch dialect {
	case "postgres":
		_, err = conn.ExecContext(ctx, "SELECT pg_terminate_backend($1, 5000)", id)
	case "mysql", "mssql":
		_, err = conn.ExecContext(ctx, fmt.Sprintf("KILL %d", id))
	default:
		t.Fatalf("unsupported dialect %q", dialect)
	}
	if err != nil {
		t.Fatalf("kill session %d: %v", id, err)
	}
}
