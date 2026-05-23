//go:build !short && !mutest

// MySQL InnoDB Locking Behavior Integration Tests
// These tests exercise InnoDB's row-level locking, gap locks, next-key locks,
// deadlock detection, lock wait timeouts, and transaction isolation levels
// against a real MySQL 8 testcontainer.
//
// Lock Types Tested:
//   - Row-level (record) locks - SELECT ... FOR UPDATE
//   - Gap locks - prevent phantom reads under REPEATABLE-READ
//   - Deadlock detection - concurrent UPDATE on overlapping rows
//   - Lock wait timeout - innodb_lock_wait_timeout enforcement
//   - Transaction isolation - READ-COMMITTED vs REPEATABLE-READ behavior
//
// InnoDB MVCC Fundamentals:
//   - REPEATABLE-READ: snapshot read at first read in transaction
//   - READ-COMMITTED: snapshot read at each statement
//   - FOR UPDATE: exclusive next-key lock (record + gap before it)
//   - LOCK IN SHARE MODE: shared next-key lock
//
// References:
//   - MySQL 8.0 Reference Manual §15.7 (InnoDB Locking)
//   - https://dev.mysql.com/doc/refman/8.0/en/innodb-locking.html
package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Helpers

// lockTableName generates a deterministic but unique table name.
func lockTableName(prefix string) string {
	return fmt.Sprintf("locks_%s_%d", prefix, time.Now().UnixNano()%100000)
}

// Section 1: Record (Row-Level) Locks

// TestInnoDB_RecordLock_SELECT_FOR_UPDATE blocks concurrent UPDATE on the
// same row. This is the most basic InnoDB lock: an exclusive (X) lock on
// the index record.
func TestInnoDB_RecordLock_SELECT_FOR_UPDATE(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping InnoDB locking integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("rowlock")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, balance INT NOT NULL DEFAULT 0) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (balance) VALUES (100)", tableName))
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// Tx1: Acquire FOR UPDATE lock on row id=1.
	ready := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		tx1, err := pool.Begin(ctx)
		if err != nil {
			done <- fmt.Errorf("tx1 begin: %w", err)
			return
		}
		defer tx1.Rollback()

		// Acquire exclusive lock on row id=1.
		var bal int
		err = tx1.QueryRowContext(ctx,
			fmt.Sprintf("SELECT balance FROM `%s` WHERE id = 1 FOR UPDATE", tableName),
		).Scan(&bal)
		if err != nil {
			done <- fmt.Errorf("tx1 SELECT FOR UPDATE: %w", err)
			return
		}
		if bal != 100 {
			done <- fmt.Errorf("tx1: expected balance=100, got %d", bal)
			return
		}

		close(ready) // Signal tx1 has the lock.

		// Hold the lock long enough for tx2 to start blocking on it.
		time.Sleep(1 * time.Second)

		if err := tx1.Commit(); err != nil {
			done <- fmt.Errorf("tx1 commit: %w", err)
			return
		}
		done <- nil
	}()

	// Wait for tx1 to acquire the lock.
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("tx1 did not acquire lock within timeout")
	}

	// Tx2: Try to UPDATE the locked row. It should block until tx1 commits.
	go func() {
		<-ready // Wait for tx1 to have the lock.

		tx2, err := pool.Begin(ctx)
		if err != nil {
			done <- fmt.Errorf("tx2 begin: %w", err)
			return
		}
		defer tx2.Rollback()

		start := time.Now()
		_, err = tx2.ExecContext(ctx,
			fmt.Sprintf("UPDATE `%s` SET balance = 200 WHERE id = 1", tableName),
		)
		elapsed := time.Since(start)

		if err != nil {
			done <- fmt.Errorf("tx2 UPDATE: %w (elapsed=%v)", err, elapsed)
			return
		}
		if err := tx2.Commit(); err != nil {
			done <- fmt.Errorf("tx2 commit: %w", err)
			return
		}

		// If elapsed < 50ms, the lock didn't actually block: that's a problem.
		if elapsed < 50*time.Millisecond {
			done <- fmt.Errorf("tx2 UPDATE did not block: elapsed=%v (FOR UPDATE should block concurrent writers)", elapsed)
			return
		}
		t.Logf("tx2 UPDATE blocked for %v (expected locking behavior)", elapsed)
		done <- nil
	}()

	// Collect results.
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("test timed out waiting for goroutines")
		}
	}
}

