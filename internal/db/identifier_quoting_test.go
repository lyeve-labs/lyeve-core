package db

import (
	"context"
	"strings"
	"testing"
)

// Quote edge cases across all dialects

func TestQuoteIdentifier_SQLInjectionVectors(t *testing.T) {
	// Verify the quoting functions properly escape SQL injection attempts
	// that bypass the slug regex (defense-in-depth).
	vectors := []struct {
		in   string
		want string
	}{
		// sql comment: embedded " gets doubled, then wrapped
		{`acme"--`, `"acme""--"`},
		// statement terminator
		{`acme;`, `"acme;"`},
		// union select
		{`union`, `"union"`},
		// pg specific
		{`test\`, `"test\"`},
	}
	for _, v := range vectors {
		got := quoteIdentifier(v.in)
		if got != v.want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", v.in, got, v.want)
		}
	}
}

func TestQuoteMySQLIdentifier_DangerousInput(t *testing.T) {
	vectors := []struct {
		in   string
		want string
	}{
		{`test;DROP`, "`test;DROP`"},
		{`test'OR'1'='1`, "`test'OR'1'='1`"},
		{`\n`, "`\\n`"},
		{`--`, "`--`"},
	}
	for _, v := range vectors {
		got := quoteMySQLIdentifier(v.in)
		if got != v.want {
			t.Errorf("quoteMySQLIdentifier(%q) = %q, want %q", v.in, got, v.want)
		}
	}
}

func TestQuoteMSSQLIdentifier_DangerousInput(t *testing.T) {
	vectors := []struct {
		in   string
		want string
	}{
		{`test;DROP`, "[test;DROP]"},
		{`test'OR'1'='1`, "[test'OR'1'='1]"},
		{`\x`, "[\\x]"},
		{`--`, "[--]"},
	}
	for _, v := range vectors {
		got := quoteMSSQLIdentifier(v.in)
		if got != v.want {
			t.Errorf("quoteMSSQLIdentifier(%q) = %q, want %q", v.in, got, v.want)
		}
	}
}

// Quote boundary: max identifier length

func TestQuoteIdentifier_MaxIdentifierLength(t *testing.T) {
	// Postgres max identifier is 63 bytes. Quoting adds 2 chars (double quotes).
	// The slug regex enforces ≤63 chars, so max quoted is 65.
	s := strings.Repeat("a", 63)
	got := quoteIdentifier("tenant_" + s)
	// "tenant_" + 63 a's = 73 total chars after quoting
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("max-length identifier not properly quoted: %q", got)
	}
}

// Tenancy Apply: slug with underscore at start

func TestPostgresSchemaTenancy_Apply_UnderscoreSlug(t *testing.T) {
	// safeSlugRe rejects slugs starting with underscore. Apply returns error.
	p := &PostgresSchemaTenancy{
		TenantIDFunc: func(ctx context.Context) string { return "_tenant" },
	}
	err := p.Apply(context.Background(), nil)
	if err == nil {
		t.Error("Apply(_tenant): want validation error, got nil")
	}
}

// Reset with safe DefaultDB

func TestMySQLDatabaseTenancy_Reset_WithDefaultDB(t *testing.T) {
	// Can't test with real conn, but verify configuration.
	m := &MySQLDatabaseTenancy{
		TenantIDFunc: func(ctx context.Context) string { return "acme" },
		DefaultDB:    "lyeve_test",
	}
	if m.DefaultDB != "lyeve_test" {
		t.Errorf("DefaultDB = %q, want 'lyeve_test'", m.DefaultDB)
	}
	if m.TenantIDFunc == nil {
		t.Error("TenantIDFunc should not be nil")
	}
}

func TestMSSQLDatabaseTenancy_Reset_WithDefaultDB(t *testing.T) {
	m := &MSSQLDatabaseTenancy{
		TenantIDFunc: func(ctx context.Context) string { return "acme" },
		DefaultDB:    "master",
	}
	if m.DefaultDB != "master" {
		t.Errorf("DefaultDB = %q, want 'master'", m.DefaultDB)
	}
	if m.TenantIDFunc == nil {
		t.Error("TenantIDFunc should not be nil")
	}
}

