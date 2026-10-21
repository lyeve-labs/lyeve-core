package core

// PasswordHasher is implemented by the engine host. It hashes a password with
// the algorithm the engine's own routes use, set by PASSWORD_HASH_ALGO, so a
// password a plugin sets is stored as user create stores it.
//
// ScopedHost forwards it. Over a host without the role both methods answer
// ErrHostRoleUnsupported, and the plugin then hashes with bcrypt.
type PasswordHasher interface {
	// PasswordMaxBytes is the longest password the hasher accepts, in bytes,
	// or 0 when it accepts any length. It lets a caller refuse a password
	// before it spends anything on it.
	PasswordMaxBytes() (int, error)

	// HashPassword returns the stored form of password.
	HashPassword(password string) (string, error)
}

// PasswordMaxBytes forwards to inner if it implements PasswordHasher. It
// reads no data, so it needs no capability.
func (h *ScopedHost) PasswordMaxBytes() (int, error) {
	if p, ok := h.inner.(PasswordHasher); ok {
		return p.PasswordMaxBytes()
	}
	return 0, ErrHostRoleUnsupported
}

// HashPassword forwards to inner if it implements PasswordHasher. A hash
// reads and grants nothing, so it needs no capability.
func (h *ScopedHost) HashPassword(password string) (string, error) {
	if p, ok := h.inner.(PasswordHasher); ok {
		return p.HashPassword(password)
	}
	return "", ErrHostRoleUnsupported
}
