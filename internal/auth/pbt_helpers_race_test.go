//go:build race

package auth_test

// skipExpensivePBT returns true when the race detector is active.
// PBKDF2 (600K HMAC-SHA256 iterations per EncryptSecret) is amplified
// 5-10x by TSan instrumentation of sha256.block and hmac, pushing the
// auth package past the per-package test timeout. The PBKDF2 code path
// has zero synchronization primitives (there is nothing for the race
// detector to find), so skipping under -race loses no real coverage.
func skipExpensivePBT() bool { return true }