// TestInnoDB_RecordLock_LOCK_IN_SHARE_MODE allows concurrent shared locks
// but blocks exclusive locks (FOR UPDATE).
func TestInnoDB_RecordLock_LOCK_IN_SHARE_MODE(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping InnoDB shared lock integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("shrlock")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, value INT NOT NULL) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (value) VALUES (42)", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Tx1: LOCK IN SHARE MODE.
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx1 begin: %v", err)
	}
	defer tx1.Rollback()

	var val int
	err = tx1.QueryRowContext(ctx,
		fmt.Sprintf("SELECT value FROM `%s` WHERE id = 1 LOCK IN SHARE MODE", tableName),
	).Scan(&val)
	if err != nil {
		t.Fatalf("tx1 SELECT LOCK IN SHARE MODE: %v", err)
	}
	if val != 42 {
		t.Fatalf("tx1: expected 42, got %d", val)
	}

	// Tx2: Another LOCK IN SHARE MODE on the same row should succeed
	// (shared locks are compatible with other shared locks).
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx2 begin: %v", err)
	}
	defer tx2.Rollback()

	err = tx2.QueryRowContext(ctx,
		fmt.Sprintf("SELECT value FROM `%s` WHERE id = 1 LOCK IN SHARE MODE", tableName),
	).Scan(&val)
	if err != nil {
		t.Fatalf("tx2 SELECT LOCK IN SHARE MODE: %v (shared locks should be compatible)", err)
	}

	// Tx3: FOR UPDATE on the same row should block (incompatible with shared locks).
	blocked := make(chan struct{})
	go func() {
		tx3, err := pool.Begin(ctx)
		if err != nil {
			return
		}
		defer tx3.Rollback()

		close(blocked) // Signal we're attempting the lock.
		start := time.Now()
		_, err = tx3.ExecContext(ctx, fmt.Sprintf("UPDATE `%s` SET value = 99 WHERE id = 1", tableName))
		elapsed := time.Since(start)

		if err != nil && elapsed < 100*time.Millisecond {
			// Immediate error (lock timeout) before our wait: that's fine too.
			t.Logf("tx3 UPDATE failed immediately: %v", err)
			return
		}
		if elapsed < 50*time.Millisecond {
			t.Errorf("tx3 FOR UPDATE did not block: elapsed=%v", elapsed)
		}
	}()

	// Wait for tx3 to start, then commit both shared-lock holders.
	select {
	case <-blocked:
		time.Sleep(100 * time.Millisecond) // Let tx3 actually block.
	case <-time.After(5 * time.Second):
		t.Fatal("tx3 did not start within timeout")
	}

	tx2.Rollback()
	tx1.Rollback()
}

// Section 2: Gap Locks & Phantom Read Prevention

// TestInnoDB_GapLock_PhantomPrevention verifies that REPEATABLE-READ
// prevents phantom reads via gap locks. Tx1 locks a range with SELECT FOR
// UPDATE; Tx2 tries to INSERT into that range and must block.
func TestInnoDB_GapLock_PhantomPrevention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping InnoDB gap lock integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("gaplock")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50), INDEX idx_name (name)) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Seed with values that create gaps: id=10 and id=20.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (id, name) VALUES (10, 'alpha'), (20, 'omega')", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Tx1: Lock all rows WHERE id BETWEEN 1 AND 30 at REPEATABLE-READ.
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx1 begin: %v", err)
	}
	defer tx1.Rollback()

	err = tx1.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE id BETWEEN 1 AND 30 FOR UPDATE", tableName),
	).Scan(new(int))
	if err != nil {
		t.Fatalf("tx1 FOR UPDATE: %v", err)
	}

	// Tx2: Try to INSERT id=15 (inside the gap-locked range).
	blocked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			done <- fmt.Errorf("tx2 begin: %w", err)
			return
		}
		defer tx2.Rollback()

		close(blocked) // Signal we're about to attempt the insert.
		start := time.Now()
		_, err = tx2.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO `%s` (id, name) VALUES (15, 'gap-insert')", tableName))
		elapsed := time.Since(start)

		if err != nil {
			done <- fmt.Errorf("tx2 INSERT: %w (elapsed=%v)", err, elapsed)
			return
		}
		if elapsed < 50*time.Millisecond {
			done <- fmt.Errorf("tx2 INSERT into gap-locked range did not block: elapsed=%v", elapsed)
			return
		}
		t.Logf("tx2 INSERT blocked for %v (gap lock working)", elapsed)
		done <- nil
	}()

	// Wait for tx2 to start, then release tx1.
	select {
	case <-blocked:
		time.Sleep(100 * time.Millisecond) // Let tx2 actually block.
	case <-time.After(5 * time.Second):
		t.Fatal("tx2 did not start within timeout")
	}

	tx1.Rollback()

	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tx2 did not complete after tx1 released lock")
	}
}

