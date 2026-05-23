//go:build !race

package auth_test

// All PBKDF2 property-based tests run at full iteration count.
func skipExpensivePBT() bool { return false }
