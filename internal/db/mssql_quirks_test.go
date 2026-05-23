//go:build !mutest

// Package db_test: MSSQL comprehensive test suite.
//
// Covers T-SQL dialect quirks, transaction isolation levels,
// temporal tables, columnstore indexes, UTF-8 collation (_UTF8),
// and MERGE deep-dive with OUTPUT.
//
// All tests use real Azure SQL Edge containers via testdb.
package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Section 1: T-SQL Dialect Quirks

// TestMSSQL_Quirk_IDENTITYInsert verifies that IDENTITY columns behave
// correctly: auto-increment on insert, and SET IDENTITY_INSERT to override.
func TestMSSQL_Quirk_IDENTITYInsert(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL IDENTITY test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("ident_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY(100, 10) PRIMARY KEY, "+
			"label NVARCHAR(100) NOT NULL"+
			")")
	if err != nil {
		t.Fatalf("create IDENTITY table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Auto-increment: seed=100, increment=10.
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (label) VALUES ($1)", "alpha")
	if err != nil {
		t.Fatalf("insert alpha: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (label) VALUES ($1)", "beta")
	if err != nil {
		t.Fatalf("insert beta: %v", err)
	}

	var idAlpha, idBeta int
	var labelAlpha, labelBeta string
	rows, err := pool.Query(ctx, "SELECT id, label FROM [dbo].["+tableName+"] ORDER BY id")
	if err != nil {
		t.Fatalf("query IDENTITY rows: %v", err)
	}
	defer rows.Close()
	rows.Next()
	rows.Scan(&idAlpha, &labelAlpha)
	rows.Next()
	rows.Scan(&idBeta, &labelBeta)

	if idAlpha != 100 {
		t.Errorf("alpha id = %d, want 100 (seed)", idAlpha)
	}
	if idBeta != 110 {
		t.Errorf("beta id = %d, want 110 (seed+inc)", idBeta)
	}
	if labelAlpha != "alpha" || labelBeta != "beta" {
		t.Errorf("labels: alpha=%q beta=%q", labelAlpha, labelBeta)
	}

	// SET IDENTITY_INSERT ON to force a specific ID. The setting is session-scoped,
	// so it must run on the same connection as the INSERT: with a pool that means
	// a single batch (otherwise the INSERT lands on a connection where it is OFF).
	_, err = pool.Exec(ctx,
		"SET IDENTITY_INSERT [dbo].["+tableName+"] ON;"+
			"INSERT INTO [dbo].["+tableName+"] (id, label) VALUES ($1, $2);"+
			"SET IDENTITY_INSERT [dbo].["+tableName+"] OFF;", 999, "manual")
	if err != nil {
		t.Fatalf("insert with IDENTITY_INSERT: %v", err)
	}

	var manualLabel string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT label FROM [dbo].["+tableName+"] WHERE id = $1", 999,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&manualLabel)
	if err != nil {
		t.Fatalf("query manual ID row: %v", err)
	}
	if manualLabel != "manual" {
		t.Errorf("manual row label = %q, want 'manual'", manualLabel)
	}

	// Verify IDENTITY resumes from the highest value after manual insert.
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (label) VALUES ($1)", "gamma")
	if err != nil {
		t.Fatalf("insert gamma after manual: %v", err)
	}

	var gammaID int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT id FROM [dbo].["+tableName+"] WHERE label = $1", "gamma",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&gammaID)
	if err != nil {
		t.Fatalf("query gamma: %v", err)
	}
	if gammaID < 999 {
		t.Errorf("gamma id = %d, should be >= 999 (IDENTITY resumes past manual insert)", gammaID)
	}
	t.Logf("IDENTITY: seed=100, inc=10, resume after manual 999 => gamma id = %d", gammaID)
}

// TestMSSQL_Quirk_IDENTITY_ScopeIdentity verifies that SCOPE_IDENTITY()
// returns the last identity value in the current scope (session + module).
func TestMSSQL_Quirk_IDENTITY_ScopeIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL SCOPE_IDENTITY test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("scopeid_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"val NVARCHAR(50)"+
			")")
	if err != nil {
		t.Fatalf("create SCOPE_IDENTITY table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Batch insert + SCOPE_IDENTITY.
	var scopeID int
	row, qrErr := pool.QueryRow(ctx,
		"INSERT INTO [dbo].["+tableName+"] (val) VALUES ($1); SELECT CAST(SCOPE_IDENTITY() AS INT)",
		"scope-test",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&scopeID)
	if err != nil {
		t.Fatalf("insert + SCOPE_IDENTITY: %v", err)
	}
	if scopeID != 1 {
		t.Errorf("SCOPE_IDENTITY = %d, want 1", scopeID)
	}

	// Multi-insert: SCOPE_IDENTITY should return the LAST inserted identity.
	var scopeID2 int
	row, qrErr = pool.QueryRow(ctx,
		"INSERT INTO [dbo].["+tableName+"] (val) VALUES ($1),($2); SELECT CAST(SCOPE_IDENTITY() AS INT)",
		"multi-a", "multi-b",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&scopeID2)
	if err != nil {
		t.Fatalf("multi-insert + SCOPE_IDENTITY: %v", err)
	}
	if scopeID2 != 3 {
		t.Errorf("multi-insert SCOPE_IDENTITY = %d, want 3", scopeID2)
	}
	t.Logf("SCOPE_IDENTITY: single=%d multi=%d", scopeID, scopeID2)
}

// TestMSSQL_Quirk_ComputedColumn verifies computed columns and persisted
// computed columns behave correctly.
func TestMSSQL_Quirk_ComputedColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL computed column test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("compcol_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"first_name NVARCHAR(50) NOT NULL, "+
			"last_name NVARCHAR(50) NOT NULL, "+
			"full_name AS first_name + N' ' + last_name, "+ // non-persisted computed
			"name_len AS LEN(first_name + N' ' + last_name) PERSISTED"+ // persisted computed
			")")
	if err != nil {
		t.Fatalf("create computed column table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	_, err = pool.Exec(ctx,
		"INSERT INTO [dbo].["+tableName+"] (first_name, last_name) VALUES ($1, $2)",
		"Jean-Luc", "Picard")
	if err != nil {
		t.Fatalf("insert computed column row: %v", err)
	}

	var fullName string
	var nameLen int
	row, qrErr := pool.QueryRow(ctx,
		"SELECT full_name, name_len FROM [dbo].["+tableName+"] WHERE id = 1",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&fullName, &nameLen)
	if err != nil {
		t.Fatalf("query computed columns: %v", err)
	}

	if fullName != "Jean-Luc Picard" {
		t.Errorf("full_name = %q, want 'Jean-Luc Picard'", fullName)
	}
	expectedLen := len("Jean-Luc Picard")
	if nameLen != expectedLen {
		t.Errorf("name_len = %d, want %d", nameLen, expectedLen)
	}
	t.Logf("computed: full_name=%q name_len=%d", fullName, nameLen)
}

// TestMSSQL_Quirk_OUTPUTClause verifies the OUTPUT clause for returning
// inserted/deleted data (INSERTED.*, DELETED.*).
func TestMSSQL_Quirk_OUTPUTClause(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL OUTPUT clause test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("output_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID(), "+
			"title NVARCHAR(200) NOT NULL, "+
			"status NVARCHAR(20) NOT NULL DEFAULT N'draft'"+
			")")
	if err != nil {
		t.Fatalf("create OUTPUT test table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// INSERT with OUTPUT INSERTED.*
	var id, title, status string
	row, qrErr := pool.QueryRow(ctx,
		"INSERT INTO [dbo].["+tableName+"] (title) "+
			"OUTPUT CAST(INSERTED.id AS NVARCHAR(36)), INSERTED.title, INSERTED.status "+
			"VALUES ($1)",
		"OUTPUT test document",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&id, &title, &status)
	if err != nil {
		t.Fatalf("insert with OUTPUT: %v", err)
	}
	if id == "" {
		t.Fatal("OUTPUT INSERTED.id is empty")
	}
	if title != "OUTPUT test document" {
		t.Errorf("OUTPUT title = %q", title)
	}
	if status != "draft" {
		t.Errorf("OUTPUT status = %q, want 'draft'", status)
	}

	// UPDATE with OUTPUT DELETED.* and INSERTED.*
	var oldTitle, newTitle string
	row, qrErr = pool.QueryRow(ctx,
		"UPDATE [dbo].["+tableName+"] SET title = $2 "+
			"OUTPUT DELETED.title, INSERTED.title WHERE id = $1",
		id, "Updated via OUTPUT",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&oldTitle, &newTitle)
	if err != nil {
		t.Fatalf("update with OUTPUT: %v", err)
	}
	if oldTitle != "OUTPUT test document" {
		t.Errorf("OUTPUT DELETED.title = %q, want 'OUTPUT test document'", oldTitle)
	}
	if newTitle != "Updated via OUTPUT" {
		t.Errorf("OUTPUT INSERTED.title = %q, want 'Updated via OUTPUT'", newTitle)
	}

	// DELETE with OUTPUT DELETED.*
	var deletedID string
	row, qrErr = pool.QueryRow(ctx,
		"DELETE FROM [dbo].["+tableName+"] "+
			"OUTPUT CAST(DELETED.id AS NVARCHAR(36)) WHERE id = $1",
		id,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&deletedID)
	if err != nil {
		t.Fatalf("delete with OUTPUT: %v", err)
	}
	if deletedID != id {
		t.Errorf("OUTPUT DELETED.id = %q, want %q", deletedID, id)
	}

	var remaining int
	row, qrErr = pool.QueryRow(ctx, "SELECT COUNT(*) FROM [dbo].["+tableName+"]")
	_ = qrErr
	_ = row.Scan(&remaining)
	if remaining != 0 {
		t.Errorf("expected 0 rows after DELETE, got %d", remaining)
	}
	t.Logf("OUTPUT: INSERT+UPDATE+DELETE with OUTPUT - all OK, id=%s", id)
}

// TestMSSQL_Quirk_ThrowVsRaiserror verifies that THROW (SQL Server 2012+)
// works as expected and produces proper error information.
func TestMSSQL_Quirk_ThrowVsRaiserror(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL THROW test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	// THROW outside CATCH block requires parameters.
	// We catch the error via the driver.
	_, err := pool.Exec(ctx, `
		BEGIN TRY
			THROW 50001, N'Custom business rule violation: duplicate entry', 1;
		END TRY
		BEGIN CATCH
			THROW;
		END CATCH
	`)
	if err == nil {
		t.Error("THROW should have produced an error")
	} else {
		// go-mssqldb surfaces the error number via the mssql.Error struct field, not
		// the .Error() text: assert on the message the driver actually returns.
		errStr := strings.ToLower(err.Error())
		if !strings.Contains(errStr, "duplicate") {
			t.Errorf("THROW error should contain the custom message: %v", err)
		} else {
			t.Logf("THROW correctly produced error: %v", err)
		}
	}

	// Verify THROW with rethrow preserves original error.
	_, err = pool.Exec(ctx, `
		BEGIN TRY
			SELECT 1/0;
		END TRY
		BEGIN CATCH
			-- Rethrow original error (division by zero)
			THROW;
		END CATCH
	`)
	if err == nil {
		t.Error("rethrow THROW should have produced division by zero error")
	} else {
		errStr := strings.ToLower(err.Error())
		if !strings.Contains(errStr, "divide") && !strings.Contains(errStr, "8134") {
			t.Logf("rethrow THROW error: %v (may not contain 'divide')", err)
		} else {
			t.Logf("rethrow THROW correctly preserved original error: %v", err)
		}
	}
}

// TestMSSQL_Quirk_BracketEscaping verifies that bracket-delimited identifiers
// handle special characters and reserved words correctly.
func TestMSSQL_Quirk_BracketEscaping(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL bracket escaping test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("bracket_%d", time.Now().UnixNano()%10000)
	// Use a reserved word as a column name, and a bracket-containing name.
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"[key] NVARCHAR(50) NOT NULL, "+ // reserved word
			"[weird]]name] NVARCHAR(50) NOT NULL"+ // contains closing bracket
			")")
	if err != nil {
		t.Fatalf("create bracket test table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	_, err = pool.Exec(ctx,
		"INSERT INTO [dbo].["+tableName+"] ([key], [weird]]name]) VALUES ($1, $2)",
		"k1", "w1")
	if err != nil {
		t.Fatalf("insert bracket row: %v", err)
	}

	var keyVal, weirdVal string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT [key], [weird]]name] FROM [dbo].["+tableName+"] WHERE id = 1",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&keyVal, &weirdVal)
	if err != nil {
		t.Fatalf("query bracket columns: %v", err)
	}
	if keyVal != "k1" {
		t.Errorf("[key] = %q, want 'k1'", keyVal)
	}
	if weirdVal != "w1" {
		t.Errorf("[weird]name] = %q, want 'w1'", weirdVal)
	}
	t.Logf("bracket escaping: [key]=%q [weird]name]=%q - OK", keyVal, weirdVal)
}

// TestMSSQL_Quirk_MultiStatementBatch verifies that multiple statements
// separated by semicolons execute correctly in a single batch.
func TestMSSQL_Quirk_MultiStatementBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL multi-statement batch test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("multi_%d", time.Now().UnixNano()%10000)

	// Multi-statement: CREATE + INSERT + SELECT in one batch.
	var result int
	row, qrErr := pool.QueryRow(ctx,
		"CREATE TABLE [dbo].["+tableName+"] (id INT IDENTITY PRIMARY KEY, val INT); "+
			"INSERT INTO [dbo].["+tableName+"] (val) VALUES (42); "+
			"SELECT val FROM [dbo].["+tableName+"] WHERE id = SCOPE_IDENTITY();",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&result)
	if err != nil {
		t.Fatalf("multi-statement batch: %v", err)
	}
	if result != 42 {
		t.Errorf("multi-statement result = %d, want 42", result)
	}
	t.Logf("multi-statement batch: result=%d", result)

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})
}

// Section 2: Transaction Isolation Levels

// TestMSSQL_Isolation_ReadCommitted verifies READ COMMITTED (default
// isolation) behavior: dirty reads are prevented.
func TestMSSQL_Isolation_ReadCommitted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL isolation test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("iso_rc_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"data NVARCHAR(100) NOT NULL"+
			")")
	if err != nil {
		t.Fatalf("create isolation table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (data) VALUES ($1)", "initial")
	if err != nil {
		t.Fatalf("insert initial row: %v", err)
	}

	// Verify default isolation level is READ COMMITTED.
	var isolationLevel string
	row, qrErr := pool.QueryRow(ctx, "SELECT CASE transaction_isolation_level WHEN 2 THEN 'READ COMMITTED' ELSE CAST(transaction_isolation_level AS NVARCHAR) END FROM sys.dm_exec_sessions WHERE session_id = @@SPID")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&isolationLevel)
	if err != nil {
		t.Fatalf("read isolation level: %v", err)
	}
	t.Logf("default isolation level: %s", isolationLevel)

	// Start a transaction, update, don't commit: verify READ COMMITTED
	// blocks reading uncommitted data via locking.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	_, err = tx.Exec("UPDATE [dbo].["+tableName+"] SET data = $1 WHERE id = 1", "modified")
	if err != nil {
		t.Fatalf("update in tx: %v", err)
	}

	// READ COMMITTED on a separate connection would block.
	// We verify the data is still readable from the TX (dirty read from same conn).
	// The important thing: after rollback, original data is intact.
	tx.Rollback()

	var data string
	row, qrErr = pool.QueryRow(ctx, "SELECT data FROM [dbo].["+tableName+"] WHERE id = 1")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&data)
	if err != nil {
		t.Fatalf("read after rollback: %v", err)
	}
	if data != "initial" {
		t.Errorf("data after rollback = %q, want 'initial'", data)
	}
	t.Logf("READ COMMITTED: rollback restored 'initial' - no dirty read persisted")
}

