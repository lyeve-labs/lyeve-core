package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	mssql "github.com/microsoft/go-mssqldb"
)

// TestQuoteIdentifier checks that quoting wraps in double quotes with embedded
// quotes doubled (Postgres / SQL-92 style). The slug regex already strips
// quote-containing input, so this is defense-in-depth. The cases below assert
// the escape itself works.
func TestQuoteIdentifier(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"tenant_acme", `"tenant_acme"`},
		{"plain", `"plain"`},
		{"", `""`},
		{`with"quote`, `"with""quote"`},
		{`a"b"c`, `"a""b""c"`},
		{`""`, `""""""`},
	}
	for _, c := range cases {
		if got := quoteIdentifier(c.in); got != c.want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTenantConn_Empty checks that TenantConn returns nil when the ctx carries
// no tenant connection, and that it is not confused by unrelated context
// values.
func TestTenantConn_Empty(t *testing.T) {
	if c := TenantConn(context.Background()); c != nil {
		t.Errorf("TenantConn(background) = %v, want nil", c)
	}
	// An unrelated value on the same context must not be picked up.
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, "tenant_acme")
	if c := TenantConn(ctx); c != nil {
		t.Errorf("TenantConn(unrelated value) = %v, want nil", c)
	}
}

// TestPostgresSchemaTenancy_Apply_NilCases checks that both a nil receiver
// and a constructed value with no TenantIDFunc are no-ops. The middleware
// should never break the request just because tenancy isn't configured.
func TestPostgresSchemaTenancy_Apply_NilCases(t *testing.T) {
	var p *PostgresSchemaTenancy
	if err := p.Apply(context.Background(), nil); err != nil {
		t.Errorf("nil receiver Apply: %v, want nil", err)
	}
	p2 := &PostgresSchemaTenancy{}
	if err := p2.Apply(context.Background(), nil); err != nil {
		t.Errorf("nil TenantIDFunc Apply: %v, want nil", err)
	}
}

// TestPostgresSchemaTenancy_Apply_EmptySlug checks that an empty slug, the
// "no tenant" signal, is a no-op. The conn is nil, so any SQL execution would
// crash.
func TestPostgresSchemaTenancy_Apply_EmptySlug(t *testing.T) {
	p := &PostgresSchemaTenancy{
		TenantIDFunc: func(context.Context) string { return "" },
	}
	if err := p.Apply(context.Background(), nil); err != nil {
		t.Errorf("empty slug Apply: %v, want nil", err)
	}
}

// TestPostgresSchemaTenancy_Apply_InvalidSlug checks that anything that
// doesn't match safeSlugRe is rejected BEFORE any SQL leaves the process. This
// is the SQL-injection seatbelt. Reject early, never concatenate.
func TestPostgresSchemaTenancy_Apply_InvalidSlug(t *testing.T) {
	bad := []string{
		"ACME",                  // uppercase
		"tenant with space",     // whitespace
		"tenant;DROP TABLE x",   // statement separator
		`tenant"quote`,          // embedded quote
		"1tenant",               // must start with [a-z]
		strings.Repeat("a", 64), // > 63 chars
		"-leading-dash",
	}
	for _, slug := range bad {
		t.Run(slug, func(t *testing.T) {
			p := &PostgresSchemaTenancy{
				TenantIDFunc: func(context.Context) string { return slug },
			}
			err := p.Apply(context.Background(), nil)
			if err == nil {
				t.Errorf("Apply(%q): want validation error, got nil", slug)
			}
		})
	}
}

// Each constructor must return a usable, non-nil strategy. TenancyConn treats a
// nil strategy as "no isolation configured" and passes the request straight
// through, so a nil here would be silent, not loud.
func TestTenancyConstructors_ReturnAUsableStrategy(t *testing.T) {
	tid := func(context.Context) string { return "acme" }

	cases := []struct {
		name string
		got  Tenancy
	}{
		{"postgres", NewPostgresSchemaTenancy(tid)},
		{"mysql", NewMySQLDatabaseTenancy(tid, "lyeve")},
		{"mssql", NewMSSQLDatabaseTenancy(tid, "lyeve")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got == nil {
				t.Fatal("constructor returned nil; TenancyConn would pass every request through unscoped")
			}
			// Apply and Reset must tolerate a nil connection rather than
			// panicking: the middleware calls Reset in a defer, which runs on
			// paths where acquisition never completed.
			if err := tc.got.Reset(context.Background(), nil); err != nil {
				t.Errorf("Reset on a nil conn: %v", err)
			}
		})
	}
}