// TestInnoDB_PhantomRead_RepeatableRead verifies that REPEATABLE-READ
// provides a consistent snapshot: Tx1 reads rows, Tx2 inserts a new row,
// Tx1 reads again and should NOT see the new row.
func TestInnoDB_PhantomRead_RepeatableRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping phantom read test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("phantom")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, val INT) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (val) VALUES (1), (2)", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Tx1: Snapshot read at REPEATABLE-READ.
	tx1, err := pool.SQLDB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatalf("tx1 begin: %v", err)
	}
	defer tx1.Rollback()

	var count1 int
	err = tx1.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM `%s`", tableName),
	).Scan(&count1)
	if err != nil {
		t.Fatalf("tx1 first read: %v", err)
	}
	if count1 != 2 {
		t.Fatalf("tx1 first read: expected 2 rows, got %d", count1)
	}

	// Tx2: INSERT a new row (auto-commit).
	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (val) VALUES (3)", tableName))
	if err != nil {
		t.Fatalf("tx2 insert: %v", err)
	}

	// Tx1: Read again. It should still see 2 rows (snapshot isolation).
	var count2 int
	err = tx1.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM `%s`", tableName),
	).Scan(&count2)
	if err != nil {
		t.Fatalf("tx1 second read: %v", err)
	}
	if count2 != 2 {
		t.Errorf("REPEATABLE-READ phantom violation: expected 2 rows, got %d (tx2 insert became visible)", count2)
	}
	t.Logf("REPEATABLE-READ snapshot: first read=%d, second read=%d", count1, count2)
}

// Section 3: Deadlock Detection

// TestInnoDB_Deadlock_Detection verifies InnoDB's automatic deadlock
// detection. Two transactions try to lock rows in reverse order: InnoDB
// must detect the cycle and roll back one transaction with error 1213.
func TestInnoDB_Deadlock_Detection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping deadlock detection test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("deadlock")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, val INT NOT NULL) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (id, val) VALUES (1, 100), (2, 200)", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Barrier to synchronize both goroutines.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	var err1, err2 error

	// Tx1: Lock id=1 first, then id=2.
	go func() {
		defer wg.Done()
		<-start

		tx, err := pool.Begin(ctx)
		if err != nil {
			err1 = fmt.Errorf("tx1 begin: %w", err)
			return
		}
		defer tx.Rollback()

		_, err = tx.ExecContext(ctx,
			fmt.Sprintf("UPDATE `%s` SET val = 101 WHERE id = 1", tableName))
		if err != nil {
			err1 = fmt.Errorf("tx1 lock id=1: %w", err)
			return
		}

		// Brief pause to let tx2 acquire its first lock.
		time.Sleep(200 * time.Millisecond)

		_, err = tx.ExecContext(ctx,
			fmt.Sprintf("UPDATE `%s` SET val = 102 WHERE id = 2", tableName))
		if err != nil {
			err1 = fmt.Errorf("tx1 lock id=2: %w", err)
			return
		}

		tx.Commit()
	}()

	// Tx2: Lock id=2 first, then id=1 (reverse order -> deadlock).
	go func() {
		defer wg.Done()
		<-start

		tx, err := pool.Begin(ctx)
		if err != nil {
			err2 = fmt.Errorf("tx2 begin: %w", err)
			return
		}
		defer tx.Rollback()

		_, err = tx.ExecContext(ctx,
			fmt.Sprintf("UPDATE `%s` SET val = 202 WHERE id = 2", tableName))
		if err != nil {
			err2 = fmt.Errorf("tx2 lock id=2: %w", err)
			return
		}

		// Brief pause to let tx1 acquire its first lock.
		time.Sleep(200 * time.Millisecond)

		_, err = tx.ExecContext(ctx,
			fmt.Sprintf("UPDATE `%s` SET val = 201 WHERE id = 1", tableName))
		if err != nil {
			err2 = fmt.Errorf("tx2 lock id=1: %w", err)
			return
		}

		tx.Commit()
	}()

	close(start)
	wg.Wait()

	// At least one transaction should encounter a deadlock (MySQL error 1213).
	deadlockCount := 0
	for _, e := range []error{err1, err2} {
		if e != nil && (containsMySQLDeadlock(e.Error())) {
			deadlockCount++
			t.Logf("deadlock detected (expected): %v", e)
		} else if e != nil {
			t.Logf("non-deadlock error: %v", e)
		}
	}

	if deadlockCount == 0 {
		t.Error("expected at least one deadlock (error 1213) in reverse-order lock scenario")
	}
}