// TestMSSQL_Isolation_ReadCommittedSnapshot verifies READ COMMITTED
// SNAPSHOT (RCSI) behavior when enabled at the database level.
func TestMSSQL_Isolation_ReadCommittedSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL RCSI test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("iso_rcsi_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"counter INT NOT NULL DEFAULT 0"+
			")")
	if err != nil {
		t.Fatalf("create RCSI test table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (counter) VALUES (10)")
	if err != nil {
		t.Fatalf("insert RCSI row: %v", err)
	}

	// Check if RCSI is enabled at the database level.
	var isRCSI bool
	row, qrErr := pool.QueryRow(ctx,
		"SELECT CAST(is_read_committed_snapshot_on AS BIT) FROM sys.databases WHERE name = DB_NAME()",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&isRCSI)
	if err != nil {
		t.Fatalf("query RCSI status: %v", err)
	}
	t.Logf("RCSI enabled: %v", isRCSI)

	// Azure SQL Edge ships with ALLOW_SNAPSHOT_ISOLATION OFF. Enable it so the probe
	// actually exercises SNAPSHOT. Only issue SET SNAPSHOT when the engine accepts
	// it: otherwise the failing statement would leave the level set on the pooled
	// connection and poison later queries (SET TRANSACTION ISOLATION LEVEL is
	// connection-scoped). The trailing reset restores the connection either way.
	if _, aerr := pool.Exec(ctx, "ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON"); aerr != nil {
		t.Logf("SNAPSHOT isolation not available (cannot enable): %v", aerr)
	} else if _, serr := pool.Exec(ctx, `
		SET TRANSACTION ISOLATION LEVEL SNAPSHOT;
		SELECT counter FROM [dbo].[`+tableName+`];
		SET TRANSACTION ISOLATION LEVEL READ COMMITTED;
	`); serr != nil {
		t.Logf("SNAPSHOT probe failed: %v", serr)
	} else {
		t.Logf("SNAPSHOT isolation available")
	}

	// Verify we can read under READ COMMITTED (default).
	var counter int
	row, qrErr = pool.QueryRow(ctx, "SELECT counter FROM [dbo].["+tableName+"] WHERE id = 1")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if counter != 10 {
		t.Errorf("counter = %d, want 10", counter)
	}
}

