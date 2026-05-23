//go:build !mutest

package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// deviceLoginDialects runs each case against all three engines. Device sign-in
// is stored as raw SQL per dialect, so a case that passes on one proves
// nothing about the others.
var deviceLoginDialects = []struct {
	name string
	pool func(*testing.T) db.DB
}{
	{"postgres", func(t *testing.T) db.DB { return testdb.Postgres(t) }},
	{"mysql", func(t *testing.T) db.DB { return testdb.MySQL(t) }},
	{"mssql", func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
}

func newDeviceLogin(userCode, ip string, created time.Time) *domain.DeviceLogin {
	return &domain.DeviceLogin{
		ID:             uuid.New(),
		DeviceCodeHash: strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")[:64],
		UserCode:       userCode,
		ClientName:     "cli on build-box",
		RequesterIP:    ip,
		RequesterNet:   ip,
		PollInterval:   5,
		CreatedAt:      created,
		ExpiresAt:      created.Add(10 * time.Minute),
	}
}

func TestDeviceLoginStore_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, d := range deviceLoginDialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			ctx := context.Background()
			pool := d.pool(t)
			store := db.NewDeviceLoginStore(pool)
			now := time.Now().UTC().Truncate(time.Millisecond)

			first := newDeviceLogin("ABCDEFGH", "203.0.113.7", now)
			second := newDeviceLogin("JKMNPQRS", "203.0.113.7", now)
			require.NoError(t, store.Create(ctx, first))
			require.NoError(t, store.Create(ctx, second))

			// A user code is unique over every row.
			clash := newDeviceLogin("ABCDEFGH", "198.51.100.1", now)
			require.Error(t, store.Create(ctx, clash))

			got, err := store.GetByUserCode(ctx, "ABCDEFGH")
			require.NoError(t, err)
			assert.Equal(t, first.ID, got.ID)
			assert.Equal(t, domain.DeviceLoginPending, got.Status)
			assert.Equal(t, "cli on build-box", got.ClientName)
			assert.Equal(t, "203.0.113.7", got.RequesterIP)
			assert.Equal(t, 5, got.PollInterval)
			assert.Nil(t, got.UserID)
			assert.Nil(t, got.TenantID)
			assert.Nil(t, got.TokenVersion)
			assert.WithinDuration(t, first.ExpiresAt, got.ExpiresAt, time.Millisecond)

			byHash, err := store.GetByDeviceCodeHash(ctx, first.DeviceCodeHash)
			require.NoError(t, err)
			assert.Equal(t, first.ID, byHash.ID)
			_, err = store.GetByUserCode(ctx, "ZZZZZZZZ")
			assert.True(t, errors.Is(err, domain.ErrNotFound), "unknown code: %v", err)
			_, err = store.GetByDeviceCodeHash(ctx, "nope")
			assert.True(t, errors.Is(err, domain.ErrNotFound), "unknown hash: %v", err)

			open, err := store.CountPendingFrom(ctx, "203.0.113.7", now)
			require.NoError(t, err)
			assert.Equal(t, 2, open)

			// Approve binds the user, the tenant and the token version, once.
			user := uuid.New()
			tenant := "acme"
			require.NoError(t, store.Approve(ctx, first.ID, user, &tenant, 3, now))
			err = store.Approve(ctx, first.ID, user, &tenant, 3, now)
			assert.True(t, errors.Is(err, domain.ErrConflict), "second approval: %v", err)
			err = store.Deny(ctx, first.ID, now)
			assert.True(t, errors.Is(err, domain.ErrConflict), "deny after approval: %v", err)
			got, err = store.GetByUserCode(ctx, "ABCDEFGH")
			require.NoError(t, err)
			assert.Equal(t, domain.DeviceLoginApproved, got.Status)
			require.NotNil(t, got.UserID)
			assert.Equal(t, user, *got.UserID)
			require.NotNil(t, got.TenantID)
			assert.Equal(t, "acme", *got.TenantID)
			require.NotNil(t, got.TokenVersion)
			assert.Equal(t, 3, *got.TokenVersion)
			require.NotNil(t, got.ApprovedAt)

			open, err = store.CountPendingFrom(ctx, "203.0.113.7", now)
			require.NoError(t, err)
			assert.Equal(t, 1, open)
			all, err := store.CountPending(ctx, now)
			require.NoError(t, err)
			assert.Equal(t, 1, all)

			// Used once. A second exchange finds nothing to use.
			require.NoError(t, store.MarkUsed(ctx, first.ID))
			err = store.MarkUsed(ctx, first.ID)
			assert.True(t, errors.Is(err, domain.ErrConflict), "second use: %v", err)
			err = store.MarkUsed(ctx, second.ID)
			assert.True(t, errors.Is(err, domain.ErrConflict), "using a pending request: %v", err)

			// A request with no tenant is approved with none.
			require.NoError(t, store.Deny(ctx, second.ID, now))
			none := newDeviceLogin("TUVWXYZ2", "198.51.100.1", now)
			require.NoError(t, store.Create(ctx, none))
			require.NoError(t, store.Approve(ctx, none.ID, user, nil, 0, now))
			got, err = store.GetByUserCode(ctx, "TUVWXYZ2")
			require.NoError(t, err)
			assert.Nil(t, got.TenantID)
			assert.Equal(t, domain.DeviceLoginDenied, mustStatus(t, store, "JKMNPQRS"))

			// An expired request cannot be decided.
			stale := newDeviceLogin("23456789", "198.51.100.1", now.Add(-20*time.Minute))
			require.NoError(t, store.Create(ctx, stale))
			err = store.Approve(ctx, stale.ID, user, &tenant, 0, now)
			assert.True(t, errors.Is(err, domain.ErrConflict), "approving an expired request: %v", err)
			err = store.Deny(ctx, stale.ID, now)
			assert.True(t, errors.Is(err, domain.ErrConflict), "denying an expired request: %v", err)
		})
	}
}

