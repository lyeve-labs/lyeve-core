package runtime

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

func sqlDBOf(t *testing.T, pool db.DB) *sql.DB {
	t.Helper()
	r, ok := pool.(interface{ SQLDB() *sql.DB })
	require.True(t, ok, "pool exposes no *sql.DB")
	return r.SQLDB()
}

// An install that runs no replication never gets a transport. The relay is
// still there, every wiring call still runs, and what it broadcasts is
// dropped rather than failing or panicking.
func TestReplicaSync_WithoutATransportIsInert(t *testing.T) {
	r := newReplicaSync("", slog.Default())
	require.NotNil(t, r.Bus())
	assert.False(t, r.bus.Attached())
	r.wireContent(nil)
	r.wireQueryCache(struct{}{})
	r.broadcast(context.Background(), "t", nil)
}
