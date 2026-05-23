package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// rolesScanner implements sql.Scanner for scanning PostgreSQL TEXT[] and
// MySQL/MSSQL JSON array values into a []string. pgx/v5/stdlib returns
// TEXT[] as string through database/sql, so the scanner parses it manually.
type rolesScanner struct {
	dst *[]string
}

func newRolesScanner(dst *[]string) *rolesScanner {
	return &rolesScanner{dst: dst}
}

// Scan implements sql.Scanner.
func (s *rolesScanner) Scan(src interface{}) error {
	if src == nil {
		*s.dst = nil
		return nil
	}

	var raw string
	switch v := src.(type) {
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("unsupported Scan type %T for roles", src)
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		*s.dst = []string{}
		return nil
	}

	// PostgreSQL array literal: {val1,val2} or {"val1","val2"}
	if strings.HasPrefix(raw, "{") {
		interior := raw[1 : len(raw)-1]
		if interior == "" {
			*s.dst = []string{}
			return nil
		}
		*s.dst = splitPGArray(interior)
		return nil
	}

	// JSON array: ["val1","val2"]
	if strings.HasPrefix(raw, "[") {
		var roles []string
		if err := json.Unmarshal([]byte(raw), &roles); err != nil {
			return fmt.Errorf("parse roles JSON: %w", err)
		}
		*s.dst = roles
		return nil
	}

	return fmt.Errorf("unrecognized roles format: %s", raw)
}

// encodeStringArray renders a []string for insertion across dialects. Postgres
// stores these columns as TEXT[], which the pgx driver binds from a []string
// directly. MySQL and MSSQL store them as JSON, so the slice is marshaled to a
// JSON array string. Read back with rolesScanner, which parses both forms.
func encodeStringArray(engine string, v []string) (any, error) {
	if v == nil {
		v = []string{} // these columns are NOT NULL. Never bind SQL NULL
	}
	if engine == "postgres" || engine == "postgresql" || engine == "" {
		return v, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// splitPGArray parses the interior of a PostgreSQL array literal
// (between the outer braces). Handles quoted values, commas inside quotes,
// and backslash/doubled-quote escaping. Whitespace is trimmed from
// unquoted values but preserved inside quoted values.
func splitPGArray(s string) []string {
	if s == "" {
		return []string{""}
	}

	var parts []string
	var current strings.Builder
	inQuotes := false
	partQuoted := false

	flush := func() {
		val := current.String()
		if !partQuoted {
			val = strings.TrimSpace(current.String())
		}
		parts = append(parts, val)
		current.Reset()
		partQuoted = false
	}

	for i := 0; i < len(s); i++ {
		ch := s[i]

		switch {
		case ch == '"':
			if inQuotes {
				// Check for SQL-standard doubled quote: skip the second
				if i+1 < len(s) && s[i+1] == '"' {
					current.WriteByte('"')
					i++
				} else {
					inQuotes = false
				}
			} else {
				inQuotes = true
				partQuoted = true
			}

		case ch == '\\' && inQuotes && i+1 < len(s):
			// Backslash escape: \" -> "
			i++
			current.WriteByte(s[i])

		case ch == ',' && !inQuotes:
			flush()

		default:
			current.WriteByte(ch)
		}
	}

	flush()
	return parts
}

// NewRolesScanner returns a Scanner that reads a roles-shaped column into dst.
// The column is TEXT[] on Postgres and a JSON array on MySQL and MSSQL. The
// scanner accepts either. Exported for callers outside this package that read
// sys_users directly.
func NewRolesScanner(dst *[]string) sql.Scanner { return newRolesScanner(dst) }
