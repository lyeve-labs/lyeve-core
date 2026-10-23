package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// TestRun_KeysSubjectRefsToTheInstallSecret boots the engine twice with one
// ENCRYPTION_KEY and a fresh process key before each boot, the way two
// replicas or a restart start. Both must log a subject by the same reference,
// and neither by the SHA-256 prefix anyone holding the address can compute.
func TestRun_KeysSubjectRefsToTheInstallSecret(t *testing.T) {
	const address = "subject@example.com"
	const encryptionKey = "subject-ref-boot-encryption-key-0123456789abcdef"

	boot := func() string {
		t.Helper()
		randomKey := make([]byte, 32)
		_, _ = rand.Read(randomKey)
		require.NoError(t, compliance.SetSubjectRefSecret(randomKey))

		addr := freeAddr(t)
		for k, v := range map[string]string{
			"LYEVE_MODE":              "stateless",
			"LYEVE_CONFIG":            "",
			"APP_ENV":                 "development",
			"API_LISTEN_ADDR":         addr,
			"LYEVE_LICENSE_KEY":       "",
			"LYEVE_LICENSE_CACHE_DIR": t.TempDir(),
			"DATABASE_URL":            "",
			"DATABASE_REPLICA_URL":    "",
			"JWT_SECRET":              "subject-ref-boot-jwt-secret-0123456789",
			"ENCRYPTION_KEY":          encryptionKey,
			"LYEVE_SETUP_MODE":        "",
			"MULTI_TENANT":            "",
			"LYEVE_PLUGINS":           "",
		} {
			t.Setenv(k, v)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- RunWithOptions(ctx, Options{}) }()
		defer func() {
			cancel()
			select {
			case err := <-done:
				assert.NoError(t, err, "the engine stops cleanly when its context ends")
			case <-time.After(10 * time.Second):
				t.Error("the engine did not stop")
			}
		}()
		require.Eventually(t, func() bool {
			resp, err := http.Get("http://" + addr + "/readyz")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 10*time.Second, 50*time.Millisecond, "the engine never became ready")
		return compliance.SubjectRef(address)
	}

	t.Cleanup(func() {
		randomKey := make([]byte, 32)
		_, _ = rand.Read(randomKey)
		_ = compliance.SetSubjectRefSecret(randomKey)
	})
	forgetBootRegistries(t)

	first := boot()
	second := boot()

	assert.Equal(t, first, second, "two engines holding one ENCRYPTION_KEY must log a subject by one reference")
	sum := sha256.Sum256([]byte(address))
	assert.NotEqual(t, hex.EncodeToString(sum[:6]), first, "the reference must not be the SHA-256 prefix of the address")
	require.NoError(t, compliance.SetSubjectRefSecret([]byte(encryptionKey)))
	assert.Equal(t, compliance.SubjectRef(address), first, "the boot keys the reference to ENCRYPTION_KEY")
}