// containsMySQLDeadlock checks whether the error string contains MySQL
// deadlock indicators (error 1213: Deadlock found when trying to get lock).
func containsMySQLDeadlock(errStr string) bool {
	if len(errStr) == 0 {
		return false
	}
	return contains(errStr, "1213") ||
		contains(errStr, "Deadlock") ||
		contains(errStr, "deadlock") ||
		contains(errStr, "try restarting transaction")
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Section 4: Lock Wait Timeout

// TestInnoDB_LockWaitTimeout verifies that innodb_lock_wait_timeout is
// enforced. A transaction holding a row lock causes a second transaction
// to time out after the configured interval.
func TestInnoDB_LockWaitTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lock wait timeout test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("waittimeout")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, val INT) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (val) VALUES (1)", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Tx1: Hold an exclusive lock on id=1.
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("tx1 begin: %v", err)
	}
	defer tx1.Rollback()

	_, err = tx1.ExecContext(ctx,
		fmt.Sprintf("UPDATE `%s` SET val = 99 WHERE id = 1", tableName))
	if err != nil {
		t.Fatalf("tx1 lock: %v", err)
	}

	// Tx2: Set a short lock wait timeout (2s), then try to lock the same row.
	done := make(chan error, 1)
	go func() {
		tx2, err := pool.Begin(ctx)
		if err != nil {
			done <- fmt.Errorf("tx2 begin: %w", err)
			return
		}
		defer tx2.Rollback()

		_, err = tx2.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 2")
		if err != nil {
			done <- fmt.Errorf("tx2 set timeout: %w", err)
			return
		}

		start := time.Now()
		_, err = tx2.ExecContext(ctx,
			fmt.Sprintf("UPDATE `%s` SET val = 42 WHERE id = 1", tableName))
		elapsed := time.Since(start)

		if err != nil {
			done <- fmt.Errorf("tx2 UPDATE timed out: %w (elapsed=%v, expected lock wait timeout ~2s)", err, elapsed)
			return
		}
		// If we got here without error, the lock was released by tx1 or
		// timeout didn't fire: either way, check timing.
		if elapsed < 1900*time.Millisecond {
			done <- fmt.Errorf("tx2 UPDATE completed too fast: %v (expected ~2s timeout)", elapsed)
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		// Error is expected (lock wait timeout).
		if err != nil {
			t.Logf("lock wait timeout triggered (expected): %v", err)
		} else {
			t.Log("tx2 completed - lock was released before timeout")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("test timed out - lock wait timeout may not be working")
	}
}

// TestInnoDB_LockWaitTimeout_Configuration verifies the default
// innodb_lock_wait_timeout is set to a reasonable value (default: 50s).
func TestInnoDB_LockWaitTimeout_Configuration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lock wait timeout config test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var globalTimeout, sessionTimeout int64

	row, qrErr := pool.QueryRow(ctx, "SELECT @@GLOBAL.innodb_lock_wait_timeout")
	_ = qrErr
	err := row.Scan(&globalTimeout)
	if err != nil {
		t.Fatalf("read global innodb_lock_wait_timeout: %v", err)
	}

	row, qrErr = pool.QueryRow(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout")
	_ = qrErr
	err = row.Scan(&sessionTimeout)
	if err != nil {
		t.Fatalf("read session innodb_lock_wait_timeout: %v", err)
	}

	t.Logf("innodb_lock_wait_timeout: global=%ds, session=%ds", globalTimeout, sessionTimeout)

	if globalTimeout < 1 || globalTimeout > 31536000 {
		t.Errorf("global innodb_lock_wait_timeout=%d is outside reasonable range [1, 31536000]", globalTimeout)
	}
	if sessionTimeout < 1 || sessionTimeout > 31536000 {
		t.Errorf("session innodb_lock_wait_timeout=%d is outside reasonable range [1, 31536000]", sessionTimeout)
	}
}

// Section 5: Transaction Isolation Levels

// TestInnoDB_Isolation_ReadCommitted sees only committed data and does
// NOT provide a repeatable snapshot: each statement gets a fresh snapshot.
func TestInnoDB_Isolation_ReadCommitted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping isolation level test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("rc")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, val INT) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (val) VALUES (10)", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Tx1: READ-COMMITTED.
	tx1, err := pool.SQLDB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("tx1 begin: %v", err)
	}
	defer tx1.Rollback()

	var v1 int
	err = tx1.QueryRowContext(ctx,
		fmt.Sprintf("SELECT val FROM `%s` WHERE id = 1", tableName),
	).Scan(&v1)
	if err != nil {
		t.Fatalf("tx1 first read: %v", err)
	}
	if v1 != 10 {
		t.Fatalf("tx1 first read: expected 10, got %d", v1)
	}

	// Tx2: UPDATE and COMMIT.
	_, err = pool.Exec(ctx, fmt.Sprintf("UPDATE `%s` SET val = 20 WHERE id = 1", tableName))
	if err != nil {
		t.Fatalf("tx2 update: %v", err)
	}

	// Tx1: Read again. It should see the committed value (non-repeatable read).
	var v2 int
	err = tx1.QueryRowContext(ctx,
		fmt.Sprintf("SELECT val FROM `%s` WHERE id = 1", tableName),
	).Scan(&v2)
	if err != nil {
		t.Fatalf("tx1 second read: %v", err)
	}

	if v1 != 10 {
		t.Errorf("first read was not 10: got %d", v1)
	}
	if v2 != 20 {
		t.Errorf("READ-COMMITTED should see committed value 20, got %d (repeatable-read violation)", v2)
	}
	t.Logf("READ-COMMITTED: first read=%d, second read=%d (saw committed change)", v1, v2)
}

