package api

import "time"

// lockoutPolicy returns the brute-force account lockout thresholds.
//
// These are the only values the binary has. Nothing at runtime selects
// another set, so an account lock cannot be loosened by configuration.
func lockoutPolicy() (maxFailedAttempts int, lockoutWindow time.Duration) {
	return 5, 15 * time.Minute
}