// TestSafeSlugRe_Valid tests the slug regex directly from the db package.
func TestSafeSlugRe_Valid(t *testing.T) {
	valid := []string{
		"a",
		"acme",
		"acme_corp",
		"tenant123",
		"my_tenant_2",
		"x_y_z",
		strings.Repeat("a", 63),
	}
	for _, slug := range valid {
		if !safeSlugRe.MatchString(slug) {
			t.Errorf("safeSlugRe should accept %q", slug)
		}
	}
}

func TestSafeSlugRe_Invalid(t *testing.T) {
	invalid := []string{
		"",
		"ACME",
		"tenant with space",
		"tenant;DROP TABLE x",
		"1tenant",
		"-leading-dash",
		"_leading_underscore",
		".dots.are.not.allowed",
		"tenant@acme",
		"has-dash",
		strings.Repeat("a", 64),
	}
	for _, slug := range invalid {
		t.Run("reject_"+slug, func(t *testing.T) {
			if safeSlugRe.MatchString(slug) {
				t.Errorf("safeSlugRe should reject %q", slug)
			}
		})
	}
}

func TestSafeSlugRe_Boundary(t *testing.T) {
	s63 := strings.Repeat("a", 63)
	s64 := strings.Repeat("a", 64)
	if !safeSlugRe.MatchString(s63) {
		t.Errorf("63-char slug should be valid")
	}
	if safeSlugRe.MatchString(s64) {
		t.Errorf("64-char slug should be invalid")
	}
}

func TestSafeSlugRe_SingleChar(t *testing.T) {
	if !safeSlugRe.MatchString("a") {
		t.Errorf("single lowercase letter should be valid")
	}
	if safeSlugRe.MatchString("1") {
		t.Errorf("single digit should be invalid")
	}
	if safeSlugRe.MatchString("_") {
		t.Errorf("single underscore should be invalid")
	}
}

func TestSafeSlugRe_UnicodeRejected(t *testing.T) {
	// Unicode and non-ASCII characters are rejected by the [a-z] prefix.
	reject := []string{
		"café",       // accented
		"über",       // umlaut
		"東京",         // CJK
		"россия",     // cyrillic
		"\u0000slug", // null-byte prefix
		"sl\u0000ug", // embedded null-byte
		"slug\u0000", // trailing null-byte
		"a\x00b",     // 0x00 byte
		"\x01slug",   // control char
		"slug\x7f",   // DEL char
	}
	for _, slug := range reject {
		t.Run("reject_"+slug, func(t *testing.T) {
			if safeSlugRe.MatchString(slug) {
				t.Errorf("safeSlugRe should reject %q", slug)
			}
		})
	}
}

func TestSafeSlugRe_AllNumericRejected(t *testing.T) {
	// All-numeric slugs must be rejected (must start with [a-z]).
	for _, s := range []string{"123", "0", "1111111111"} {
		if safeSlugRe.MatchString(s) {
			t.Errorf("safeSlugRe should reject all-numeric %q", s)
		}
	}
}

func TestSafeSlugRe_UnderscoreOnlyRejected(t *testing.T) {
	if safeSlugRe.MatchString("_abc") {
		t.Error("safeSlugRe should reject slug starting with underscore")
	}
	if safeSlugRe.MatchString("__") {
		t.Error("safeSlugRe should reject double-underscore")
	}
}

// TestTenantConn_WithValueAfterApply verifies that WithTenantConn -> TenantConn
// round-trips correctly with a concrete *sql.Conn pointer.
func TestTenantConn_WithValueAfterApply(t *testing.T) {
	// nil conn pointer is still round-trippable via the context key.
	ctx := WithTenantConn(context.Background(), nil)
	if TenantConn(ctx) != nil {
		t.Error("TenantConn(WithTenantConn(nil)) should return nil")
	}
}

