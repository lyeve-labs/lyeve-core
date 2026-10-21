package config_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// The engine logs one JSON object per line, and a log pipeline that parses
// each line drops or rejects plain text. A plaintext database URL is reported
// through the structured production warnings, so loading the config writes
// nothing of its own to stderr.
func TestLoad_PlaintextDatabaseURLsWriteNoPlainTextLine(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATABASE_URL", "postgres://localhost/cms?sslmode=disable")
	t.Setenv("DATABASE_REPLICA_URL", "postgres://replica/cms?sslmode=disable")
	t.Setenv("JWT_SECRET", "test-secret-16+chars")
	t.Setenv("ENCRYPTION_KEY", "separate-encryption-key-128-bits-long")

	orig := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	cfg, loadErr := config.Load()
	require.NoError(t, w.Close())
	os.Stderr = orig
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	require.NoError(t, loadErr)
	require.NotNil(t, cfg)
	require.Empty(t, buf.String(), "config load wrote a line that is not the structured log")
}

// The plain stderr line was the only report of a plaintext replica, so the
// structured warnings name the replica the way they name the primary.
func TestProductionWarnings_NameAPlaintextReplica(t *testing.T) {
	cfg := &config.Config{
		Environment:        "production",
		DatabaseURL:        "postgres://host/db?sslmode=require",
		DatabaseReplicaURL: "postgres://replica/db?sslmode=disable",
		ConsoleURL:         "https://admin.example.com",
	}
	warns := cfg.ProductionWarnings()
	require.Len(t, warns, 1)
	require.Contains(t, warns[0], "DATABASE_REPLICA_URL sets sslmode=disable")

	cfg.DatabaseReplicaURL = "postgres://replica/db?sslmode=verify-full"
	require.Empty(t, cfg.ProductionWarnings())
}
