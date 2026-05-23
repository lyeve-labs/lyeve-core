package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Refresh token rotation with reuse detection (RFC 6749 §10.4, OAuth 2.1).
//
// Storage is any byte-level core.CacheBackend: a plugin can supply a
// distributed one (Redis, Valkey, KeyDB), and the runtime supplies
// MemoryBackend when none does. Core never imports a cache client itself.
//
// Each login starts a token family: the current token hash plus every
// previously used hash. Rotation moves the old hash to the used set and
// issues a new current token. Presenting a used hash again means replay, so
// the whole family is revoked at once, killing every outstanding token for
// that login.
//
// Keys, all under the configured keyspace (e.g. "cms") and TTL'd to the
// token lifetime so the backend self-cleans:
//
//	{ks}:rt:family:{familyID} -> JSON familyRecord
//	{ks}:rt:user:{userID}     -> JSON []string of familyIDs (for revoke-all)
//
// Rotation's read-modify-write is atomic across two layers:
//
//  1. A process-local keyed mutex serializes rotations of the same family
//     within one instance.
// 2. Cross-replica CAS: the record is persisted with
//     CompareAndSet against the pre-mutation snapshot, not a plain Set. If
//     another replica rotated first, the CAS fails with ErrCASFailed and
//     the loser retries or fails. The mutex is an optimization, not the
//     correctness boundary.

// CASBackend is an optional extension to core.CacheBackend that supports atomic
// Compare-And-Set. RefreshTokenStore type-asserts to this interface, and backends
// that implement it get cross-replica rotation-race protection.
// Backends that don't implement CASBackend fall back to plain Set.
type CASBackend interface {
	CompareAndSet(ctx context.Context, key string, oldVal, newVal []byte, ttl time.Duration) error
}

var (
	// ErrCASFailed is returned by CompareAndSet when oldVal does not match
	// the current value at key, or when the key does not exist and oldVal
	// is non-empty. The caller should abort the rotation and re-read state.
	ErrCASFailed = errors.New("compare-and-set failed: value mismatch")
)

var (
	// ErrTokenReuse is returned when a previously-rotated refresh token is
	// presented again. The caller MUST revoke the entire family.
	ErrTokenReuse = errors.New("refresh token reuse detected")

	// ErrTokenExpired means the token or its family has expired.
	ErrTokenExpired = errors.New("refresh token expired")

	// ErrTokenInvalid means the token is unknown or the family is missing.
	ErrTokenInvalid = errors.New("refresh token invalid")

	// ErrFamilyRevoked means the token family was explicitly revoked.
	ErrFamilyRevoked = errors.New("refresh token family revoked")

	// ErrBackendUnavailable means the token store could not be reached, so
	// nothing is known about the token either way. It must never be reported
	// as an invalid token: a client told its token is invalid discards the
	// session, so answering that during a cache outage logs every user out
	// permanently instead of asking them to retry. Callers map it to 503.
	ErrBackendUnavailable = errors.New("refresh token backend unavailable")
)

// maxUsedHashes caps the per-family used-hash set to bound record growth.
const maxUsedHashes = 50

// revokedFamilyTTL keeps a revoked family record alive long enough that a
// rotation attempt receives ErrFamilyRevoked rather than ErrTokenInvalid.
const revokedFamilyTTL = 15 * time.Minute

// expiryGraceTTL extends a family's physical storage TTL past its logical
// expiry (the ExpiresAt field). This lets a rotation of a just-expired token
// find the record and return ErrTokenExpired rather than ErrTokenInvalid.
// After the grace the backend self-cleans the key.
const expiryGraceTTL = 15 * time.Minute

// storageTTL is the physical key TTL for an active family: the token lifetime
// plus a grace window for expiry detection.
func storageTTL(tokenTTL time.Duration) time.Duration {
	return tokenTTL + expiryGraceTTL
}

