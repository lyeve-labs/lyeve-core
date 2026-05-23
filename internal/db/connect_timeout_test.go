package db

import (
	"testing"
	"time"
)

func TestApplyConnectTimeout_Postgres(t *testing.T) {
	dsn := "postgres://localhost/mydb"
	got := applyConnectTimeout(dsn, "postgres", 10*time.Second)
	want := "postgres://localhost/mydb?connect_timeout=10"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyConnectTimeout_MySQL(t *testing.T) {
	dsn := "user:***@tcp(localhost:3306)/mydb"
	got := applyConnectTimeout(dsn, "mysql", 10*time.Second)
	want := dsn + "?timeout=10s"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyConnectTimeout_MSSQL(t *testing.T) {
	dsn := "sqlserver://localhost/mydb"
	got := applyConnectTimeout(dsn, "mssql", 10*time.Second)
	want := "sqlserver://localhost/mydb?dial+timeout=10000"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyConnectTimeout_AppendToExistingParams(t *testing.T) {
	dsn := "postgres://localhost/mydb?sslmode=disable"
	got := applyConnectTimeout(dsn, "postgres", 10*time.Second)
	want := dsn + "&connect_timeout=10"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplyConnectTimeout_Zero(t *testing.T) {
	dsn := "postgres://localhost/mydb"
	got := applyConnectTimeout(dsn, "postgres", 0)
	if got != dsn {
		t.Errorf("zero timeout should be no-op; got %q", got)
	}
}

func TestApplyConnectTimeout_Negative(t *testing.T) {
	dsn := "postgres://localhost/mydb"
	got := applyConnectTimeout(dsn, "postgres", -1*time.Second)
	if got != dsn {
		t.Errorf("negative timeout should be no-op; got %q", got)
	}
}