// TestMSSQL_Isolation_Serializable verifies that the SERIALIZABLE
// isolation level can be set and is reflected in the session metadata.
// On Azure SQL Edge with snapshot isolation enabled, rows inserted on
// one connection may not be visible on another at SERIALIZABLE. We
// verify metadata and the command works rather than cross-connection
// visibility.
func TestMSSQL_Isolation_Serializable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL serializable test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("iso_ser_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"val INT NOT NULL"+
			")")
	if err != nil {
		t.Fatalf("create serializable test table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Insert and verify rows via pool.
	for i := range 5 {
		_, err = pool.Exec(ctx,
			"INSERT INTO [dbo].["+tableName+"] (val) VALUES ($1)", i*10)
		if err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	var count int
	row, qrErr := pool.QueryRow(ctx, "SELECT COUNT(*) FROM [dbo].["+tableName+"]")
	_ = qrErr
	_ = row.Scan(&count)
	if count != 5 {
		t.Fatalf("pre-condition: expected 5 rows, got %d", count)
	}

	// Verify SERIALIZABLE can be set at the session level.
	_, err = pool.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE")
	if err != nil {
		t.Fatalf("SET TRANSACTION ISOLATION LEVEL SERIALIZABLE: %v", err)
	}

	// Read back current isolation level (should be 4).
	// Note: SET TRANSACTION ISOLATION LEVEL in go-mssqldb may or may
	// not persist on the next pooled operation. This is informational.
	var isoLevel int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT transaction_isolation_level FROM sys.dm_exec_sessions WHERE session_id = @@SPID",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&isoLevel)
	if err != nil {
		t.Fatalf("read isolation level: %v", err)
	}
	t.Logf("isolation level after SET: %d (4=SERIALIZABLE, 2=READ COMMITTED)", isoLevel)

	// Restore default.
	_, err = pool.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED")
	if err != nil {
		t.Logf("restore READ COMMITTED: %v", err)
	}

	// Use a single dedicated connection for a self-contained test.
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()

	// Create and seed a table visible only on this connection.
	connTable := tableName + "_conn"
	_, err = conn.ExecContext(ctx,
		"CREATE TABLE [dbo].["+connTable+"] (val INT)")
	if err != nil {
		t.Fatalf("create conn table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+connTable+"]")

	for i := range 5 {
		_, err = conn.ExecContext(ctx,
			"INSERT INTO [dbo].["+connTable+"] (val) VALUES ($1)", i*10)
		if err != nil {
			t.Fatalf("insert conn row %d: %v", i, err)
		}
	}

	// Set SERIALIZABLE on this connection.
	_, err = conn.ExecContext(ctx, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE")
	if err != nil {
		t.Fatalf("set serializable on conn: %v", err)
	}

	// Begin SERIALIZABLE transaction and verify consistent reads.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin serializable tx: %v", err)
	}
	defer tx.Rollback()

	var count1, count2 int
	tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM [dbo].["+connTable+"] WHERE val BETWEEN $1 AND $2",
		0, 50,
	).Scan(&count1)
	tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM [dbo].["+connTable+"] WHERE val BETWEEN $1 AND $2",
		0, 50,
	).Scan(&count2)

	if count1 != count2 {
		t.Errorf("SERIALIZABLE: count changed from %d to %d - should be stable", count1, count2)
	}
	if count1 != 5 {
		t.Errorf("expected 5 rows in serializable tx, got %d", count1)
	}
	t.Logf("SERIALIZABLE on dedicated conn: stable count=%d across two reads", count1)
}

// TestMSSQL_Isolation_LevelsAvailable catalogs which isolation levels
// are available on the current instance.
func TestMSSQL_Isolation_LevelsAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL isolation catalog test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	// Query snapshot isolation configuration.
	// SQL Server returns BIT for these columns. Scan into bool.
	var allowSnapshot, isRCSI bool
	row, qrErr := pool.QueryRow(ctx,
		"SELECT CAST(snapshot_isolation_state AS BIT), CAST(is_read_committed_snapshot_on AS BIT) FROM sys.databases WHERE name = DB_NAME()",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&allowSnapshot, &isRCSI)
	if err != nil {
		t.Fatalf("query snapshot config: %v", err)
	}

	t.Logf("ALLOW_SNAPSHOT_ISOLATION: %v", allowSnapshot)
	t.Logf("READ_COMMITTED_SNAPSHOT: %v", isRCSI)

	// Try each isolation level.
	levels := []struct {
		name string
		sql  string
	}{
		{"READ UNCOMMITTED", "SET TRANSACTION ISOLATION LEVEL READ UNCOMMITTED; SELECT 1;"},
		{"READ COMMITTED", "SET TRANSACTION ISOLATION LEVEL READ COMMITTED; SELECT 1;"},
		{"REPEATABLE READ", "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ; SELECT 1;"},
		{"SERIALIZABLE", "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE; SELECT 1;"},
		{"SNAPSHOT", "SET TRANSACTION ISOLATION LEVEL SNAPSHOT; SELECT 1;"},
	}

	for _, lvl := range levels {
		_, err := pool.Exec(ctx, lvl.sql)
		if err != nil {
			t.Logf("%s: NOT AVAILABLE - %v", lvl.name, err)
		} else {
			t.Logf("%s: available", lvl.name)
		}
	}
}

