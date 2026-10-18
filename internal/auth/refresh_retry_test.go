package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// closeRetryWindow moves a family's last rotation eleven seconds into the
// past, which is the record the store reads once the window has run out. The
// rest of the stored bytes stay as they were, so a later compare-and-set
// still matches them.
func closeRetryWindow(t *testing.T, backend core.CacheBackend, keyspace, familyID string) {
	t.Helper()
	ctx := context.Background()
	key := keyspace + ":rt:family:" + familyID
	b, err := backend.Get(ctx, key)
	require.NoError(t, err)
	var rec struct {
		IssuedAt string `json:"issued_at"`
	}
	require.NoError(t, json.Unmarshal(b, &rec))
	at, err := time.Parse(time.RFC3339Nano, rec.IssuedAt)
	require.NoError(t, err)
	earlier := at.Add(-11 * time.Second).Format(time.RFC3339Nano)
	b = bytes.Replace(b, []byte(`"issued_at":"`+rec.IssuedAt+`"`), []byte(`"issued_at":"`+earlier+`"`), 1)
	require.NoError(t, backend.Set(ctx, key, b, time.Hour))
}

func TestRefreshTokenStore_Rotate_RetryWithinWindowKeepsFamily(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	rotated, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenRotated, "a retry within the window is not replay")

	_, err = store.Rotate(ctx, rotated.RefreshToken, 5*time.Minute)
	require.NoError(t, err, "the successor must still rotate after the retry")
}

func TestRefreshTokenStore_Rotate_RetryAfterWindowIsReuse(t *testing.T) {
	backend := auth.NewMemoryBackend()
	store := auth.NewRefreshTokenStore(backend, "cms-test")
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	rotated, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenRotated)
	closeRetryWindow(t, backend, "cms-test", issued.FamilyID)
	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenReuse, "the window has closed")

	_, err = store.Rotate(ctx, rotated.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrFamilyRevoked)
}

func TestRefreshTokenStore_Unrotate_MakesTheSentTokenCurrent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	undelivered, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, store.Unrotate(ctx, undelivered, 5*time.Minute))

	_, err = store.Rotate(ctx, undelivered.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenInvalid, "the undelivered token must not work")

	rotated, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err, "the sent token must be current again")
	_, err = store.Rotate(ctx, rotated.RefreshToken, 5*time.Minute)
	require.NoError(t, err)
}

func TestRefreshTokenStore_Unrotate_LeavesAFamilyThatMovedOn(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	first, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)
	second, err := store.Rotate(ctx, first.RefreshToken, 5*time.Minute)
	require.NoError(t, err)

	require.NoError(t, store.Unrotate(ctx, first, 5*time.Minute))

	_, err = store.Rotate(ctx, second.RefreshToken, 5*time.Minute)
	require.NoError(t, err, "a stale restore must not undo a later rotation")
}

func TestRefreshTokenStore_Unrotate_KeepsAnOlderTokenReplay(t *testing.T) {
	backend := auth.NewMemoryBackend()
	store := auth.NewRefreshTokenStore(backend, "cms-test")
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	first, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)
	closeRetryWindow(t, backend, "cms-test", issued.FamilyID)
	undelivered, err := store.Rotate(ctx, first.RefreshToken, 5*time.Minute)
	require.NoError(t, err)
	require.NoError(t, store.Unrotate(ctx, undelivered, 5*time.Minute))

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenReuse, "a restore must not reopen the window for the token before")
}
