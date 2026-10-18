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
// unprivileged user. The boot error has to name the command that fixes it,
// because "permission denied" alone names nothing. A refused read grants the
// key file alone and never recurses.
func TestInitJWTSigning_RefusedKeyReadNamesTheFileChown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads and writes past file modes")
	}
	t.Setenv("JWT_ALG", "EdDSA")

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "jwt_key.json")
	require.NoError(t, os.WriteFile(keyPath, []byte("{}"), 0o000))

	err := auth.InitJWTSigning(keyPath)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.Contains(t, err.Error(), fmt.Sprintf("with chown %d:%d %s)", os.Getuid(), os.Getgid(), keyPath))
	assert.NotContains(t, err.Error(), "chown -R")
}

// The key may sit directly in a shared directory such as / or /var/lib, and
// owning a directory lets the engine rename or unlink any entry in it. A
// refused write of the key's .tmp file has to name a new subdirectory for the
// engine, never a chown of the existing directory.
func TestInitJWTSigning_RefusedKeyWriteNamesANewSubdirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads and writes past file modes")
	}
	t.Setenv("JWT_ALG", "EdDSA")

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "jwt_key.json")
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := auth.InitJWTSigning(keyPath)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.NotContains(t, err.Error(), "chown")
	assert.Contains(t, err.Error(), fmt.Sprintf("uid %d and gid %d", os.Getuid(), os.Getgid()))
	ownDir := filepath.Join(dir, "lyeve")
	assert.Contains(t, err.Error(), fmt.Sprintf("install -d -o %d -g %d -m 700 %s ", os.Getuid(), os.Getgid(), ownDir))
	assert.Contains(t, err.Error(), "set JWT_KEY_PATH to "+filepath.Join(ownDir, "jwt_key.json")+")")
}

// When the key directory is missing and its parent refuses the mkdir, the
// parent belongs to someone else. The hint has to create the key directory
// alone and never hand the parent, which may be / or /var/lib, to the engine.
func TestInitJWTSigning_RefusedMkdirCreatesOnlyTheKeyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads and writes past file modes")
	}
	t.Setenv("JWT_ALG", "EdDSA")

	cases := map[string]string{
		"key directory cannot be created":       filepath.Join("keys", "jwt_key.json"),
		"missing mount above the key directory": filepath.Join("data", "keys", "jwt_key.json"),
	}
	for name, keyName := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			keyPath := filepath.Join(dir, keyName)
			require.NoError(t, os.Chmod(dir, 0o500))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			err := auth.InitJWTSigning(keyPath)
			require.Error(t, err)
			assert.ErrorIs(t, err, os.ErrPermission)
			assert.Contains(t, err.Error(), fmt.Sprintf("install -d -o %d -g %d %s)", os.Getuid(), os.Getgid(), filepath.Dir(keyPath)))
			assert.NotContains(t, err.Error(), "chown -R")
		})
	}
}