func mustStatus(t *testing.T, store *db.DeviceLoginStore, code string) string {
	t.Helper()
	d, err := store.GetByUserCode(context.Background(), code)
	require.NoError(t, err)
	return d.Status
}

func TestDeviceLoginStore_PollingAndPrune(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, d := range deviceLoginDialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			ctx := context.Background()
			pool := d.pool(t)
			store := db.NewDeviceLoginStore(pool)
			now := time.Now().UTC().Truncate(time.Millisecond)

			req := newDeviceLogin("HJKMNPQR", "203.0.113.9", now)
			require.NoError(t, store.Create(ctx, req))

			// The first poll is always on time. One within the interval is not.
			ok, err := store.MarkPolled(ctx, req.ID, now, now.Add(-5*time.Second))
			require.NoError(t, err)
			assert.True(t, ok)
			ok, err = store.MarkPolled(ctx, req.ID, now.Add(2*time.Second), now.Add(-3*time.Second))
			require.NoError(t, err)
			assert.False(t, ok, "a poll two seconds after the last one is too soon")
			ok, err = store.MarkPolled(ctx, req.ID, now.Add(6*time.Second), now.Add(time.Second))
			require.NoError(t, err)
			assert.True(t, ok, "a poll six seconds after the last one is on time")

			// Slowing down grows the interval by the step and stops at the limit.
			require.NoError(t, store.SlowDown(ctx, req.ID, now.Add(7*time.Second), 5, 12))
			got, err := store.GetByUserCode(ctx, "HJKMNPQR")
			require.NoError(t, err)
			assert.Equal(t, 10, got.PollInterval)
			require.NotNil(t, got.LastPolledAt)
			assert.WithinDuration(t, now.Add(7*time.Second), *got.LastPolledAt, time.Millisecond)
			require.NoError(t, store.SlowDown(ctx, req.ID, now.Add(8*time.Second), 5, 12))
			got, err = store.GetByUserCode(ctx, "HJKMNPQR")
			require.NoError(t, err)
			assert.Equal(t, 12, got.PollInterval)

			// Prune takes only rows that expired before the cutoff, a batch at a time.
			old := []string{"AAAAAAAA", "BBBBBBBB", "CCCCCCCC"}
			for _, c := range old {
				require.NoError(t, store.Create(ctx, newDeviceLogin(c, "198.51.100.2", now.Add(-3*time.Hour))))
			}
			recent := newDeviceLogin("DDDDDDDD", "198.51.100.2", now.Add(-30*time.Minute))
			require.NoError(t, store.Create(ctx, recent))

			n, err := store.Prune(ctx, now.Add(-time.Hour), 2)
			require.NoError(t, err)
			assert.Equal(t, int64(2), n)
			n, err = store.Prune(ctx, now.Add(-time.Hour), 2)
			require.NoError(t, err)
			assert.Equal(t, int64(1), n)
			for _, c := range old {
				_, err := store.GetByUserCode(ctx, c)
				assert.True(t, errors.Is(err, domain.ErrNotFound), "%s survived the prune", c)
			}
			_, err = store.GetByUserCode(ctx, "DDDDDDDD")
			assert.NoError(t, err, "a request expired under an hour ago was pruned")
			_, err = store.GetByUserCode(ctx, "HJKMNPQR")
			assert.NoError(t, err, "a live request was pruned")
		})
	}
}
