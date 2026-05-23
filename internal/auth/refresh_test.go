package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// newTestStore creates a RefreshTokenStore backed by the in-process memory
// backend for fast, isolated unit tests: no external dependency.
func newTestStore(t *testing.T) *auth.RefreshTokenStore {
	t.Helper()
	return auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
}

func TestRefreshTokenStore_Issue_Success(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	result, err := store.Issue(ctx, "user-1", 15*time.Minute)
	require.NoError(t, err, "Issue should succeed")
	assert.NotEmpty(t, result.RefreshToken, "refresh token should not be empty")
	assert.NotEmpty(t, result.FamilyID, "family ID should not be empty")
	assert.True(t, result.ExpiresAt.After(time.Now()), "expires_at should be in the future")
	assert.True(t, result.ExpiresAt.Before(time.Now().Add(16*time.Minute)), "expires_at should be within TTL")

	// Token format: {uuid}.{base64url random}
	assert.Contains(t, result.RefreshToken, ".", "refresh token should contain '.' separator")
}

func TestRefreshTokenStore_Issue_MultipleUsers(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r1, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	r2, err := store.Issue(ctx, "user-2", 5*time.Minute)
	require.NoError(t, err)

	assert.NotEqual(t, r1.FamilyID, r2.FamilyID, "different users should get different families")
	assert.NotEqual(t, r1.RefreshToken, r2.RefreshToken)
}

func TestRefreshTokenStore_Issue_SameUserMultipleDevices(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r1, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	r2, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	assert.NotEqual(t, r1.FamilyID, r2.FamilyID, "same user on different devices = different families")
}

func TestRefreshTokenStore_Rotate_HappyPath(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	rotated, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err, "first rotation should succeed")
	assert.NotEmpty(t, rotated.RefreshToken, "new refresh token should not be empty")
	assert.NotEqual(t, issued.RefreshToken, rotated.RefreshToken, "rotated token should differ from issued")
	assert.Equal(t, "user-1", rotated.UserID, "user ID should be preserved")
	assert.Equal(t, issued.FamilyID, rotated.FamilyID, "family ID should be preserved across rotations")
	assert.True(t, rotated.ExpiresAt.After(time.Now()), "new expiry should be in the future")
}

func TestRefreshTokenStore_Rotate_MultipleRotations(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	var prev = issued.RefreshToken
	for i := 0; i < 5; i++ {
		rotated, err := store.Rotate(ctx, prev, 5*time.Minute)
		require.NoError(t, err, "rotation %d should succeed", i+1)
		assert.NotEqual(t, prev, rotated.RefreshToken, "each rotation should produce a new token")
		prev = rotated.RefreshToken
	}
}

func TestRefreshTokenStore_Rotate_ReplayDetection(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err, "first rotation should succeed")

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenReuse, "replaying a rotated token should return ErrTokenReuse")

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrFamilyRevoked, "family should be revoked after replay detection")
}

func TestRefreshTokenStore_Rotate_InvalidToken(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	_, err = store.Rotate(ctx, "not-a-valid-token-at-all", 5*time.Minute)
	require.Error(t, err, "rotating a garbage token should fail")
}

func TestRefreshTokenStore_Rotate_UnknownFamily(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Well-formed token with a nonexistent family ID: parse succeeds
	// (valid UUID + suffix), but no family record exists.
	_, err := store.Rotate(ctx, "00000000-0000-0000-0000-000000000000.abcd", 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenInvalid, "unknown family should return ErrTokenInvalid")
}

func TestRefreshTokenStore_Rotate_ExpiredToken(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 1*time.Second)
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenExpired, "expired token should return ErrTokenExpired")
}

func TestRefreshTokenStore_RevokeFamily(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	err = store.RevokeFamily(ctx, issued.FamilyID)
	require.NoError(t, err, "RevokeFamily should succeed")

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrFamilyRevoked, "token from revoked family should be rejected")
}

func TestRefreshTokenStore_RevokeAllForUser(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r1, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	r2, err := store.Issue(ctx, "user-1", 5*time.Minute) // second device
	require.NoError(t, err)
	r3, err := store.Issue(ctx, "user-3", 5*time.Minute)
	require.NoError(t, err)

	err = store.RevokeAllForUser(ctx, "user-1")
	require.NoError(t, err, "RevokeAllForUser should succeed")

	_, err = store.Rotate(ctx, r1.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrFamilyRevoked, "user-1 device 1 should be revoked")
	_, err = store.Rotate(ctx, r2.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrFamilyRevoked, "user-1 device 2 should be revoked")

	_, err = store.Rotate(ctx, r3.RefreshToken, 5*time.Minute)
	require.NoError(t, err, "user-3 family should not be affected by user-1 revocation")
}