// familyRecord is the JSON shape persisted to the cache backend for one family.
type familyRecord struct {
	UserID string `json:"user_id"`
	// TenantID is the tenant the session was acting in when the family was
	// issued. One account can hold several tenants, so a refresh that dropped
	// this would quietly move the caller back to their home tenant mid-session.
	// A record without it decodes to "", which means the home tenant.
	TenantID string `json:"tenant_id,omitempty"`
	// TokenVersion is the account's token_version when the family was issued.
	// Logout, disabling the account and setting its password move that
	// version, and a family issued under an older one must not mint a session
	// under the newer one. A record without it decodes to 0, which no account
	// holds, so such a family is refused once and the user signs in again.
	TokenVersion int       `json:"tv,omitempty"`
	CurrentHash  string    `json:"current_hash"`
	UsedHashes   []string  `json:"used_hashes"`
	Gen          int       `json:"gen"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Revoked      bool      `json:"revoked"`
}

// RefreshTokenStore manages refresh token families over a core.CacheBackend.
type RefreshTokenStore struct {
	backend  core.CacheBackend
	keyspace string // prefix for all keys (e.g. "cms")
	locks    *keyedMutex
}

// NewRefreshTokenStore creates a RefreshTokenStore over the given cache backend.
// keyspace is the prefix for all keys (e.g. "cms" -> "cms:rt:family:...").
//
// The backend is owned by the caller: the store never closes it (the cache
// plugin or runtime owns the backend lifecycle).
func NewRefreshTokenStore(backend core.CacheBackend, keyspace string) *RefreshTokenStore {
	return &RefreshTokenStore{
		backend:  backend,
		keyspace: keyspace,
		locks:    newKeyedMutex(),
	}
}

// Close is a no-op: the store does not own the backend. It exists to satisfy
// callers that defer Close() on an optional component.
func (s *RefreshTokenStore) Close() error { return nil }

// unreachable reports whether a failed Get means the store could not be
// reached rather than the key being absent.
//
// core.CacheBackend.Get does not distinguish the two, and the backend is
// supplied by a plugin, so the error value cannot be interpreted here. Ask the
// backend instead: one that still answers Ping is up, and the Get that failed
// against it was a miss. Only the miss path pays the extra round trip.
func (s *RefreshTokenStore) unreachable(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return s.Ping(ctx) != nil
}

// Ping reports whether the backing store is reachable, for health probing. It
// prefers an optional Ping method on the backend and otherwise performs a
// lightweight write to a sentinel key.
func (s *RefreshTokenStore) Ping(ctx context.Context) error {
	if p, ok := s.backend.(interface {
		Ping(context.Context) error
	}); ok {
		return p.Ping(ctx)
	}
	return s.backend.Set(ctx, s.keyspace+":rt:health", []byte("1"), 30*time.Second)
}

// Issue: create a new token family on login.

// IssueResult is returned by Issue and passed to the HTTP handler.
type IssueResult struct {
	RefreshToken string
	FamilyID     string
	ExpiresAt    time.Time
}

// Issue creates a new token family for userID in that account's home tenant.
// ttl is the refresh token lifetime (not the access token lifetime).
func (s *RefreshTokenStore) Issue(ctx context.Context, userID string, ttl time.Duration) (*IssueResult, error) {
	return s.IssueForTenant(ctx, userID, "", ttl)
}

// IssueForTenant creates a new token family for userID acting in tenantID. An
// empty tenantID means the account's home tenant.
func (s *RefreshTokenStore) IssueForTenant(ctx context.Context, userID, tenantID string, ttl time.Duration) (*IssueResult, error) {
	return s.IssueForSession(ctx, userID, tenantID, 0, ttl)
}

// IssueForSession creates a new token family for userID acting in tenantID,
// bound to the account's token_version at the time. Refresh refuses the
// family once that version has moved on.
func (s *RefreshTokenStore) IssueForSession(ctx context.Context, userID, tenantID string, tokenVersion int, ttl time.Duration) (*IssueResult, error) {
	familyID := uuid.New().String()
	token, tokenHash, err := generateRefreshToken(familyID)
	if err != nil {
		return nil, fmt.Errorf("refresh token issue: %w", err)
	}

	now := time.Now()
	expires := now.Add(ttl)

	rec := familyRecord{
		UserID:       userID,
		TenantID:     tenantID,
		TokenVersion: tokenVersion,
		CurrentHash:  tokenHash,
		UsedHashes:   []string{},
		Gen:          0,
		IssuedAt:     now,
		ExpiresAt:    expires,
		Revoked:      false,
	}

	unlock := s.locks.lock(familyID)
	if err := s.saveFamily(ctx, familyID, &rec, storageTTL(ttl)); err != nil {
		unlock()
		return nil, fmt.Errorf("refresh token issue: %w", err)
	}
	unlock()

	// Track family membership for revoke-all. Best-effort: a failure here does
	// not invalidate the issued token, only the ability to bulk-revoke later.
	if err := s.addFamilyToUser(ctx, userID, familyID, storageTTL(ttl)); err != nil {
		return nil, fmt.Errorf("refresh token issue: index family: %w", err)
	}

	return &IssueResult{
		RefreshToken: token,
		FamilyID:     familyID,
		ExpiresAt:    expires,
	}, nil
}

// Rotate: exchange an old refresh token for a new one.

// RotateResult is returned by Rotate.
type RotateResult struct {
	RefreshToken string
	FamilyID     string
	UserID       string
	TenantID     string
	// TokenVersion is the token_version the family was issued under.
	TokenVersion int
	ExpiresAt    time.Time
}

// Rotate validates oldToken and returns a new refresh token. If oldToken was
// already rotated (reuse detection), the family is revoked and ErrTokenReuse is
// returned.
func (s *RefreshTokenStore) Rotate(ctx context.Context, oldToken string, ttl time.Duration) (*RotateResult, error) {
	familyID, oldHash, err := parseRefreshToken(oldToken)
	if err != nil {
		return nil, fmt.Errorf("refresh token rotate: %w", err)
	}

	newToken, newHash, err := generateRefreshToken(familyID)
	if err != nil {
		return nil, fmt.Errorf("refresh token rotate: %w", err)
	}

	unlock := s.locks.lock(familyID)
	defer unlock()

	rec, err := s.loadFamily(ctx, familyID)
	if err != nil {
		return nil, err
	}
	if rec.Revoked {
		return nil, ErrFamilyRevoked
	}
	if !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt) {
		return nil, ErrTokenExpired
	}

	// Exact match against the current token -> normal rotation.
	if rec.CurrentHash == oldHash {
		// Snapshot the pre-mutation record for CAS cross-replica safety
		// Two replicas that read the same state will both
		// attempt CAS. Only one wins.
		oldBytes, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("refresh token rotate: %w", err)
		}

		rec.UsedHashes = append(rec.UsedHashes, oldHash)
		if len(rec.UsedHashes) > maxUsedHashes {
			rec.UsedHashes = rec.UsedHashes[len(rec.UsedHashes)-maxUsedHashes:]
		}
		rec.CurrentHash = newHash
		rec.Gen++
		rec.IssuedAt = time.Now()

		newBytes, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("refresh token rotate: %w", err)
		}

		if err := s.casSave(ctx, familyID, oldBytes, newBytes, storageTTL(ttl)); err != nil {
			return nil, fmt.Errorf("refresh token rotate: %w", err)
		}
		return &RotateResult{
			RefreshToken: newToken,
			FamilyID:     familyID,
			UserID:       rec.UserID,
			TenantID:     rec.TenantID,
			TokenVersion: rec.TokenVersion,
			ExpiresAt:    rec.ExpiresAt,
		}, nil
	}

	// Old token is in the used set: reuse detected. Revoke the whole family.
	for _, h := range rec.UsedHashes {
		if h == oldHash {
			rec.Revoked = true
			rec.CurrentHash = ""
			if saveErr := s.saveFamily(ctx, familyID, rec, revokedFamilyTTL); saveErr != nil {
				slog.Error("refresh token: persist reuse revocation failed", "family_id", familyID, "err", saveErr)
			}
			return nil, ErrTokenReuse
		}
	}

	// No match anywhere -> invalid token.
	return nil, ErrTokenInvalid
}

// Revoke: explicit logout / revoke-all.

// RevokeFamily marks a single family as revoked (logout from one device).
func (s *RefreshTokenStore) RevokeFamily(ctx context.Context, familyID string) error {
	unlock := s.locks.lock(familyID)
	defer unlock()
	return s.revokeFamilyLocked(ctx, familyID)
}

// RevokeAllForUser revokes every active refresh token family for the user
// (logout-all-devices / password change).
func (s *RefreshTokenStore) RevokeAllForUser(ctx context.Context, userID string) error {
	families, err := s.loadUserFamilies(ctx, userID)
	if err != nil {
		return fmt.Errorf("refresh token revoke-all: %w", err)
	}
	// Revoking every family is the point of this call, so a family that could
	// not be revoked has to reach the caller. An operator cutting off a
	// compromised account must never hear it is done while a session works.
	var failed int
	for _, fid := range families {
		unlock := s.locks.lock(fid)
		if err := s.revokeFamilyLocked(ctx, fid); err != nil {
			slog.Error("refresh token: revoke family failed", "family_id", fid, "err", err)
			failed++
		}
		unlock()
	}
	if failed > 0 {
		return fmt.Errorf("refresh token revoke-all: %w: %d of %d families still live",
			ErrBackendUnavailable, failed, len(families))
	}
	// Clean up the membership index.
	if err := s.backend.Delete(ctx, s.userKey(userID)); err != nil {
		slog.Warn("refresh token: cleanup user family index failed", "user_id", userID, "err", err)
	}
	return nil
}

// revokeFamilyLocked marks a family as revoked. The caller must hold the
// family lock. The record stays alive with revoked=1 for revokedFamilyTTL so
// future rotation attempts receive ErrFamilyRevoked rather than ErrTokenInvalid.
func (s *RefreshTokenStore) revokeFamilyLocked(ctx context.Context, familyID string) error {
	rec, err := s.loadFamily(ctx, familyID)
	if errors.Is(err, ErrTokenInvalid) {
		// Nothing to revoke (already gone / never existed).
		return nil
	}
	if err != nil {
		// The store is unreachable, so the family may well still be live.
		// Reporting success here would tell an operator a session was cut
		// off when it was not.
		return err
	}
	rec.Revoked = true
	rec.CurrentHash = ""
	return s.saveFamily(ctx, familyID, rec, revokedFamilyTTL)
}

// Backend helpers

// loadFamily reads a family record. It returns ErrTokenInvalid when the family
// is absent or unreadable, and ErrBackendUnavailable when the store itself
// could not answer, so an outage is never reported as an invalid token.
func (s *RefreshTokenStore) loadFamily(ctx context.Context, familyID string) (*familyRecord, error) {
	b, err := s.backend.Get(ctx, s.familyKey(familyID))
	if err != nil {
		if s.unreachable(ctx, err) {
			return nil, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
		return nil, ErrTokenInvalid
	}
	if len(b) == 0 {
		return nil, ErrTokenInvalid
	}
	var rec familyRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, ErrTokenInvalid
	}
	return &rec, nil
}

func (s *RefreshTokenStore) saveFamily(ctx context.Context, familyID string, rec *familyRecord, ttl time.Duration) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode family: %w", err)
	}
	return s.backend.Set(ctx, s.familyKey(familyID), b, ttl)
}

// casSave persists a family record atomically via CompareAndSet when the
// backend supports it (CASBackend interface), falling back to a plain Set
// otherwise. oldBytes is the serialized state the caller read before mutation,
// and newBytes is the serialized state after mutation. Returns ErrCASFailed when
// the backend CAS detects a conflicting write (cross-replica race), wrapped
// so the caller can unwrap with errors.Is.
func (s *RefreshTokenStore) casSave(ctx context.Context, familyID string, oldBytes, newBytes []byte, ttl time.Duration) error {
	if cas, ok := s.backend.(CASBackend); ok {
		if err := cas.CompareAndSet(ctx, s.familyKey(familyID), oldBytes, newBytes, ttl); err != nil {
			return err // may be ErrCASFailed
		}
		return nil
	}
	// Backend doesn't support CAS: fall back to plain Set.
	return s.backend.Set(ctx, s.familyKey(familyID), newBytes, ttl)
}

// addFamilyToUser appends familyID to the user's membership set (deduplicated)
// and refreshes its TTL.
func (s *RefreshTokenStore) addFamilyToUser(ctx context.Context, userID, familyID string, ttl time.Duration) error {
	unlock := s.locks.lock("user:" + userID)
	defer unlock()

	families, err := s.loadUserFamilies(ctx, userID)
	if err != nil {
		return err
	}
	for _, fid := range families {
		if fid == familyID {
			return nil // already present
		}
	}
	families = append(families, familyID)
	b, err := json.Marshal(families)
	if err != nil {
		return fmt.Errorf("encode user families: %w", err)
	}
	return s.backend.Set(ctx, s.userKey(userID), b, ttl)
}

// loadUserFamilies reads a user's family index. A miss yields an empty slice
// with no error. An unreachable store is
// reported, not flattened to an empty index: revoke-all reading "no families"
// from a failed Get would answer success while every session stayed live.
func (s *RefreshTokenStore) loadUserFamilies(ctx context.Context, userID string) ([]string, error) {
	b, err := s.backend.Get(ctx, s.userKey(userID))
	if err != nil {
		if s.unreachable(ctx, err) {
			return nil, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
		return nil, nil
	}
	if len(b) == 0 {
		return nil, nil
	}
	var families []string
	if err := json.Unmarshal(b, &families); err != nil {
		// Corrupt index: treat as empty rather than failing revoke-all.
		return nil, nil
	}
	return families, nil
}

// Key helpers

func (s *RefreshTokenStore) familyKey(familyID string) string {
	return s.keyspace + ":rt:family:" + familyID
}

func (s *RefreshTokenStore) userKey(userID string) string {
	return s.keyspace + ":rt:user:" + userID
}

// Keyed mutex: serializes operations on a single key (family or user index)
// within this process. Reference-counted so the lock map stays bounded.

type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refLock
}

type refLock struct {
	mu   sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*refLock)}
}

// lock acquires the mutex for key and returns an unlock function. Callers must
// not hold two keyed locks simultaneously (no nested locking) to avoid deadlock.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	rl, ok := k.locks[key]
	if !ok {
		rl = &refLock{}
		k.locks[key] = rl
	}
	rl.refs++
	k.mu.Unlock()

	rl.mu.Lock()
	return func() {
		rl.mu.Unlock()
		k.mu.Lock()
		rl.refs--
		if rl.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// Token generation and parsing

// refreshTokenSep is the delimiter between family ID and random value in the
// opaque refresh token string.
const refreshTokenSep = "."

// generateRefreshToken creates an opaque refresh token and its SHA-256 hash.
// Format: {familyID}.{base64url(32 random bytes)}
func generateRefreshToken(familyID string) (token, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	random := base64.RawURLEncoding.EncodeToString(buf)
	token = familyID + refreshTokenSep + random
	h := sha256.Sum256([]byte(token))
	hash = hex.EncodeToString(h[:])
	return token, hash, nil
}

// RefreshTokenFamilyID extracts the family ID and SHA-256 hash from an opaque
// refresh token without a backend call.
func RefreshTokenFamilyID(token string) (familyID, hash string, err error) {
	return parseRefreshToken(token)
}

// parseRefreshToken extracts the family ID and SHA-256 hash from an opaque
// refresh token.
func parseRefreshToken(token string) (familyID, hash string, err error) {
	idx := -1
	for i := range token {
		if token[i] == refreshTokenSep[0] {
			// The family ID is a 36-char UUID, so the separator must sit at index 36.
			if i == 36 {
				idx = i
				break
			}
		}
	}
	if idx < 0 || idx >= len(token)-1 {
		return "", "", fmt.Errorf("invalid refresh token format")
	}
	familyID = token[:idx]
	if _, err := uuid.Parse(familyID); err != nil {
		return "", "", fmt.Errorf("invalid family ID in refresh token: %w", err)
	}
	h := sha256.Sum256([]byte(token))
	return familyID, hex.EncodeToString(h[:]), nil
}
