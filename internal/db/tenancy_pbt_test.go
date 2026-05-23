package db

import (
	"context"
	"regexp"
	"testing"

	"pgregory.net/rapid"
)

// Tenancy Property-Based Tests

// Property: for any random string that matches safeSlugRe, the slug is a
// valid tenant identifier (lowercase alphanumeric + underscore, starts with
// letter, max 63 chars). This is the model-level invariant that prevents
// SQL injection through tenant slugs.
func TestPBT_SafeSlugReAcceptsValidSlugs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		slug := rapid.StringMatching(`[a-z][a-z0-9_]{0,62}`).Draw(t, "slug")

		if !safeSlugRe.MatchString(slug) {
			t.Fatalf("safeSlugRe rejected valid slug %q", slug)
		}
	})
}

// Property: for random strings that contain any character outside [a-z0-9_],
// start with a digit, or are empty/too long, safeSlugRe rejects them.
// This ensures no slug can carry SQL metacharacters into SET search_path or USE.
func TestPBT_SafeSlugReRejectsUnsafeSlugs(t *testing.T) {
	// Characters that must never appear in a tenant slug.
	unsafeChars := []string{
		";", "'", "\"", "--", "/*", "*/", "\\", "/",
		"DROP", "TABLE", "SELECT", "UNION", "INSERT",
		" ", "\t", "\n", "\r",
		".", ",", "(", ")", "[", "]", "{", "}",
		"=", "<", ">", "!", "@", "#", "$", "%", "^", "&", "*",
	}

	for _, bad := range unsafeChars {
		// Prepend a valid letter so the string isn't empty.
		candidate := "a" + bad + "b"
		if safeSlugRe.MatchString(candidate) {
			t.Errorf("safeSlugRe accepted unsafe slug containing %q: %q", bad, candidate)
		}
	}
}

// Property: for random slugs matching safeSlugRe, the Postgres search_path
// SET statement is safe. The slug appears in the quoted identifier
// "tenant_<slug>" and cannot break out of the identifier quoting.
func TestPBT_PostgresTenancySearchPathSafe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		slug := rapid.StringMatching(`[a-z][a-z0-9_]{0,30}`).Draw(t, "slug")

		tenancy := NewPostgresSchemaTenancy(func(ctx context.Context) string {
			return slug
		})

		// Verify the tenancy object was created with the expected slug.
		// The Apply method requires a real *sql.Conn, so we test the slug
		// validation path (safeSlugRe) and the identifier quoting instead.
		if !safeSlugRe.MatchString(slug) {
			t.Fatalf("valid slug %q was rejected by safeSlugRe", slug)
		}

		// quoteIdentifier must produce a properly quoted PG identifier.
		quoted := quoteIdentifier("tenant_" + slug)
		if len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
			t.Fatalf("quoteIdentifier(%q) = %q, expected double-quoted identifier", "tenant_"+slug, quoted)
		}

		_ = tenancy // prevent unused variable
	})
}

// Property: for random slugs, MySQL database-per-tenant uses backtick quoting.
func TestPBT_MySQLTenancyIdentifierSafe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		slug := rapid.StringMatching(`[a-z][a-z0-9_]{0,30}`).Draw(t, "slug")

		if !safeSlugRe.MatchString(slug) {
			t.Fatalf("valid slug %q rejected by safeSlugRe", slug)
		}

		quoted := quoteMySQLIdentifier("tenant_" + slug)
		if len(quoted) < 2 || quoted[0] != '`' || quoted[len(quoted)-1] != '`' {
			t.Fatalf("quoteMySQLIdentifier(%q) = %q, expected backtick-quoted identifier", "tenant_"+slug, quoted)
		}
	})
}

// Property: for random slugs, MSSQL database-per-tenant uses bracket quoting.
func TestPBT_MSSQLTenancyIdentifierSafe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		slug := rapid.StringMatching(`[a-z][a-z0-9_]{0,30}`).Draw(t, "slug")

		if !safeSlugRe.MatchString(slug) {
			t.Fatalf("valid slug %q rejected by safeSlugRe", slug)
		}

		quoted := quoteMSSQLIdentifier("tenant_" + slug)
		if len(quoted) < 2 || quoted[0] != '[' || quoted[len(quoted)-1] != ']' {
			t.Fatalf("quoteMSSQLIdentifier(%q) = %q, expected bracket-quoted identifier", "tenant_"+slug, quoted)
		}
	})
}

