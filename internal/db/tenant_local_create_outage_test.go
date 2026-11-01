package db_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// A tenant-local create that fails because the database is unreachable
// reports the failure, which a handler answers as unavailable, and never as a
// conflict over the address.
func TestCreateTenantLocal_UnreachableDatabaseIsNotAConflict(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		users := db.NewUserStore(pool)
		require.NoError(t, pool.Close())

		_, created, err := users.CreateTenantLocal(context.Background(), "acme", "down-"+uuid.NewString()[:8]+"@example.com", "", nil, true)
		require.Error(t, err)
		assert.False(t, created)
		assert.NotErrorIs(t, err, domain.ErrConflict)
		assert.Equal(t, http.StatusServiceUnavailable, httpx.StoreStatusFor(err))
	})
}
