package mockhost

import (
	"context"
	"fmt"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// MockConfig

func TestMockConfig_String(t *testing.T) {
	cfg := NewMockConfig()
	cfg.Set("jwt_secret", "super-secret")

	if got := cfg.String("jwt_secret"); got != "super-secret" {
		t.Errorf("expected 'super-secret', got %q", got)
	}
	if got := cfg.String("unknown"); got != "" {
		t.Errorf("expected empty for unknown key, got %q", got)
	}
}

func TestMockConfig_Bool(t *testing.T) {
	cfg := NewMockConfig()
	cfg.Set("secure_cookie", "true")
	cfg.Set("debug", "1")
	cfg.Set("enabled", "false")

	if !cfg.Bool("secure_cookie") {
		t.Error("expected secure_cookie=true")
	}
	if !cfg.Bool("debug") {
		t.Error("expected debug=true (via '1')")
	}
	if cfg.Bool("enabled") {
		t.Error("expected enabled=false")
	}
	if cfg.Bool("unknown") {
		t.Error("expected unknown=false")
	}
}

func TestMockConfig_Duration(t *testing.T) {
	cfg := NewMockConfig()
	if got := cfg.Duration("anything"); got != 0 {
		t.Errorf("expected 0, got %v", got)
	}
}

func TestMockConfig_Strings(t *testing.T) {
	cfg := NewMockConfig()

	// Explicit slice
	cfg.SetStrings("cors_origins", []string{"https://a.com", "https://b.com"})
	got := cfg.Strings("cors_origins")
	if len(got) != 2 || got[0] != "https://a.com" {
		t.Errorf("unexpected Strings: %v", got)
	}

	// Fallback from comma-separated String value
	cfg.Set("jwt_secrets", "s1,s2,s3")
	got = cfg.Strings("jwt_secrets")
	if len(got) != 3 || got[2] != "s3" {
		t.Errorf("unexpected comma-split: %v", got)
	}

	// Unknown
	if got := cfg.Strings("unknown"); got != nil {
		t.Errorf("expected nil for unknown, got %v", got)
	}
}

// QuerierSpy

func TestQuerierSpy_QueryRow(t *testing.T) {
	mock := New(t)
	spy := mock.Spy()
	spy.OnQueryRow("SELECT id FROM users WHERE email = $1").WithScan("user-1")

	row, _ := mock.Host().Querier(context.Background()).QueryRow(context.Background(),
		"SELECT id FROM users WHERE email = $1", "a@b.com")
	if row == nil {
		t.Fatal("expected non-nil row")
	}

	spy.AssertCalled(t, "QueryRow", "SELECT id FROM users WHERE email = $1")
}

func TestQuerierSpy_Exec(t *testing.T) {
	mock := New(t)
	spy := mock.Spy()
	spy.OnExec("UPDATE users SET name = $1 WHERE id = $2").ReturnTag(ExecResult(1))

	tag, err := mock.Host().Querier(context.Background()).Exec(context.Background(),
		"UPDATE users SET name = $1 WHERE id = $2", "Alice", "user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag.RowsAffected != 1 {
		t.Errorf("expected 1 row affected, got %d", tag.RowsAffected)
	}
	spy.AssertCalled(t, "Exec", "UPDATE users SET name = $1 WHERE id = $2")
}

func TestQuerierSpy_Query(t *testing.T) {
	mock := New(t)
	spy := mock.Spy()
	spy.OnQuery("SELECT * FROM bookmarks").ReturnRows(
		Rows("id", "title").Add("abc", "Hello").Add("def", "World"),
	)

	rows, err := mock.Host().Querier(context.Background()).Query(context.Background(),
		"SELECT * FROM bookmarks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var count int
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			t.Fatalf("scan: %v", err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
}

func TestQuerierSpy_AssertNotCalled(t *testing.T) {
	mock := New(t)
	spy := mock.Spy()

	// Should not panic: nothing called yet.
	spy.AssertNotCalled(t, "Exec", "DROP TABLE users")
}

func TestQuerierSpy_CallCount(t *testing.T) {
	mock := New(t)
	spy := mock.Spy()

	q := mock.Host().Querier(context.Background())
	q.Exec(context.Background(), "INSERT INTO t VALUES ($1)", 1)
	q.Exec(context.Background(), "INSERT INTO t VALUES ($1)", 2)
	q.QueryRow(context.Background(), "SELECT COUNT(*) FROM t")

	if n := spy.CallCount("Exec"); n != 2 {
		t.Errorf("expected 2 Exec calls, got %d", n)
	}
	if n := spy.CallCount("QueryRow"); n != 1 {
		t.Errorf("expected 1 QueryRow call, got %d", n)
	}
	if n := spy.CallCount("Query"); n != 0 {
		t.Errorf("expected 0 Query calls, got %d", n)
	}
}

func TestQuerierSpy_Reset(t *testing.T) {
	mock := New(t)
	spy := mock.Spy()
	spy.OnQueryRow("SELECT 1")
	mock.Host().Querier(context.Background()).QueryRow(context.Background(), "SELECT 1")

	spy.Reset()
	if spy.CallCount("QueryRow") != 0 {
		t.Error("expected 0 calls after Reset")
	}
	if len(spy.ExpectRow) != 0 {
		t.Error("expected 0 expectations after Reset")
	}
}

// MockRows

func TestMockRows_Empty(t *testing.T) {
	r := Rows("id", "name")
	if r.Next() {
		t.Error("empty rows should return false on first Next")
	}
}

func TestMockRows_Scan(t *testing.T) {
	r := Rows("id", "name").Add("id-1", "Alice").Add("id-2", "Bob")

	if !r.Next() {
		t.Fatal("expected first row")
	}
	var id, name string
	if err := r.Scan(&id, &name); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if id != "id-1" || name != "Alice" {
		t.Errorf("unexpected first row: id=%q name=%q", id, name)
	}

	if !r.Next() {
		t.Fatal("expected second row")
	}
	if err := r.Scan(&id, &name); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if id != "id-2" || name != "Bob" {
		t.Errorf("unexpected second row: id=%q name=%q", id, name)
	}

	if r.Next() {
		t.Error("expected no more rows")
	}
}

func TestMockRows_WithError(t *testing.T) {
	r := Rows("id").Add("a").WithError(fmt.Errorf("db error"))

	// Err() returns the error regardless of iteration position.
	// (sql.Rows behavior: Err() is checked after Next returns false)
	if r.Err() == nil {
		t.Error("expected error from WithError")
	}
}

// MockRow

func TestMockRow_WithScan(t *testing.T) {
	row := Row().WithScan("hello", 42)
	// Just verify it doesn't panic.
	_ = row
}

// hookSpy

func TestHookSpy_AssertSubscribed(t *testing.T) {
	mock := New(t)
	host := mock.Host()

	// Plugin-like subscription
	host.Hooks().Subscribe("bookmarks", core.AfterCreate, func(ctx context.Context, e core.Event) error {
		return nil
	})

	mock.Hooks().AssertSubscribed(t, core.AfterCreate, "bookmarks")
}

func TestHookSpy_AssertPublished(t *testing.T) {
	mock := New(t)
	host := mock.Host()

	err := host.HookPublisher().Publish(context.Background(), core.Event{
		Type:   core.BeforeDelete,
		Schema: "bookmarks",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	mock.Hooks().AssertPublished(t, core.BeforeDelete, "bookmarks")
}

// mockHostAdapter completeness

func TestMockHost_AllMethods(t *testing.T) {
	mock := New(t)
	host := mock.Host()

	// Verify all Host methods are callable (no panics).
	if host.Querier(context.Background()) == nil {
		t.Error("Querier returned nil")
	}
	if host.QuerierRO(context.Background()) == nil {
		t.Error("QuerierRO returned nil")
	}
	if host.Config() == nil {
		t.Error("Config returned nil")
	}
	if host.Hooks() == nil {
		t.Error("Hooks returned nil")
	}
	if v := host.Version(); v != "mock" {
		t.Errorf("Version: got %q, want 'mock'", v)
	}
	if d := host.Dialect(); d != "postgres" {
		t.Errorf("Dialect: got %q, want 'postgres'", d)
	}
	if host.RawDB() != nil {
		t.Error("RawDB should return nil for mockhost")
	}
	if host.Schema() != nil {
		t.Error("Schema should return nil for mockhost")
	}
	if host.Tracer("test") == nil {
		t.Error("Tracer should return non-nil noop tracer")
	}
	if host.Logger(context.Background()) == nil {
		t.Error("Logger returned nil")
	}
}