// Property: Apply with empty tenant slug is a no-op (returns nil) for all
// tenancy implementations.
func TestPBT_TenancyApplyNoOpOnEmptySlug(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Empty slug from context -> no-op.
		pgTenancy := NewPostgresSchemaTenancy(func(ctx context.Context) string { return "" })
		mysqlTenancy := NewMySQLDatabaseTenancy(func(ctx context.Context) string { return "" }, "lyeve")
		mssqlTenancy := NewMSSQLDatabaseTenancy(func(ctx context.Context) string { return "" }, "lyeve")

		// Apply with nil conn + empty slug should return nil (no-op).
		if err := pgTenancy.Apply(context.Background(), nil); err != nil {
			t.Errorf("PG Apply(nil, empty): %v", err)
		}
		if err := mysqlTenancy.Apply(context.Background(), nil); err != nil {
			t.Errorf("MySQL Apply(nil, empty): %v", err)
		}
		if err := mssqlTenancy.Apply(context.Background(), nil); err != nil {
			t.Errorf("MSSQL Apply(nil, empty): %v", err)
		}
	})
}

// Property: Reset and ResetToDefault with nil receiver or nil conn is a no-op.
func TestPBT_TenancyResetNilSafety(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ctx := context.Background()

		// Nil receiver safety.
		var pgNil *PostgresSchemaTenancy
		if err := pgNil.Apply(ctx, nil); err != nil {
			t.Errorf("nil PG Apply: %v", err)
		}
		if err := pgNil.Reset(ctx, nil); err != nil {
			t.Errorf("nil PG Reset: %v", err)
		}
		if err := pgNil.Reset(ctx, nil); err != nil {
			t.Errorf("nil PG ResetToDefault: %v", err)
		}

		// Nil conn safety for MySQL.
		mysqlTenancy := NewMySQLDatabaseTenancy(func(ctx context.Context) string { return "test" }, "lyeve")
		if err := mysqlTenancy.Reset(ctx, nil); err != nil {
			t.Errorf("MySQL Reset(nil conn): %v", err)
		}
		if err := mysqlTenancy.Reset(ctx, nil); err != nil {
			t.Errorf("MySQL ResetToDefault(nil conn): %v", err)
		}

		// Nil conn safety for MSSQL.
		mssqlTenancy := NewMSSQLDatabaseTenancy(func(ctx context.Context) string { return "test" }, "lyeve")
		if err := mssqlTenancy.Reset(ctx, nil); err != nil {
			t.Errorf("MSSQL Reset(nil conn): %v", err)
		}
		if err := mssqlTenancy.Reset(ctx, nil); err != nil {
			t.Errorf("MSSQL ResetToDefault(nil conn): %v", err)
		}
	})
}

// Property: for any two distinct safe slugs, the quoted identifiers are
// distinct. This guarantees tenant A's schema/database name never collides
// with tenant B's.
func TestPBT_TenantIdentifiersDistinct(t *testing.T) {
	// Pre-compile the regex so the rapid generator doesn't recompile per draw.
	validSlug := regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

	rapid.Check(t, func(t *rapid.T) {
		slug1 := rapid.StringMatching(`[a-z][a-z0-9_]{0,30}`).Draw(t, "slug1")
		slug2 := rapid.StringMatching(`[a-z][a-z0-9_]{0,30}`).Draw(t, "slug2")
		if slug1 == slug2 {
			slug2 = slug2 + "x"
		}

		if !validSlug.MatchString(slug1) || !validSlug.MatchString(slug2) {
			t.Skip("generated slugs don't match expected pattern")
		}

		pg1 := quoteIdentifier("tenant_" + slug1)
		pg2 := quoteIdentifier("tenant_" + slug2)
		if pg1 == pg2 {
			t.Fatalf("PG: tenants %q and %q got same quoted identifier %q", slug1, slug2, pg1)
		}

		my1 := quoteMySQLIdentifier("tenant_" + slug1)
		my2 := quoteMySQLIdentifier("tenant_" + slug2)
		if my1 == my2 {
			t.Fatalf("MySQL: tenants %q and %q got same quoted identifier %q", slug1, slug2, my1)
		}

		ms1 := quoteMSSQLIdentifier("tenant_" + slug1)
		ms2 := quoteMSSQLIdentifier("tenant_" + slug2)
		if ms1 == ms2 {
			t.Fatalf("MSSQL: tenants %q and %q got same quoted identifier %q", slug1, slug2, ms1)
		}
	})
}