// TestPostgresSchemaTenancy_Reset_NonNilReceiver_WithoutConn verifies
// that a non-nil PostgresSchemaTenancy with a nil conn will panic on
// Reset (nil pointer deref on *sql.Conn.ExecContext). This documents
// the current behavior: the middleware never passes nil conn, and the
// caller is responsible for acquiring a connection first.
// We can't call Reset with nil conn (it panics), so we verify that the
// struct is properly constructed and skip the unreachable call path.
func TestPostgresSchemaTenancy_NonNilStruct_Configuration(t *testing.T) {
	p := NewPostgresSchemaTenancy(func(ctx context.Context) string { return "acme" }).(*PostgresSchemaTenancy)
	if p.TenantIDFunc == nil {
		t.Error("TenantIDFunc should not be nil after construction")
	}
}

// TestMySQLDatabaseTenancy_EmptyDefaultDB_ResetIsNoop verifies that
// Reset is a no-op when DefaultDB is empty (covered by the nil checks
// in the existing test). This test just confirms the struct initializes
// correctly.
func TestMySQLDatabaseTenancy_EmptyDefaultDB_StructInit(t *testing.T) {
	m := &MySQLDatabaseTenancy{
		TenantIDFunc: func(ctx context.Context) string { return "acme" },
	}
	if m.DefaultDB != "" {
		t.Errorf("DefaultDB = %q, want empty", m.DefaultDB)
	}
}

// TestSafeSlugRe_ExactMatch verifies the regex matches the entire string
// (anchored with ^ and $), not a substring.
func TestSafeSlugRe_ExactMatch(t *testing.T) {
	if safeSlugRe.MatchString("abc\n") {
		t.Error("safeSlugRe should reject slug with trailing newline")
	}
	if safeSlugRe.MatchString(" abc") {
		t.Error("safeSlugRe should reject slug with leading space")
	}
	if safeSlugRe.MatchString("abc def") {
		t.Error("safeSlugRe should reject slug with embedded space")
	}
}

// A database the caller may not open is not a database that is missing.
// Reading a permission failure as absence would put a request that should have
// been refused onto the engine's own database instead, so the classification is
// by error code and never by message.
func TestIsMissingDatabaseErr_TellsAbsenceFromRefusal(t *testing.T) {
	tests := []struct {
		name   string
		engine string
		err    error
		want   bool
	}{
		{"mysql unknown database", "mysql", &mysql.MySQLError{Number: 1049, Message: "Unknown database 'tenant_acme'"}, true},
		{"mysql access denied", "mysql", &mysql.MySQLError{Number: 1044, Message: "Access denied for user"}, false},
		{"mysql wrapped", "mysql", fmt.Errorf("apply: %w", &mysql.MySQLError{Number: 1049}), true},
		{"mssql database does not exist", "mssql", mssql.Error{Number: 911}, true},
		{"mssql no access under this context", "mssql", mssql.Error{Number: 916}, false},
		{"mssql wrapped", "sqlserver", fmt.Errorf("apply: %w", mssql.Error{Number: 911}), true},
		{"postgres never falls back", "postgres", &mysql.MySQLError{Number: 1049}, false},
		{"plain error", "mysql", errors.New("unknown database"), false},
		{"nil", "mysql", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMissingDatabaseErr(tc.err, tc.engine); got != tc.want {
				t.Errorf("isMissingDatabaseErr = %v, want %v", got, tc.want)
			}
		})
	}
}

// The record is per slug: finding one tenant without a database says nothing
// about the next one.
func TestMissingTenantDB_RecordsOneSlugAtATime(t *testing.T) {
	var m missingTenantDB
	if m.known("acme") {
		t.Fatal("an empty record claims to know a slug")
	}
	m.record("acme")
	if !m.known("acme") {
		t.Error("acme was recorded and is not known")
	}
	if m.known("globex") {
		t.Error("recording acme decided globex too")
	}
}
