package api

import "time"

// lockoutPolicy returns the brute-force account lockout thresholds.
//
// These are the only values the binary has. Nothing at runtime selects
// another set, so an account lock cannot be loosened by configuration.
func lockoutPolicy() (maxFailedAttempts int, lockoutWindow time.Duration) {
	return 5, 15 * time.Minute
}

// accountAttemptCeiling is how many password attempts one account takes in a
// lockout window from every network together. The per-network limit is the
// one an ordinary caller meets, and it stops a single source from locking the
// owner out. This ceiling stops a guesser who spreads attempts over many
// networks. At ten times the per-network limit, an owner who mistypes from
// several networks never reaches it, and keeping the owner out takes ten
// networks inside one window.
const accountAttemptCeiling = 50
