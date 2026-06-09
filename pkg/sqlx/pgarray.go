package sqlx

// PGArray parses a PostgreSQL TEXT[] literal, {a,b,c} or {"quote me",b}, into
// its elements. Empty braces, braces holding only spaces, and an empty string
// give an empty slice. Anything else that is not wrapped in braces gives nil,
// so a caller can tell a value that is not an array literal from an empty
// array. It never panics.
func PGArray(raw string) []string {
	if raw == "" {
		return []string{}
	}
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return nil
	}
	if out := ParsePGArray(raw); out != nil {
		return out
	}
	return []string{}
}
