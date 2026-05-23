package core

import "strings"

// NormalizeEmail returns the canonical stored form of an address: trimmed of
// surrounding whitespace and folded to lower case.
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
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
