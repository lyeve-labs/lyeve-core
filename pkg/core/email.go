package core

import (
	"net/mail"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// NormalizeEmail returns the canonical stored form of an address: trimmed of
// surrounding whitespace, put in Unicode normalization form C and folded to
// lower case, as FoldAddress folds it.
//
// sys_users.email carries one unique constraint, but the three engines do not
// agree on what makes two addresses equal. PostgreSQL compares TEXT case
// sensitively, so Foo@x.com and foo@x.com are two separate accounts there.
// MySQL under utf8mb4_0900_ai_ci and SQL Server under its default collation
// fold case, so the same pair violates the key. Without one rule an install
// would behave differently depending on its database.
//
// Folding inside the query does not help. A predicate written
// LOWER(email) = LOWER($1) cannot use the unique index on email, so every such
// lookup would scan the table, and call sites that fold differently resolve
// one address to different accounts.
//
// So the address is stored lower case, and every lookup normalizes its
// argument through this function before comparing with a plain email = $1,
// which the unique index serves. Read and write must call this same function,
// because two sides that fold differently split one address in two.
//
// Case is not the only way one address is spelled twice. josé@ typed with a
// precomposed é and with e and a combining accent are one address, and
// PostgreSQL compares them as different bytes, so without the normalization
// form two accounts held one address there.
func NormalizeEmail(email string) string {
	return FoldAddress(strings.TrimSpace(email))
}

// FoldAddress puts an address in Unicode normalization form C and lowers it,
// the form in which two spellings of one address are equal. It trims nothing,
// because what surrounds an address is the caller's to decide. Lowering is
// all it folds: Unicode case folding would also take ſam@ for sam@, a
// different address, and accents and ß stay significant.
func FoldAddress(address string) string {
	return strings.ToLower(norm.NFC.String(address))
}

// ValidEmail reports whether s is an email address, by the rule a schema field
// of type email applies to what is written to it: net/mail parses it, and it
// holds an at sign and a dot.
func ValidEmail(s string) bool {
	if _, err := mail.ParseAddress(s); err != nil {
		return false
	}
	return strings.Contains(s, "@") && strings.Contains(s, ".")
}
