package auth_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

func TestInitJWTSigning_GeneratesAndPersistsEd25519Keypair(t *testing.T) {
	oldAlg := os.Getenv("JWT_ALG")
	t.Cleanup(func() {
		if oldAlg != "" {
			os.Setenv("JWT_ALG", oldAlg)
		} else {
			os.Unsetenv("JWT_ALG")
		}
	})
	os.Unsetenv("JWT_ALG")

	keyPath := filepath.Join(t.TempDir(), "jwt_key.json")

	err := auth.InitJWTSigning(keyPath)
	require.NoError(t, err, "first InitJWTSigning should generate a keypair")

	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(50), "key file should contain JSON data")

	pub1 := auth.PublicKey()
	require.NotNil(t, pub1, "PublicKey must be non-nil after InitJWTSigning")

	err = auth.InitJWTSigning(keyPath)
	require.NoError(t, err, "second InitJWTSigning should load the existing key")

	pub2 := auth.PublicKey()
	require.NotNil(t, pub2, "PublicKey must be non-nil after second InitJWTSigning")
	assert.Equal(t, pub1, pub2, "PublicKey must be identical (same key loaded from disk)")
}
