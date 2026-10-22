package enginehost

import "github.com/lyeve-labs/lyeve-core/internal/auth"

// passwordHashAlgo is the algorithm the engine's own routes hash with. A host
// built without a configuration uses the engine's default.
func (h *engineHost) passwordHashAlgo() string {
	if h.cfg == nil {
		return ""
	}
	return h.cfg.PasswordHashAlgo
}

// PasswordMaxBytes implements core.PasswordHasher.
func (h *engineHost) PasswordMaxBytes() (int, error) {
	return auth.MaxPasswordBytes(h.passwordHashAlgo()), nil
}

// HashPassword implements core.PasswordHasher.
func (h *engineHost) HashPassword(password string) (string, error) {
	return auth.HashPassword(h.passwordHashAlgo(), password)
}
