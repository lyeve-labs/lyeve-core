package db

import (
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

func TestMSSQLConnector_SetsLockTimeoutOnEverySession(t *testing.T) {
	c, err := mssqlConnector("sqlserver://sa:pw@localhost:1433?database=lyeve", 30*time.Second)
	if err != nil {
		t.Fatalf("mssqlConnector: %v", err)
	}
	if c.SessionInitSQL != "SET LOCK_TIMEOUT 30000" {
		t.Errorf("SessionInitSQL = %q, want SET LOCK_TIMEOUT 30000", c.SessionInitSQL)
	}
}
