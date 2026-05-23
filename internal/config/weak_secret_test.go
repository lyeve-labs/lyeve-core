package config

import "testing"

// A placeholder must fail wherever it is copied from.
// "change-me-to-a-32-plus-character-jwt-secret" is 43 characters, so it clears
// the length check, and it equals no word in an exact-match set, so only a
// substring test catches it.
func TestIsWeakSecret(t *testing.T) {
	weak := []string{
		"change-me-to-a-32-plus-character-jwt-secret",
		"change-me-to-a-different-32-plus-character-key",
		"change-me-in-production-min-32-chars",
		"change-this-to-a-separate-high-entropy-key",
		"CHANGE-ME-TO-A-32-PLUS-CHARACTER-JWT-SECRET",
		"changeme",
		"replace-me-with-something-random",
		"your-secret-here-please-replace",
		"dev-secret",
		"insecure",
		"secret",
		"password",
		"default",
		"a-placeholder-value-for-local-development",
		"example-key-do-not-use-in-production",
	}
	for _, v := range weak {
		if !isWeakSecret(v) {
			t.Errorf("%q must not pass as a secret", v)
		}
	}

	// A real secret is not refused for containing an ordinary word. A guard
	// that fires on a good value is a guard somebody turns off.
	strong := []string{
		"7f3a9c1e5b8d2046a1c4e7f0b3d6895247ae0c13",
		"correct-horse-battery-staple-and-then-some",
		"defaults-are-fine-when-they-are-random-9f3a",
		"the-exemplary-key-material-here-is-random",
		"",
	}
	for _, v := range strong {
		if isWeakSecret(v) {
			t.Errorf("%q is a usable secret and must not be refused", v)
		}
	}
}

// The check has to be reached by the boot path, not only by its own test.
func TestValidateCritical_RefusesTheShippedPlaceholder(t *testing.T) {
	c := &Config{
		JWTSecret:     "change-me-to-a-32-plus-character-jwt-secret",
		EncryptionKey: "change-me-to-a-different-32-plus-character-key",
	}
	err := c.ValidateCritical()
	if err == nil {
		t.Fatal("the placeholder from the shipped manifest must not boot")
	}
}
