package runtime

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// The erasure signs the subject out before any eraser runs, and erases the
// account only once every other eraser has succeeded. Signing out ends every
// session and keeps the account's address, which the other owners resolve
// the subject by on the next run.
func TestSysUserEraser_EndSessionsSignsOutAndKeepsTheAccount(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		wrapped := &revokingHost{Host: host}
		eraser := newSysUserEraser(wrapped)
		q := host.Querier(ctx)

		id := uuid.New()
		email := "signout-" + uuid.NewString()[:8] + "@example.com"
		_, err := q.Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash, token_version) VALUES ($1, $2, $3, 1)`,
			id, email, "hashed")
		require.NoError(t, err)

		require.NoError(t, eraser.EndSessions(ctx, id.String()))

		row, err := q.QueryRow(ctx, `SELECT token_version, email, password_hash FROM sys_users WHERE id = $1`, id)
		require.NoError(t, err)
		var tv int
		var gotEmail, hash string
		require.NoError(t, row.Scan(&tv, &gotEmail, &hash))
		assert.Equal(t, 2, tv, "every access token issued before is rejected")
		assert.Equal(t, []string{id.String()}, wrapped.revoked, "the refresh tokens are revoked")
		assert.Equal(t, email, gotEmail, "the account keeps its address until it is erased")
		assert.Equal(t, "hashed", hash)
	})
}