func TestRefreshTokenStore_Ping(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.Ping(ctx)
	require.NoError(t, err, "Ping should succeed")
}

func TestRefreshTokenStore_Issue_DifferentTTL(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	result, err := store.Issue(ctx, "user-1", 30*time.Minute)
	require.NoError(t, err)

	assert.True(t, result.ExpiresAt.After(time.Now().Add(29*time.Minute)), "expiry should match TTL")
	assert.True(t, result.ExpiresAt.Before(time.Now().Add(31*time.Minute)))
}

func TestRefreshTokenStore_ReplayRevokesFamily_AllTokensInFamily(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	rotated, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)

	rotated2, err := store.Rotate(ctx, rotated.RefreshToken, 5*time.Minute)
	require.NoError(t, err)

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrTokenReuse)

	_, err = store.Rotate(ctx, rotated2.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, auth.ErrFamilyRevoked, "current token should also be rejected after family revoked")
}

// TestRefreshTokenStore_Rotate_CASCrossReplicaRace verifies two
// concurrent Rotate calls on the same token, each running on a distinct
// RefreshTokenStore instance (simulating two replicas sharing a distributed
// backend), produce exactly one success and one failure. The CAS layer
// (CompareAndSet on the backend) prevents both replicas from issuing valid
// new tokens for the same old token: a silent rotation-race that the
// process-local keyedMutex alone cannot prevent across replicas.
func TestRefreshTokenStore_Rotate_CASCrossReplicaRace(t *testing.T) {
	ctx := context.Background()

	// Single shared backend: two replicas talking to the same Redis.
	backend := auth.NewMemoryBackend()

	// Two RefreshTokenStore instances = two "replicas", each with its own
	// process-local keyedMutex. They share only the backend data store.
	store1 := auth.NewRefreshTokenStore(backend, "cms")
	store2 := auth.NewRefreshTokenStore(backend, "cms")

	issued, err := store1.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	// Barrier: align both goroutines as tightly as possible so they both
	// read the family record before either writes (the worst-case CAS race).
	var ready, goSync sync.WaitGroup
	ready.Add(2)
	goSync.Add(1)

	var (
		r1 *auth.RotateResult
		e1 error
		r2 *auth.RotateResult
		e2 error
	)

	var done sync.WaitGroup
	done.Add(2)

	go func() {
		defer done.Done()
		ready.Done()
		goSync.Wait()
		r1, e1 = store1.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	}()

	go func() {
		defer done.Done()
		ready.Done()
		goSync.Wait()
		r2, e2 = store2.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	}()

	ready.Wait()
	goSync.Done()

	done.Wait()

	successes := 0
	failures := 0
	if e1 == nil {
		successes++
		require.NotNil(t, r1, "successful rotation must return a result")
	} else {
		failures++
	}
	if e2 == nil {
		successes++
		require.NotNil(t, r2, "successful rotation must return a result")
	} else {
		failures++
	}

	assert.Equal(t, 1, successes, "exactly one replica must succeed")
	assert.Equal(t, 1, failures, "exactly one replica must fail")

	// The failure must be either ErrCASFailed (lost the CAS race) or
	// ErrTokenReuse (saw the updated record and detected the old token
	// in UsedHashes). Both are correct cross-replica outcomes.
	if e1 != nil {
		assert.True(t,
			errors.Is(e1, auth.ErrCASFailed) || errors.Is(e1, auth.ErrTokenReuse),
			"losing replica must get ErrCASFailed or ErrTokenReuse, got: %v", e1,
		)
	}
	if e2 != nil {
		assert.True(t,
			errors.Is(e2, auth.ErrCASFailed) || errors.Is(e2, auth.ErrTokenReuse),
			"losing replica must get ErrCASFailed or ErrTokenReuse, got: %v", e2,
		)
	}

	// After the race, a further rotation with the winner's token works
	// only when the CAS race was pure (both replicas read pre-mutation
	// state). If the loser read after the winner wrote, it detected reuse
	// and revoked the family: a correct but more aggressive outcome.
	// Both are valid cross-replica behaviors. We just verify the family
	// isn't corrupted.
	winner := r1
	if winner == nil {
		winner = r2
	}
	require.NotNil(t, winner, "must have a winning token")
	_, postErr := store1.Rotate(ctx, winner.RefreshToken, 5*time.Minute)
	if postErr != nil {
		require.True(t,
			errors.Is(postErr, auth.ErrFamilyRevoked),
			"post-race rotation with winner's token must succeed or return ErrFamilyRevoked, got: %v", postErr,
		)
	} else {
		// Family is still alive: winner's token is usable.
		t.Log("winner's token usable for another rotation (CAS-only race)")
	}
}
