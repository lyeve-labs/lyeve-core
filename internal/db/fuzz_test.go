//go:build !mutest

// fuzz_test.go: Go native fuzz harnesses for tenant slug validation.
//
// Verifies that the safeSlugRe regex never panics on arbitrary input, and
// that any slug accepted by the regex is safe to embed in DDL across all
// three SQL dialects (Postgres search_path, MySQL USE, MSSQL USE).
//
// Run:
//
//	go test -fuzz=FuzzSlugValidate -fuzztime=30s ./internal/db/
//	make fuzz-schema-short    # 30s each
//	make fuzz-schema           # 60s each

package db

import (
	"strings"
	"testing"
)

// FuzzSlugValidate fuzzes the safeSlugRe regex with arbitrary strings.
// Asserts:
//   - Regex evaluation never panics on any input
//   - No accepted slug contains SQL-injection characters (quotes, backticks,
//     brackets, semicolons, comment sequences)
//   - No accepted slug can break search_path/USE/DDL context
//   - No accepted slug exceeds 63 characters (PG identifier limit)
//
// This is the seatbelt for tenant isolation: a slug that passes
// safeSlugRe ends up in raw SQL (SET search_path, USE [tenant_<slug>]).
// If the regex is too permissive, a tenant slug can inject SQL.
func FuzzSlugValidate(f *testing.F) {
	// Seed corpus: valid slugs, adversarial inputs, boundary cases.
	f.Add("a")                     // minimum valid
	f.Add("acme")                  // normal
	f.Add("acme_corp")             // underscore
	f.Add("tenant123")             // trailing digits
	f.Add(strings.Repeat("a", 63)) // max length
	f.Add("")                      // empty
	f.Add("ACME")                  // uppercase
	f.Add("tenant with space")     // whitespace
	f.Add("tenant;DROP TABLE x")   // statement separator
	f.Add("1digit")                // starts with digit
	f.Add("-leading-dash")         // starts with dash
	f.Add("_leading_underscore")   // starts with underscore
	f.Add(strings.Repeat("a", 64)) // one over max
	f.Add("café")                  // accented
	f.Add("\x00slug")              // null byte prefix
	f.Add("slug\x00end")           // embedded null byte
	f.Add("slug\ninjection")       // embedded newline
	f.Add("`backtick`")            // MySQL injection
	f.Add(`"doublequote"`)         // PG injection
	f.Add("]bracket[")             // MSSQL injection
	f.Add("a-- comment")           // SQL line comment
	f.Add("a/* block */")          // SQL block comment
	f.Add("a' OR '1'='1")          // tautology
	f.Add("a\x1bESC")              // ANSI escape
	f.Add("a\ttab")                // tab

	f.Fuzz(func(t *testing.T, slug string) {
		// The regex must never panic on any input.
		accepted := safeSlugRe.MatchString(slug)

		if !accepted {
			return // rejected: no further checks needed
		}

		// Invariant 1: accepted slug is [a-z][a-z0-9_]{0,62}
		if len(slug) == 0 {
			t.Fatal("empty slug accepted by safeSlugRe")
		}
		if len(slug) > 63 {
			t.Fatalf("slug %q (len=%d) exceeds 63-char limit but was accepted", slug, len(slug))
		}
		if slug[0] < 'a' || slug[0] > 'z' {
			t.Fatalf("slug %q starts with %q (not [a-z]) but was accepted", slug, string(slug[0]))
		}
		for i, c := range slug {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
				t.Fatalf("slug %q contains invalid char %q at position %d but was accepted", slug, string(c), i)
			}
		}

		// Invariant 2: accepted slug cannot break DDL context
		// These characters, if present in a slug, could allow SQL injection
		// when concatenated into SET search_path or USE statements.
		forbidden := []struct {
			char string
			desc string
		}{
			{`"`, "double-quote (PG identifier escape)"},
			{"`", "backtick (MySQL identifier escape)"},
			{"]", "close-bracket (MSSQL identifier escape)"},
			{"[", "open-bracket (MSSQL identifier)"},
			{"'", "single-quote (SQL string delimiter)"},
			{";", "statement separator"},
			{"\n", "newline (statement terminator)"},
			{"\r", "carriage return"},
			{"\x00", "null byte"},
			{"--", "SQL line comment"},
			{"/*", "SQL block comment start"},
			{"*/", "SQL block comment end"},
		}
		for _, fb := range forbidden {
			if strings.Contains(slug, fb.char) {
				t.Fatalf("accepted slug %q contains %s - DDL injection risk", slug, fb.desc)
			}
		}

		// Invariant 3: quoting roundtrip holds
		// QuoteIdentifier (PG, MySQL, MSSQL) must produce a well-formed
		// identifier that round-trips back to the original slug.
		fullSlug := "tenant_" + slug

		// PG: "tenant_slug" -> inner -> unescape "" -> " -> original
		pgQuoted := quoteIdentifier(fullSlug)
		if !strings.HasPrefix(pgQuoted, `"`) || !strings.HasSuffix(pgQuoted, `"`) {
			t.Fatalf("PG quote of %q = %q: not double-quoted", fullSlug, pgQuoted)
		}
		pgInner := pgQuoted[1 : len(pgQuoted)-1]
		if strings.ReplaceAll(pgInner, `""`, `"`) != fullSlug {
			t.Fatalf("PG roundtrip failed for %q", fullSlug)
		}

		// MySQL: `tenant_slug` -> inner -> unescape `` -> ` -> original
		mysqlQuoted := quoteMySQLIdentifier(fullSlug)
		if !strings.HasPrefix(mysqlQuoted, "`") || !strings.HasSuffix(mysqlQuoted, "`") {
			t.Fatalf("MySQL quote of %q = %q: not backtick-quoted", fullSlug, mysqlQuoted)
		}
		mysqlInner := mysqlQuoted[1 : len(mysqlQuoted)-1]
		if strings.ReplaceAll(mysqlInner, "``", "`") != fullSlug {
			t.Fatalf("MySQL roundtrip failed for %q", fullSlug)
		}

		// MSSQL: [tenant_slug] -> inner -> unescape ]] -> ] -> original
		mssqlQuoted := quoteMSSQLIdentifier(fullSlug)
		if !strings.HasPrefix(mssqlQuoted, "[") || !strings.HasSuffix(mssqlQuoted, "]") {
			t.Fatalf("MSSQL quote of %q = %q: not bracket-quoted", fullSlug, mssqlQuoted)
		}
		mssqlInner := mssqlQuoted[1 : len(mssqlQuoted)-1]
		if strings.ReplaceAll(mssqlInner, "]]", "]") != fullSlug {
			t.Fatalf("MSSQL roundtrip failed for %q", fullSlug)
		}
	})
}