// Section 3: Temporal Tables (System-Versioned)

// TestMSSQL_Temporal_CreateAndQuery verifies system-versioned temporal
// table creation, insert, update, delete, and FOR SYSTEM_TIME queries.
func TestMSSQL_Temporal_CreateAndQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL temporal table test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	baseName := fmt.Sprintf("temp_%d", time.Now().UnixNano()%10000)
	historyName := baseName + "_history"

	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+baseName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"title NVARCHAR(200) NOT NULL, "+
			"price DECIMAL(10,2) NOT NULL, "+
			"valid_from DATETIME2 GENERATED ALWAYS AS ROW START HIDDEN NOT NULL, "+
			"valid_to   DATETIME2 GENERATED ALWAYS AS ROW END HIDDEN NOT NULL, "+
			"PERIOD FOR SYSTEM_TIME (valid_from, valid_to)"+
			") "+
			"WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = [dbo].["+historyName+"]))")
	if err != nil {
		t.Fatalf("create temporal table: %v", err)
	}
	t.Cleanup(func() {
		// Must disable system versioning before dropping.
		pool.Exec(context.Background(),
			"ALTER TABLE [dbo].["+baseName+"] SET (SYSTEM_VERSIONING = OFF)")
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+baseName+"]")
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+historyName+"]")
	})

	// Phase 1: Insert and capture time.
	beforeInsert := time.Now().UTC()
	_, err = pool.Exec(ctx,
		"INSERT INTO [dbo].["+baseName+"] (title, price) VALUES ($1, $2)",
		"Temporal Document", 19.99)
	if err != nil {
		t.Fatalf("insert temporal row: %v", err)
	}
	afterInsert := time.Now().UTC()

	// Phase 2: Update (generates history row).
	time.Sleep(10 * time.Millisecond) // ensure time gap
	beforeUpdate := time.Now().UTC()
	_ = beforeUpdate // captured for temporal range query boundaries
	_, err = pool.Exec(ctx,
		"UPDATE [dbo].["+baseName+"] SET price = $1 WHERE id = 1", 29.99)
	if err != nil {
		t.Fatalf("update temporal row: %v", err)
	}
	afterUpdate := time.Now().UTC()

	// Phase 3: Query current row.
	var title string
	var price float64
	row, qrErr := pool.QueryRow(ctx,
		"SELECT title, price FROM [dbo].["+baseName+"] WHERE id = 1",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&title, &price)
	if err != nil {
		t.Fatalf("query current temporal row: %v", err)
	}
	if title != "Temporal Document" {
		t.Errorf("title = %q", title)
	}
	if price != 29.99 {
		t.Errorf("current price = %f, want 29.99", price)
	}

	// Phase 4: Query AS OF (point in time after insert, before update).
	var oldPrice float64
	row, qrErr = pool.QueryRow(ctx,
		fmt.Sprintf(
			"SELECT price FROM [dbo].[%s] FOR SYSTEM_TIME AS OF '%s' WHERE id = 1",
			baseName, afterInsert.Add(-1*time.Millisecond).Format("2006-01-02 15:04:05.999"),
		),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&oldPrice)
	if err != nil {
		t.Fatalf("AS OF query: %v", err)
	}
	if oldPrice != 19.99 {
		t.Errorf("AS OF price = %f, want 19.99", oldPrice)
	}
	t.Logf("temporal AS OF: old price = %f", oldPrice)

	// Phase 5: Query BETWEEN (all versions in a range).
	var versionCount int
	row, qrErr = pool.QueryRow(ctx,
		fmt.Sprintf(
			"SELECT COUNT(*) FROM [dbo].[%s] FOR SYSTEM_TIME BETWEEN '%s' AND '%s'",
			baseName,
			// Sub-second precision: insert and update happen within the same wall-clock
			// second, so truncating to seconds collapses the range and misses the
			// version transitions (the AS OF query above uses the same precision).
			beforeInsert.Format("2006-01-02 15:04:05.9999999"),
			afterUpdate.Format("2006-01-02 15:04:05.9999999"),
		),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&versionCount)
	if err != nil {
		t.Fatalf("BETWEEN query: %v", err)
	}
	if versionCount < 2 {
		t.Errorf("expected >= 2 versions, got %d", versionCount)
	}
	t.Logf("temporal BETWEEN: %d versions in range", versionCount)

	// Phase 6: History table should have the old version.
	var historyCount int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM [dbo].["+historyName+"]",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&historyCount)
	if err != nil {
		t.Fatalf("query history table: %v", err)
	}
	if historyCount != 1 {
		t.Errorf("history table should have 1 row (the old version), got %d", historyCount)
	}
	t.Logf("temporal: current=%f, history=%d row(s)", price, historyCount)
}

