package core

// ContainsControlChars reports whether s contains a null byte (0x00), a C0
// control character (0x01-0x1f), or DEL (0x7f).
//
// A caller-supplied string carrying one of these is not portable across the
// engines: PostgreSQL refuses a null byte in a text column outright, while
// MySQL and MSSQL store it. Left unchecked, the same request is a 4xx on one
// dialect and a 201 on another, and the value that got through goes on to
// truncate logs and terminal output wherever it is echoed.
//
// Plugins validating a caller-supplied name, key or identifier should reject
// on this rather than leaving the decision to whichever database is behind
// them.
func ContainsControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
