// Package sqlx provides dialect-aware SQL helpers shared across the engine and
// its plugins: identifier quoting, PostgreSQL array parsing, tag scanning, and
// safe JSON marshaling.
package sqlx

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Identifier quoting

// QuotePG wraps id in double quotes, doubling any embedded double quote.
// Example: say "hi" -> "say ""hi""".
func QuotePG(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// QuoteMySQL wraps id in backticks, doubling any embedded backtick.
// Example: say `hi` -> `say "hi```.
func QuoteMySQL(id string) string {
	return "`" + strings.ReplaceAll(id, "`", "``") + "`"
}

// QuoteMSSQL wraps id in brackets, doubling any embedded close bracket.
// Example: say [hi] -> [say [hi]]].
func QuoteMSSQL(id string) string {
	return "[" + strings.ReplaceAll(id, "]", "]]") + "]"
}

// QuoteMSSQLString doubles single quotes in s for safe embedding inside MSSQL
// string literals, so an apostrophe inside a value cannot end it early.
// Defense-in-depth for
// DB_ID('tenant_<slug>') guards.
func QuoteMSSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// TenantSchemaName returns the per-tenant schema (Postgres) or database
// (MySQL/MSSQL) identifier for a tenant slug.
//
// Slugs that fit as "tenant_<slug>" within the tightest identifier budget
// (Postgres's 63 bytes) keep their name verbatim, so existing tenants'
// schemas/databases are unchanged. Longer slugs are hashed so the identifier
// stays within every engine's limit. The slug itself remains stored in full
// and used as the X-Tenant-ID value. The hash is a pure function of the slug,
// so every caller derives the same name.
func TenantSchemaName(slug string) string {
	const prefix = "tenant_"
	if len(prefix+slug) <= 63 {
		return prefix + slug
	}
	sum := sha256.Sum256([]byte(slug))
	return prefix + hex.EncodeToString(sum[:16])
}

// QuoteIdent dispatches to QuotePG, QuoteMySQL, or QuoteMSSQL by dialect name.
// Unknown dialects fall back to PostgreSQL quoting.
func QuoteIdent(id, dialect string) string {
	switch dialect {
	case "mysql":
		return QuoteMySQL(id)
	case "mssql", "sqlserver":
		return QuoteMSSQL(id)
	default:
		return QuotePG(id)
	}
}

// PostgreSQL array parsing

// ParsePGArray parses a PostgreSQL TEXT[] literal ({a,b,c} or {"quoted value",b})
// into a []string. Returns nil for empty or whitespace-only braces, and for a
// value too short to hold the braces at all.
func ParsePGArray(raw string) []string {
	if len(raw) < 2 {
		return nil
	}
	inner := raw[1 : len(raw)-1] // strip { and }
	if strings.TrimSpace(inner) == "" {
		return nil
	}

	var result []string
	var current strings.Builder
	inQuotes := false
	escaped := false

	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		switch {
		case escaped:
			current.WriteByte(ch)
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
		case ch == ',' && !inQuotes:
			result = append(result, current.String())
			current.Reset()
		default:
			current.WriteByte(ch)
		}
	}
	result = append(result, current.String())
	return result
}

// Tag scanning

// ScanTags normalizes a tag-array wire value into []string. Accepted formats:
//   - PostgreSQL TEXT[]: {a,"b c",d}
//   - JSON array:        ["a","b c","d"]
//   - Null/empty:        "", "{}", "[]", "null" -> empty slice
//   - Bare string:       "plain" -> ["plain"]
func ScanTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "[]" || raw == "null" {
		return []string{}
	}

	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		parsed := ParsePGArray(raw)
		if parsed == nil {
			return []string{}
		}
		return parsed
	}

	if strings.HasPrefix(raw, "[") {
		var tags []string
		if err := json.Unmarshal([]byte(raw), &tags); err == nil {
			return tags
		}
	}

	return []string{raw}
}

// ScanTagsNull delegates to ScanTags, returning nil when ns is NULL.
func ScanTagsNull(ns sql.NullString) []string {
	if !ns.Valid {
		return nil
	}
	return ScanTags(ns.String)
}

// ScanTagsAny coerces a database-scanned value (string, []byte, nil, or
// fmt.Stringer fallback) to string, then delegates to ScanTags.
func ScanTagsAny(raw any) []string {
	var s string
	switch v := raw.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	case nil:
		return nil
	default:
		s = fmt.Sprintf("%v", v)
	}
	return ScanTags(s)
}

// Safe JSON marshaling

// MustMarshalBytes marshals v to JSON, returning []byte("{}") on failure so
// the result is always valid for database storage.
func MustMarshalBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// MustMarshalString marshals v to a JSON string, returning "" on failure.
// Suitable for wire protocols where an empty payload is acceptable degradation.
func MustMarshalString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
