package core

// PasswordPolicyValidator is implemented by the engine host. It checks a
// password against the policy the engine's own routes apply, set by
// PASSWORD_MIN_LENGTH, PASSWORD_REQUIRE_COMPLEXITY, PASSWORD_CHECK_COMMON and
// the hash algorithm. A plugin that sets a password calls it, so the plugin
// accepts only what user create and first-run setup would accept.
//
// ScopedHost forwards it. Over a host without the role it answers
// ErrHostRoleUnsupported, and the plugin then applies checks of its own.
type PasswordPolicyValidator interface {
	// ValidatePassword returns nil for a password the policy accepts and a
	// *PasswordViolation for one it refuses.
	ValidatePassword(password string) error
}

// PasswordViolation is the refusal PasswordPolicyValidator returns. The
// engine's routes answer it with status 422, Code as the error code and Params
// as the values its message names, such as the minimum length.
type PasswordViolation struct {
	Code   string
	Params map[string]any
}

func (v *PasswordViolation) Error() string { return "password refused: " + v.Code }