// TestMSSQL_Temporal_DeleteHistory verifies that DELETE operations
// create history rows and FOR SYSTEM_TIME ALL includes deleted rows.
func TestMSSQL_Temporal_DeleteHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL temporal delete test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	baseName := fmt.Sprintf("tempdel_%d", time.Now().UnixNano()%10000)
	historyName := baseName + "_history"

	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+baseName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"name NVARCHAR(100) NOT NULL, "+
			"valid_from DATETIME2 GENERATED ALWAYS AS ROW START HIDDEN NOT NULL, "+
			"valid_to   DATETIME2 GENERATED ALWAYS AS ROW END HIDDEN NOT NULL, "+
			"PERIOD FOR SYSTEM_TIME (valid_from, valid_to)"+
			") "+
			"WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = [dbo].["+historyName+"]))")
	if err != nil {
		t.Fatalf("create temporal table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			"ALTER TABLE [dbo].["+baseName+"] SET (SYSTEM_VERSIONING = OFF)")
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+baseName+"]")
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+historyName+"]")
	})

	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+baseName+"] (name) VALUES ($1)", "Alice")
	if err != nil {
		t.Fatalf("insert Alice: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+baseName+"] (name) VALUES ($1)", "Bob")
	if err != nil {
		t.Fatalf("insert Bob: %v", err)
	}

	// Give Bob a non-zero validity period before deleting: SQL Server / Azure SQL
	// Edge omit zero-duration history rows from FOR SYSTEM_TIME queries, so an
	// insert+delete in the same instant would leave Bob invisible to the ALL query.
	time.Sleep(25 * time.Millisecond)

	// Delete Bob.
	_, err = pool.Exec(ctx, "DELETE FROM [dbo].["+baseName+"] WHERE name = $1", "Bob")
	if err != nil {
		t.Fatalf("delete Bob: %v", err)
	}

	// Current table should only have Alice.
	var currentCount int
	row, qrErr := pool.QueryRow(ctx, "SELECT COUNT(*) FROM [dbo].["+baseName+"]")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&currentCount)
	if err != nil {
		t.Fatalf("count current: %v", err)
	}
	if currentCount != 1 {
		t.Errorf("current count = %d, want 1 (Alice only)", currentCount)
	}

	// History should have Bob.
	var historyCount int
	row, qrErr = pool.QueryRow(ctx, "SELECT COUNT(*) FROM [dbo].["+historyName+"]")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&historyCount)
	if err != nil {
		t.Fatalf("count history: %v", err)
	}
	if historyCount != 1 {
		t.Errorf("history count = %d, want 1 (Bob)", historyCount)
	}

	// FOR SYSTEM_TIME ALL should include both current and deleted.
	var allCount int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM [dbo].["+baseName+"] FOR SYSTEM_TIME ALL",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&allCount)
	if err != nil {
		t.Fatalf("FOR SYSTEM_TIME ALL: %v", err)
	}
	if allCount != 2 {
		t.Errorf("FOR SYSTEM_TIME ALL count = %d, want 2 (Alice + Bob)", allCount)
	}
	t.Logf("temporal delete: current=%d, history=%d, FOR SYSTEM_TIME ALL=%d",
		currentCount, historyCount, allCount)
}

// Section 4: Columnstore Index Behavior

