package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Over a host without the hasher role both methods say so, and the plugin
// then hashes with bcrypt.
func TestScopedHost_PasswordHasherOverAHostWithoutTheRole(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "password-reset", CapAll)
	_, err := h.PasswordMaxBytes()
	assert.ErrorIs(t, err, ErrHostRoleUnsupported)
	_, err = h.HashPassword("Correct-Horse-9")
	assert.ErrorIs(t, err, ErrHostRoleUnsupported)
}
