package runtime

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestGenerateMissingSecrets_OnlyForSecrets(t *testing.T) {
	t.Parallel()
	got, err := generateMissingSecrets([]config.SetupSetting{
		{EnvKey: "DATABASE_URL"},
		{EnvKey: "JWT_SECRET", Secret: true},
		{EnvKey: "ENCRYPTION_KEY", Secret: true},
	})
	require.NoError(t, err)
	assert.NotContains(t, got, "DATABASE_URL")
	assert.Len(t, got["JWT_SECRET"], 64)
	assert.Len(t, got["ENCRYPTION_KEY"], 64)
	assert.NotEqual(t, got["JWT_SECRET"], got["ENCRYPTION_KEY"], "the two keys must differ or production refuses to boot")
}

func TestRunSetupMode_ServesUntilStopped(t *testing.T) {
	t.Parallel()
	admin, apiAddr := freeAddr(t), freeAddr(t)
	req := &config.SetupRequiredError{
		Missing:         []config.SetupSetting{{EnvKey: "DATABASE_URL", YAMLPath: "database.url"}},
		AdminListenAddr: admin,
		APIListenAddr:   apiAddr,
		SetupToken:      "operator-setup-token-0123",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runSetupMode(ctx, req, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	get := func(addr, path string) int {
		for range 50 {
			resp, err := http.Get("http://" + addr + path)
			if err == nil {
				_ = resp.Body.Close()
				return resp.StatusCode
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("setup mode never answered on %s", addr)
		return 0
	}
	assert.Equal(t, http.StatusOK, get(admin, "/healthz"))
	assert.Equal(t, http.StatusServiceUnavailable, get(admin, "/readyz"))
	assert.Equal(t, http.StatusOK, get(apiAddr, "/healthz"))
	assert.Equal(t, http.StatusServiceUnavailable, get(apiAddr, "/api/v1/articles"))

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("setup mode did not stop when its context ended")
	}
}