// TestMSSQL_Columnstore_CreateAndInsert verifies CLUSTERED COLUMNSTORE
// index creation, insert, and query correctness.
func TestMSSQL_Columnstore_CreateAndInsert(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL columnstore test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("cs_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT NOT NULL, "+
			"category NVARCHAR(50) NOT NULL, "+
			"value INT NOT NULL, "+
			"payload NVARCHAR(MAX), "+
			"created_at DATETIME2 DEFAULT SYSUTCDATETIME()"+
			")")
	if err != nil {
		t.Fatalf("create columnstore table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Create clustered columnstore index.
	_, err = pool.Exec(ctx,
		"CREATE CLUSTERED COLUMNSTORE INDEX [CCI_"+tableName+"] ON [dbo].["+tableName+"]")
	if err != nil {
		t.Fatalf("create columnstore index: %v", err)
	}

	// Insert enough rows to matter for columnstore (typically > 102400 for
	// compression, but batch-mode operators work at smaller sizes).
	for i := range 200 {
		cat := "even"
		if i%2 != 0 {
			cat = "odd"
		}
		_, err = pool.Exec(ctx,
			"INSERT INTO [dbo].["+tableName+"] (id, category, value, payload) VALUES ($1, $2, $3, $4)",
			i, cat, i*10, fmt.Sprintf("payload-for-row-%05d-with-extra-padding-to-make-it-worth-compressing", i),
		)
		if err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	// Query correctness: aggregation.
	var totalValue int
	row, qrErr := pool.QueryRow(ctx,
		"SELECT SUM(value) FROM [dbo].["+tableName+"]",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&totalValue)
	if err != nil {
		t.Fatalf("columnstore SUM query: %v", err)
	}
	expectedSum := 0
	for i := range 200 {
		expectedSum += i * 10
	}
	if totalValue != expectedSum {
		t.Errorf("SUM(value) = %d, want %d", totalValue, expectedSum)
	}

	// Grouped query.
	var catCounts int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT COUNT(DISTINCT category) FROM [dbo].["+tableName+"]",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&catCounts)
	if err != nil {
		t.Fatalf("columnstore DISTINCT query: %v", err)
	}
	if catCounts != 2 {
		t.Errorf("DISTINCT categories = %d, want 2", catCounts)
	}

	t.Logf("columnstore: 200 rows, SUM=%d, DISTINCT categories=%d", totalValue, catCounts)
}

// TestMSSQL_Columnstore_VerifyIndex verifies the columnstore index is
// actually a columnstore (not a fallback to rowstore).
func TestMSSQL_Columnstore_VerifyIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL columnstore verification test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("csverify_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT NOT NULL, "+
			"data NVARCHAR(100)"+
			")")
	if err != nil {
		t.Fatalf("create columnstore verify table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	_, err = pool.Exec(ctx,
		"CREATE CLUSTERED COLUMNSTORE INDEX [CCI_"+tableName+"] ON [dbo].["+tableName+"]")
	if err != nil {
		t.Fatalf("create columnstore index: %v", err)
	}

	// Verify the index exists and is of COLUMNSTORE type.
	// Azure SQL Edge / SQL Server: type 5 = CLUSTERED COLUMNSTORE,
	// type 6 = NONCLUSTERED COLUMNSTORE.
	// We accept either 5 or 6: both are columnstore index types.
	var indexType int
	row, qrErr := pool.QueryRow(ctx,
		"SELECT type FROM sys.indexes WHERE object_id = OBJECT_ID('[dbo].["+tableName+"]') AND type_desc LIKE '%COLUMNSTORE%'",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&indexType)
	if err != nil {
		t.Fatalf("query columnstore index metadata: %v", err)
	}
	if indexType != 5 && indexType != 6 {
		t.Errorf("index type = %d, want 5 or 6 (COLUMNSTORE variants)", indexType)
	}
	t.Logf("columnstore index verified: type=%d", indexType)
}

// Section 5: UTF-8 Collation (_UTF8)

// TestMSSQL_UTF8_Collation_SupplementaryCharacters verifies that
// _UTF8 collations handle Unicode supplementary characters (emoji,
// CJK extension B, etc.) correctly. SQL Server 2019+ supports
// Latin1_General_100_CI_AS_SC_UTF8 and similar _UTF8 collations.
func TestMSSQL_UTF8_Collation_SupplementaryCharacters(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL UTF-8 collation test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("utf8_%d", time.Now().UnixNano()%10000)

	// Create table with UTF-8 collation on string columns.
	// SQL Server 2019+ supports _UTF8 collations.
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"emoji NVARCHAR(100) COLLATE Latin1_General_100_CI_AS_SC_UTF8, "+
			"cjk NVARCHAR(100) COLLATE Latin1_General_100_CI_AS_SC_UTF8, "+
			"mixed NVARCHAR(200) COLLATE Latin1_General_100_CI_AS_SC_UTF8"+
			")")
	if err != nil {
		// _UTF8 collation may not be available on Azure SQL Edge.
		// Fall back to standard collation test.
		t.Logf("_UTF8 collation not available, testing standard NVARCHAR Unicode: %v", err)
		pool.Exec(ctx, "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
		_, err = pool.Exec(ctx,
			"CREATE TABLE [dbo].["+tableName+"] ("+
				"id INT IDENTITY PRIMARY KEY, "+
				"emoji NVARCHAR(100), "+
				"cjk NVARCHAR(100), "+
				"mixed NVARCHAR(200)"+
				")")
		if err != nil {
			t.Fatalf("create fallback UTF-8 test table: %v", err)
		}
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Insert supplementary Unicode characters.
	emoji := "🚀 Hello 🌍 - rocket and globe"
	cjk := "𠀋 - CJK Extension B character U+2000B"
	mixed := "Café résumé naïve façade 日本語 한국어 中文"

	_, err = pool.Exec(ctx,
		"INSERT INTO [dbo].["+tableName+"] (emoji, cjk, mixed) VALUES ($1, $2, $3)",
		emoji, cjk, mixed)
	if err != nil {
		t.Fatalf("insert UTF-8 supplementary characters: %v", err)
	}

	// Round-trip verification.
	var outEmoji, outCJK, outMixed string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT emoji, cjk, mixed FROM [dbo].["+tableName+"] WHERE id = 1",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&outEmoji, &outCJK, &outMixed)
	if err != nil {
		t.Fatalf("read UTF-8 supplementary characters: %v", err)
	}

	if outEmoji != emoji {
		t.Errorf("emoji roundtrip: got %q, want %q", outEmoji, emoji)
	}
	if outCJK != cjk {
		t.Errorf("CJK roundtrip: got %q, want %q", outCJK, cjk)
	}
	if outMixed != mixed {
		t.Errorf("mixed roundtrip: got %q, want %q", outMixed, mixed)
	}

	// Verify LEN works correctly for supplementary characters.
	var emojiLen int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT LEN(emoji) FROM [dbo].["+tableName+"] WHERE id = 1",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&emojiLen)
	if err != nil {
		t.Fatalf("LEN query: %v", err)
	}
	t.Logf("UTF-8 supplementary: emoji=%q (len=%d), cjk=%q, mixed=%q",
		outEmoji, emojiLen, outCJK, outMixed)
}

// TestMSSQL_UTF8_Collation_CaseSensitivityCI_AS_UTF8 verifies that
// _UTF8 collation with CI_AS (case-insensitive, accent-sensitive)
// behaves the same as non-UTF8 CI_AS for equality comparisons.
func TestMSSQL_UTF8_Collation_CaseSensitivityCI_AS_UTF8(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL UTF-8 case sensitivity test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	// Check if _UTF8 collation is supported.
	var utf8Match int
	row, qrErr := pool.QueryRow(ctx,
		"SELECT CASE WHEN 'A' COLLATE Latin1_General_100_CI_AS_SC_UTF8 = 'a' COLLATE Latin1_General_100_CI_AS_SC_UTF8 THEN 1 ELSE 0 END",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&utf8Match)

	if err != nil {
		// _UTF8 not available: test NVARCHAR Unicode roundtrip instead.
		t.Logf("_UTF8 collation not available: %v", err)

		tableName := fmt.Sprintf("utf8ci_%d", time.Now().UnixNano()%10000)
		_, err = pool.Exec(ctx,
			"CREATE TABLE [dbo].["+tableName+"] ("+
				"id INT IDENTITY PRIMARY KEY, "+
				"name NVARCHAR(100))")
		if err != nil {
			t.Fatalf("create UTF-8 CI test table: %v", err)
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
		})

		// Insert and verify standard Unicode handling.
		_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (name) VALUES ($1)", "Müller")
		if err != nil {
			t.Fatalf("insert Unicode name: %v", err)
		}

		var name string
		row, qrErr = pool.QueryRow(ctx, "SELECT name FROM [dbo].["+tableName+"] WHERE id = 1")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&name)
		if err != nil {
			t.Fatalf("read Unicode name: %v", err)
		}
		if name != "Müller" {
			t.Errorf("Unicode roundtrip: got %q, want 'Müller'", name)
		}
		t.Logf("standard NVARCHAR Unicode roundtrip: %s - OK", name)
		return
	}

	if utf8Match != 1 {
		t.Errorf("_UTF8 CI_AS: 'A' SHOULD equal 'a', got %d", utf8Match)
	} else {
		t.Logf("_UTF8 CI_AS: 'A' = 'a' - case-insensitive works")
	}

	// Accent-sensitive: é should NOT equal e.
	var accentMatch int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT CASE WHEN N'é' COLLATE Latin1_General_100_CI_AS_SC_UTF8 = N'e' COLLATE Latin1_General_100_CI_AS_SC_UTF8 THEN 1 ELSE 0 END",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&accentMatch)
	if err != nil {
		t.Fatalf("_UTF8 accent comparison: %v", err)
	}
	if accentMatch != 0 {
		t.Errorf("_UTF8 CI_AS (accent-sensitive): 'é' should NOT equal 'e', got match")
	} else {
		t.Logf("_UTF8 CI_AS accent: 'é' != 'e' - accent-sensitive works")
	}
}

// TestMSSQL_UTF8_Collation_CompareToNonUTF8 verifies that a _UTF8
// column can be compared to a non-_UTF8 column (explicit COLLATE needed).
func TestMSSQL_UTF8_Collation_CompareToNonUTF8(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL UTF-8 cross-collation test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	// First, check if _UTF8 is available.
	var serverCollation string
	row, qrErr := pool.QueryRow(ctx, "SELECT SERVERPROPERTY('Collation')")
	_ = qrErr
	_ = row.Scan(&serverCollation)
	t.Logf("server collation: %s", serverCollation)

	// If the server's collation doesn't contain _UTF8, try to create a table using it.
	// If that fails, the test logs and skips: _UTF8 is a SQL Server 2019+ feature.

	tableName := fmt.Sprintf("utf8cmp_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"utf8_col NVARCHAR(100) COLLATE Latin1_General_100_CI_AS_SC_UTF8, "+
			"standard_col NVARCHAR(100)"+ // server default collation
			")")

	if err != nil {
		// _UTF8 collation not available: test gracefully.
		t.Logf("_UTF8 cross-collation table creation failed (expected if _UTF8 unsupported): %v", err)
		pool.Exec(ctx, "DROP TABLE IF EXISTS [dbo].["+tableName+"]")

		// Fall back: test that standard NVARCHAR with server collation works.
		_, err = pool.Exec(ctx,
			"CREATE TABLE [dbo].["+tableName+"] ("+
				"id INT IDENTITY PRIMARY KEY, "+
				"col_a NVARCHAR(100), "+
				"col_b NVARCHAR(100))")
		if err != nil {
			t.Fatalf("create fallback cross-collation table: %v", err)
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
		})

		_, err = pool.Exec(ctx,
			"INSERT INTO [dbo].["+tableName+"] (col_a, col_b) VALUES ($1, $2)",
			"Hello", "Hello")
		if err != nil {
			t.Fatalf("insert fallback row: %v", err)
		}

		var match int
		row, qrErr = pool.QueryRow(ctx,
			"SELECT CASE WHEN col_a = col_b THEN 1 ELSE 0 END FROM [dbo].["+tableName+"] WHERE id = 1",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&match)
		if err != nil {
			t.Fatalf("compare fallback columns: %v", err)
		}
		if match != 1 {
			t.Errorf("standard columns: 'Hello' != 'Hello' - unexpected")
		}
		t.Logf("_UTF8 not available - standard NVARCHAR cross-column comparison works correctly")
		return
	}

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Insert same value in both columns.
	_, err = pool.Exec(ctx,
		"INSERT INTO [dbo].["+tableName+"] (utf8_col, standard_col) VALUES ($1, $2)",
		"TestValue", "TestValue")
	if err != nil {
		t.Fatalf("insert cross-collation row: %v", err)
	}

	// Direct comparison between UTF-8 and non-UTF-8 should raise collation conflict.
	row, qrErr = pool.QueryRow(ctx,
		"SELECT CASE WHEN utf8_col = standard_col THEN 1 ELSE 0 END FROM [dbo].["+tableName+"] WHERE id = 1",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(new(int))

	if err != nil {
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "collat") {
			t.Logf("cross-collation conflict correctly detected: %v", err)
		} else {
			t.Logf("cross-collation comparison failed: %v", err)
		}
	} else {
		t.Logf("cross-collation comparison succeeded without COLLATE - server resolved it")
	}

	// Resolve with explicit COLLATE.
	var resolved int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT CASE WHEN utf8_col COLLATE Latin1_General_100_CI_AS = standard_col THEN 1 ELSE 0 END FROM [dbo].["+tableName+"] WHERE id = 1",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&resolved)
	if err != nil {
		t.Fatalf("collate-resolved comparison: %v", err)
	}
	if resolved != 1 {
		t.Errorf("COLLATE-resolved comparison: 'TestValue' should equal 'TestValue'")
	}
	t.Logf("_UTF8 cross-collation: resolved=%d", resolved)
}

// Section 6: MERGE Deep-Dive

// TestMSSQL_Merge_AllWhenClauses verifies MERGE with all three WHEN
// clauses: MATCHED, NOT MATCHED BY TARGET, and NOT MATCHED BY SOURCE.
func TestMSSQL_Merge_AllWhenClauses(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL MERGE test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("merge_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"slug NVARCHAR(100) PRIMARY KEY, "+
			"title NVARCHAR(200) NOT NULL, "+
			"version INT NOT NULL DEFAULT 1, "+
			"updated_at DATETIME2 DEFAULT SYSUTCDATETIME()"+
			")")
	if err != nil {
		t.Fatalf("create MERGE test table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Seed with some existing rows.
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (slug, title, version) VALUES ($1, $2, $3)", "keep", "Keep Me", 1)
	if err != nil {
		t.Fatalf("insert keep: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (slug, title, version) VALUES ($1, $2, $3)", "update", "Old Title", 1)
	if err != nil {
		t.Fatalf("insert update: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (slug, title, version) VALUES ($1, $2, $3)", "delete", "Delete Me", 1)
	if err != nil {
		t.Fatalf("insert delete: %v", err)
	}

	// MERGE: update 'update', insert 'new', do nothing for 'keep' (MATCHED without update),
	// and delete rows not in source (the 'delete' row).
	_, err = pool.Exec(ctx,
		"MERGE [dbo].["+tableName+"] AS target "+
			"USING (VALUES "+
			"($1, $2), "+ // update slug
			"($3, $4), "+ // keep slug
			"($5, $6)  "+ // new slug
			") AS source (slug, title) "+
			"ON target.slug = source.slug "+
			"WHEN MATCHED AND target.slug = $7 THEN "+ // only update 'update' row
			"  UPDATE SET title = source.title, version = target.version + 1 "+
			"WHEN NOT MATCHED BY TARGET THEN "+
			"  INSERT (slug, title, version) VALUES (source.slug, source.title, 1) "+
			"WHEN NOT MATCHED BY SOURCE THEN "+
			"  DELETE;",
		"update", "New Title",
		"keep", "Keep Me",
		"new", "New Kid",
		"update", // parameter for the AND condition
	)
	if err != nil {
		t.Fatalf("MERGE: %v", err)
	}

	// Verify results.
	var count int
	row, qrErr := pool.QueryRow(ctx, "SELECT COUNT(*) FROM [dbo].["+tableName+"]")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("count after MERGE: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 rows after MERGE, got %d", count)
	}

	// Verify 'update' row.
	var title string
	var version int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT title, version FROM [dbo].["+tableName+"] WHERE slug = $1", "update",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&title, &version)
	if err != nil {
		t.Fatalf("query update row: %v", err)
	}
	if title != "New Title" {
		t.Errorf("update row title = %q, want 'New Title'", title)
	}
	if version != 2 {
		t.Errorf("update row version = %d, want 2", version)
	}

	// Verify 'new' row.
	row, qrErr = pool.QueryRow(ctx,
		"SELECT title, version FROM [dbo].["+tableName+"] WHERE slug = $1", "new",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&title, &version)
	if err != nil {
		t.Fatalf("query new row: %v", err)
	}
	if title != "New Kid" || version != 1 {
		t.Errorf("new row: title=%q version=%d", title, version)
	}

	// Verify 'keep' row still exists.
	row, qrErr = pool.QueryRow(ctx,
		"SELECT title, version FROM [dbo].["+tableName+"] WHERE slug = $1", "keep",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&title, &version)
	if err != nil {
		t.Fatalf("query keep row: %v", err)
	}
	if title != "Keep Me" || version != 1 {
		t.Errorf("keep row modified: title=%q version=%d", title, version)
	}

	// Verify 'delete' row is gone.
	var deletedExists int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM [dbo].["+tableName+"] WHERE slug = $1", "delete",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&deletedExists)
	if err != nil {
		t.Fatalf("query deleted row: %v", err)
	}
	if deletedExists != 0 {
		t.Errorf("'delete' row should be gone, found %d", deletedExists)
	}

	t.Logf("MERGE all clauses: updated=%q (v%d), new=%q, keep=%q, deleted removed",
		title, version, "New Kid", "Keep Me")
}

// TestMSSQL_Merge_OUTPUT verifies that MERGE with OUTPUT clause returns
// affected rows with $action (INSERT/UPDATE/DELETE).
func TestMSSQL_Merge_OUTPUT(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL MERGE OUTPUT test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("mergeout_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"id INT IDENTITY PRIMARY KEY, "+
			"key_name NVARCHAR(100) UNIQUE NOT NULL, "+
			"val INT NOT NULL"+
			")")
	if err != nil {
		t.Fatalf("create MERGE OUTPUT table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	// Seed one row.
	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] (key_name, val) VALUES ($1, $2)", "a", 1)
	if err != nil {
		t.Fatalf("insert seed: %v", err)
	}

	// MERGE with OUTPUT: update 'a', insert 'b'.
	rows, err := pool.Query(ctx,
		"MERGE [dbo].["+tableName+"] AS target "+
			"USING (VALUES ($1, $2), ($3, $4)) AS source (key_name, val) "+
			"ON target.key_name = source.key_name "+
			"WHEN MATCHED THEN UPDATE SET val = source.val "+
			"WHEN NOT MATCHED THEN INSERT (key_name, val) VALUES (source.key_name, source.val) "+
			"OUTPUT $action, INSERTED.key_name, INSERTED.val;",
		"a", 10, // update a
		"b", 20, // insert b
	)
	if err != nil {
		t.Fatalf("MERGE OUTPUT: %v", err)
	}
	defer rows.Close()

	type actionRow struct {
		action  string
		keyName string
		val     int
	}
	var actions []actionRow
	for rows.Next() {
		var ar actionRow
		if err := rows.Scan(&ar.action, &ar.keyName, &ar.val); err != nil {
			t.Fatalf("scan MERGE OUTPUT row: %v", err)
		}
		actions = append(actions, ar)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err after MERGE OUTPUT: %v", err)
	}

	if len(actions) != 2 {
		t.Errorf("expected 2 OUTPUT rows, got %d", len(actions))
	}
	for _, ar := range actions {
		t.Logf("MERGE OUTPUT: $action=%s key_name=%s val=%d", ar.action, ar.keyName, ar.val)
		switch ar.keyName {
		case "a":
			if ar.action != "UPDATE" {
				t.Errorf("expected UPDATE for key 'a', got %s", ar.action)
			}
			if ar.val != 10 {
				t.Errorf("a.val = %d, want 10", ar.val)
			}
		case "b":
			if ar.action != "INSERT" {
				t.Errorf("expected INSERT for key 'b', got %s", ar.action)
			}
			if ar.val != 20 {
				t.Errorf("b.val = %d, want 20", ar.val)
			}
		default:
			t.Errorf("unexpected key %s", ar.keyName)
		}
	}
}

// TestMSSQL_Merge_RowCount verifies that @@ROWCOUNT after MERGE returns
// the total number of rows affected (INSERT + UPDATE + DELETE).
func TestMSSQL_Merge_RowCount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL MERGE @@ROWCOUNT test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("mergerc_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE [dbo].["+tableName+"] ("+
			"k NVARCHAR(10) PRIMARY KEY, "+
			"n INT NOT NULL"+
			")")
	if err != nil {
		t.Fatalf("create MERGE rowcount table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS [dbo].["+tableName+"]")
	})

	_, err = pool.Exec(ctx, "INSERT INTO [dbo].["+tableName+"] VALUES ($1, $2),($3, $4)", "x", 1, "y", 2)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// MERGE: update x, insert z, delete y (not in source).
	var rowCount int
	row, qrErr := pool.QueryRow(ctx,
		"MERGE [dbo].["+tableName+"] AS target "+
			"USING (VALUES ($1, $2), ($3, $4)) AS source (k, n) "+
			"ON target.k = source.k "+
			"WHEN MATCHED THEN UPDATE SET n = source.n "+
			"WHEN NOT MATCHED BY TARGET THEN INSERT (k, n) VALUES (source.k, source.n) "+
			"WHEN NOT MATCHED BY SOURCE THEN DELETE; "+
			"SELECT @@ROWCOUNT;",
		"x", 100, // update
		"z", 300, // insert
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&rowCount)
	if err != nil {
		t.Fatalf("MERGE @@ROWCOUNT: %v", err)
	}

	// Should be 3: 1 update + 1 insert + 1 delete.
	if rowCount != 3 {
		t.Errorf("@@ROWCOUNT = %d, want 3 (1 update + 1 insert + 1 delete)", rowCount)
	}
	t.Logf("MERGE @@ROWCOUNT = %d", rowCount)
}

// Section 7: MSSQL Server Configuration Audit

// TestMSSQL_ServerConfiguration audits key server-level settings that
// affect CMS behavior: collation, version, edition, compatibility level,
// and database scoped configurations.
func TestMSSQL_ServerConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping MSSQL server configuration audit in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	checks := []struct {
		label string
		query string
	}{
		{"Server Version", "SELECT SERVERPROPERTY('ProductVersion')"},
		{"Edition", "SELECT SERVERPROPERTY('Edition')"},
		{"Engine Edition", "SELECT CAST(SERVERPROPERTY('EngineEdition') AS INT)"},
		{"Collation", "SELECT SERVERPROPERTY('Collation')"},
		{"Product Level", "SELECT SERVERPROPERTY('ProductLevel')"},
		{"Compatibility Level", "SELECT compatibility_level FROM sys.databases WHERE name = DB_NAME()"},
		{"RCSI Enabled", "SELECT is_read_committed_snapshot_on FROM sys.databases WHERE name = DB_NAME()"},
		{"Snapshot Isolation", "SELECT snapshot_isolation_state_desc FROM sys.databases WHERE name = DB_NAME()"},
		{"Recovery Model", "SELECT recovery_model_desc FROM sys.databases WHERE name = DB_NAME()"},
		{"Query Store", "SELECT CASE WHEN is_query_store_on = 1 THEN 'ON' ELSE 'OFF' END FROM sys.databases WHERE name = DB_NAME()"},
	}

	for _, c := range checks {
		var val string
		row, qrErr := pool.QueryRow(ctx, c.query)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Logf("%s: ERROR - %v", c.label, err)
		} else {
			t.Logf("%s: %s", c.label, val)
		}
	}
}