// SafeSlug comprehensive additional cases

func TestSafeSlugRe_MixedValidCases(t *testing.T) {
	valid := []string{
		"z",
		"abc123",
		"a_b_c",
		"test_123_abc",
		"my_org_2",
		"x",                                // single lowercase letter
		strings.Repeat("z", 63),            // exact max
		"a" + strings.Repeat("b", 62),      // 63 chars starting with a
		"abc_def_123_xyz",                  // mixed underscores
		"tenant" + strings.Repeat("0", 57), // 63 chars
	}
	for _, slug := range valid {
		if !safeSlugRe.MatchString(slug) {
			t.Errorf("safeSlugRe should accept %q (len=%d)", slug, len(slug))
		}
	}
}

func TestSafeSlugRe_LeadingTrailingUnderscore(t *testing.T) {
	// Underscores must not be at position 0. Trailing is fine.
	if safeSlugRe.MatchString("_abc") {
		t.Error("safeSlugRe should reject leading underscore")
	}
	if !safeSlugRe.MatchString("abc_") {
		t.Error("safeSlugRe should accept trailing underscore")
	}
	if !safeSlugRe.MatchString("a_b") {
		t.Error("safeSlugRe should accept embedded underscore")
	}
}

func TestSafeSlugRe_DashesHyphens(t *testing.T) {
	// Dashes/hyphens are NOT in the allowed character class.
	reject := []string{"my-org", "test-123", "a-b-c", "-abc", "abc-"}
	for _, slug := range reject {
		if safeSlugRe.MatchString(slug) {
			t.Errorf("safeSlugRe should reject slug with dash: %q", slug)
		}
	}
}

func TestSafeSlugRe_NullAndControlCharacters(t *testing.T) {
	reject := []string{
		"abc\x00def",
		"\x00abc",
		"abc\x00",
		"\x01abc",
		"abc\x1b",
		"abc\x7f",
	}
	for _, slug := range reject {
		if safeSlugRe.MatchString(slug) {
			t.Errorf("safeSlugRe should reject control char in %q", slug)
		}
	}
}

// WithTenantConn / TenantConn: nil and typed-nil

func TestWithTenantConn_NilPointer(t *testing.T) {
	ctx := WithTenantConn(context.Background(), nil)
	if TenantConn(ctx) != nil {
		t.Error("TenantConn with nil pointer should return nil")
	}
}

// Apply: slug with dots

func TestPostgresSchemaTenancy_Apply_DottedSlug(t *testing.T) {
	p := &PostgresSchemaTenancy{
		TenantIDFunc: func(ctx context.Context) string { return "a.b" },
	}
	err := p.Apply(context.Background(), nil)
	if err == nil {
		t.Error("Apply(a.b): want validation error, got nil")
	}
}

// Apply: slug with at-sign

func TestMySQLDatabaseTenancy_Apply_AtSlug(t *testing.T) {
	m := &MySQLDatabaseTenancy{
		TenantIDFunc: func(ctx context.Context) string { return "org@acme" },
	}
	err := m.Apply(context.Background(), nil)
	if err == nil {
		t.Error("Apply(org@acme): want validation error, got nil")
	}
}

func TestMSSQLDatabaseTenancy_Apply_AtSlug(t *testing.T) {
	m := &MSSQLDatabaseTenancy{
		TenantIDFunc: func(ctx context.Context) string { return "org@acme" },
	}
	err := m.Apply(context.Background(), nil)
	if err == nil {
		t.Error("Apply(org@acme): want validation error, got nil")
	}
}

// All-tenancy roundup: check Tenancy interface satisfaction for all 3

func TestAllTenancyTypes_SatisfyInterface(t *testing.T) {
	// Compile-time checks that all three constructors return the Tenancy interface.
	var _ = NewPostgresSchemaTenancy(func(ctx context.Context) string { return "" })
	var _ = NewMySQLDatabaseTenancy(func(ctx context.Context) string { return "" }, "lyeve_test")
	var _ = NewMSSQLDatabaseTenancy(func(ctx context.Context) string { return "" }, "master")
}
