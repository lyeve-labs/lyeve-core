package auth_test

import (
	"fmt"
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

// A platform disk usually arrives owned by root while the image runs as an
// unprivileged user. The boot error has to name the directory and the chown
// that fixes it, because "permission denied" alone names neither.
func TestInitJWTSigning_RefusedDataDirNamesTheChown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads and writes past file modes")
	}
	t.Setenv("JWT_ALG", "EdDSA")

	cases := map[string]struct {
		keyName string
		refuse  func(t *testing.T, dir, keyPath string)
	}{
		"directory not writable": {"jwt_key.json", func(t *testing.T, dir, _ string) {
			require.NoError(t, os.Chmod(dir, 0o500))
		}},
		"key file not readable": {"jwt_key.json", func(t *testing.T, _, keyPath string) {
			require.NoError(t, os.WriteFile(keyPath, []byte("{}"), 0o000))
		}},
		"key directory cannot be created": {filepath.Join("keys", "jwt_key.json"), func(t *testing.T, dir, _ string) {
			require.NoError(t, os.Chmod(dir, 0o500))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			keyPath := filepath.Join(dir, tc.keyName)
			tc.refuse(t, dir, keyPath)
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			err := auth.InitJWTSigning(keyPath)
			require.Error(t, err)
			assert.ErrorIs(t, err, os.ErrPermission)
			assert.Contains(t, err.Error(), fmt.Sprintf("chown -R %d:%d %s)", os.Getuid(), os.Getgid(), dir))
		})
	}
}