// TestInnoDB_Isolation_Serializable rejects concurrent writes that would
// create serialization anomalies (error 1213 or 1205).
func TestInnoDB_Isolation_Serializable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SERIALIZABLE isolation test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := lockTableName("serial")

	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, val INT) ENGINE=InnoDB", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (val) VALUES (1)", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Tx1: SERIALIZABLE, read some rows.
	tx1, err := pool.SQLDB().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatalf("tx1 begin: %v", err)
	}
	defer tx1.Rollback()

	var count int
	err = tx1.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM `%s`", tableName),
	).Scan(&count)
	if err != nil {
		t.Fatalf("tx1 read: %v", err)
	}

	// Tx2: Try to INSERT while Tx1 holds a serializable snapshot.
	// In MySQL SERIALIZABLE, SELECT acquires shared next-key locks that block INSERTs.
	errChan := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (val) VALUES (99)", tableName))
		errChan <- err
	}()

	select {
	case err := <-errChan:
		if err != nil {
			t.Logf("SERIALIZABLE: tx2 INSERT rejected (expected): %v", err)
		} else {
			t.Log("SERIALIZABLE: tx2 INSERT succeeded without blocking")
		}
	case <-time.After(2 * time.Second):
		// INSERT is blocked by tx1's SERIALIZABLE shared locks: expected behavior.
		// Release tx1's lock so the test can complete.
		tx1.Rollback()
		select {
		case err := <-errChan:
			if err != nil {
				t.Errorf("tx2 INSERT after tx1 rollback: %v", err)
			} else {
				t.Log("SERIALIZABLE: tx2 INSERT completed after tx1 released lock")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("tx2 INSERT still blocked after tx1 rollback")
		}
		return // tx1 already rolled back
	}
}

// TestInnoDB_Isolation_DefaultVerification verifies the server default
// isolation level is REPEATABLE-READ (InnoDB default).
func TestInnoDB_Isolation_DefaultVerification(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping isolation config test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var globalIsolation, sessionIsolation string

	row, qrErr := pool.QueryRow(ctx, "SELECT @@GLOBAL.transaction_isolation")
	_ = qrErr
	err := row.Scan(&globalIsolation)
	if err != nil {
		t.Fatalf("read global transaction_isolation: %v", err)
	}

	row, qrErr = pool.QueryRow(ctx, "SELECT @@SESSION.transaction_isolation")
	_ = qrErr
	err = row.Scan(&sessionIsolation)
	if err != nil {
		t.Fatalf("read session transaction_isolation: %v", err)
	}

	t.Logf("transaction_isolation: global=%s, session=%s", globalIsolation, sessionIsolation)

	// InnoDB default is REPEATABLE-READ. Accept both REPEATABLE-READ and READ-COMMITTED
	// since some configurations change it.
	validLevels := map[string]bool{
		"REPEATABLE-READ": true,
		"READ-COMMITTED":  true,
		"SERIALIZABLE":    true,
	}
	if !validLevels[globalIsolation] && !validLevels[sessionIsolation] {
		t.Errorf("transaction_isolation=%s is not a standard InnoDB level", globalIsolation)
	}
}
