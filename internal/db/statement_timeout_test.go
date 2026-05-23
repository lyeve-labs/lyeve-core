package db

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestApplyStatementTimeout_Postgres(t *testing.T) {
	dsn := "postgres://localhost/mydb"
	got := applyStatementTimeout(dsn, "postgres", 30*time.Second)
	want := "postgres://localhost/mydb?statement_timeout=30000"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// already has params
	dsn2 := "postgres://localhost/mydb?sslmode=disable"
	got2 := applyStatementTimeout(dsn2, "postgres", 30*time.Second)
	want2 := dsn2 + "&statement_timeout=30000"
	if got2 != want2 {
		t.Errorf("got %q, want %q", got2, want2)
	}
	// custom timeout
	got3 := applyStatementTimeout(dsn, "postgres", 5*time.Second)
	want3 := "postgres://localhost/mydb?statement_timeout=5000"
	if got3 != want3 {
		t.Errorf("got %q, want %q", got3, want3)
	}
}

func TestApplyStatementTimeout_MySQL(t *testing.T) {
	dsn := "root:secret@tcp(localhost:3306)/mydb"
	got := applyStatementTimeout(dsn, "mysql", 30*time.Second)
	want := dsn + "?max_execution_time=30000"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyStatementTimeout_MSSQL(t *testing.T) {
	// MSSQL timeout is connector-based, not DSN-based: applyStatementTimeout is no-op
	dsn := "sqlserver://localhost/mydb"
	got := applyStatementTimeout(dsn, "mssql", 30*time.Second)
	if got != dsn {
		t.Errorf("MSSQL should be passthrough; got %q", got)
	}
}

func TestApplyStatementTimeout_Zero(t *testing.T) {
	dsn := "postgres://localhost/mydb"
	got := applyStatementTimeout(dsn, "postgres", 0)
	if got != dsn {
		t.Errorf("zero timeout should be no-op; got %q", got)
	}
}

func TestAppendDSNParam(t *testing.T) {
	if got := appendDSNParam("host/db", "a=1"); got != "host/db?a=1" {
		t.Errorf("first param: got %q", got)
	}
	if got := appendDSNParam("host/db?a=1", "b=2"); got != "host/db?a=1&b=2" {
		t.Errorf("subsequent param: got %q", got)
	}
}

// --- MSSQL lock_timeout connector tests ---

// mockConnector is a minimal driver.Connector for unit testing.
type mockConnector struct {
	conn  driver.Conn
	drv   driver.Driver
	err   error
	calls int
}

func (m *mockConnector) Connect(context.Context) (driver.Conn, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.conn, nil
}

func (m *mockConnector) Driver() driver.Driver { return m.drv }

// mockConn implements driver.Conn + driver.ExecerContext.
type mockConn struct {
	execSQL []string
	closed  bool
}

func (m *mockConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("not impl") }
func (m *mockConn) Close() error                        { m.closed = true; return nil }
func (m *mockConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("not impl") }
func (m *mockConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	m.execSQL = append(m.execSQL, query)
	return driver.RowsAffected(0), nil
}

// mockDriver implements driver.Driver.
type mockDriver struct{}

func (d *mockDriver) Open(string) (driver.Conn, error) { return nil, fmt.Errorf("not impl") }

func TestMSSQLLockTimeoutConnector_ExecutesSET(t *testing.T) {
	conn := &mockConn{}
	parent := &mockConnector{
		conn: conn,
		drv:  &mockDriver{},
	}
	wrapped := &mssqlLockTimeoutConnector{
		parent:  parent,
		timeout: 30 * time.Second,
	}

	got, err := wrapped.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect() error: %v", err)
	}
	if got != conn {
		t.Error("Connect() should return underlying conn")
	}
	if parent.calls != 1 {
		t.Errorf("parent.Connect called %d times, want 1", parent.calls)
	}
	if len(conn.execSQL) != 1 || conn.execSQL[0] != "SET LOCK_TIMEOUT 30000" {
		t.Errorf("expected SET LOCK_TIMEOUT 30000, got %v", conn.execSQL)
	}
}

func TestMSSQLLockTimeoutConnector_ParentConnectError(t *testing.T) {
	parent := &mockConnector{
		err: fmt.Errorf("connection refused"),
		drv: &mockDriver{},
	}
	wrapped := &mssqlLockTimeoutConnector{
		parent:  parent,
		timeout: 30 * time.Second,
	}

	_, err := wrapped.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error from parent Connect()")
	}
	if err.Error() != "connection refused" {
		t.Errorf("expected wrapped parent error, got: %v", err)
	}
}

func TestMSSQLLockTimeoutConnector_ExecFailure(t *testing.T) {
	failingConn := &failingExecConn{}
	parent := &mockConnector{
		conn: failingConn,
		drv:  &mockDriver{},
	}
	wrapped := &mssqlLockTimeoutConnector{
		parent:  parent,
		timeout: 30 * time.Second,
	}

	_, err := wrapped.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error when SET LOCK_TIMEOUT fails")
	}
	if !strings.Contains(err.Error(), "set mssql lock_timeout") {
		t.Errorf("expected 'set mssql lock_timeout' in error, got: %v", err)
	}
	if !failingConn.closed {
		t.Error("underlying conn should be closed on error")
	}
}

func TestMSSQLLockTimeoutConnector_Driver(t *testing.T) {
	d := &mockDriver{}
	parent := &mockConnector{drv: d}
	wrapped := &mssqlLockTimeoutConnector{parent: parent, timeout: 30 * time.Second}

	if wrapped.Driver() != d {
		t.Error("Driver() should delegate to parent")
	}
}

// failingExecConn implements driver.Conn + driver.ExecerContext, always errors on Exec.
type failingExecConn struct {
	closed bool
}

func (f *failingExecConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("nope") }
func (f *failingExecConn) Close() error                        { f.closed = true; return nil }
func (f *failingExecConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("nope") }
func (f *failingExecConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, fmt.Errorf("execution denied")
}
