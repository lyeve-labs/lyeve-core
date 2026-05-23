package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// An unreachable token store must stay distinguishable from an absent family,
// or a cache outage answers "invalid token" on every refresh and each client
// discards its session. These pin the two apart.

var errBackendDown = errors.New("dial tcp: connection refused")

// failingBackend serves reads and writes until it is broken, then fails every
// call the way an unreachable cache does.
type failingBackend struct {
	inner  core.CacheBackend
	broken bool
}

func (f *failingBackend) Get(ctx context.Context, key string) ([]byte, error) {
	if f.broken {
		return nil, errBackendDown
	}
	return f.inner.Get(ctx, key)
}

func (f *failingBackend) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if f.broken {
		return errBackendDown
	}
	return f.inner.Set(ctx, key, val, ttl)
}

func (f *failingBackend) Delete(ctx context.Context, key string) error {
	if f.broken {
		return errBackendDown
	}
	return f.inner.Delete(ctx, key)
}

func (f *failingBackend) Flush(ctx context.Context) error {
	if f.broken {
		return errBackendDown
	}
	return f.inner.Flush(ctx)
}

func newFailingStore(t *testing.T) (*RefreshTokenStore, *failingBackend) {
	t.Helper()
	fb := &failingBackend{inner: NewMemoryBackend()}
	return NewRefreshTokenStore(fb, "test"), fb
}

func TestRotate_BackendOutageIsNotAnInvalidToken(t *testing.T) {
	ctx := context.Background()
	s, fb := newFailingStore(t)

	issued, err := s.Issue(ctx, "user-1", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	fb.broken = true

	_, err = s.Rotate(ctx, issued.RefreshToken, time.Hour)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("Rotate during outage = %v, want ErrBackendUnavailable", err)
	}
	if errors.Is(err, ErrTokenInvalid) {
		t.Error("Rotate reported the token invalid; the client would discard a live session")
	}

	// The token is still good once the store comes back.
	fb.broken = false
	if _, err := s.Rotate(ctx, issued.RefreshToken, time.Hour); err != nil {
		t.Errorf("Rotate after recovery = %v, want success", err)
	}
}

func TestRotate_UnknownTokenIsStillInvalid(t *testing.T) {
	ctx := context.Background()
	s, _ := newFailingStore(t)

	issued, err := s.Issue(ctx, "user-1", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := s.RevokeFamily(ctx, issued.FamilyID); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}

	// A family that genuinely is not usable must not read as an outage.
	_, err = s.Rotate(ctx, issued.RefreshToken, time.Hour)
	if errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("Rotate on a revoked family = %v, want a token error", err)
	}
}

func TestRevokeAllForUser_ReportsAnOutageRatherThanSuccess(t *testing.T) {
	ctx := context.Background()
	s, fb := newFailingStore(t)

	if _, err := s.Issue(ctx, "user-1", time.Hour); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	fb.broken = true

	// Revoking every session is a security control, so an outage must reach the
	// operator rather than read as success.
	if err := s.RevokeAllForUser(ctx, "user-1"); !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("RevokeAllForUser during outage = %v, want ErrBackendUnavailable", err)
	}
}

func TestRevokeAllForUser_SucceedsWhenTheStoreAnswers(t *testing.T) {
	ctx := context.Background()
	s, _ := newFailingStore(t)

	issued, err := s.Issue(ctx, "user-1", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := s.RevokeAllForUser(ctx, "user-1"); err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}
	if _, err := s.Rotate(ctx, issued.RefreshToken, time.Hour); !errors.Is(err, ErrFamilyRevoked) {
		t.Errorf("Rotate after revoke-all = %v, want ErrFamilyRevoked", err)
	}
}

// pingingBackend answers reachability directly, the way a Redis client does,
// and counts writes so a test can prove the store did not probe with one.
type pingingBackend struct {
	failingBackend
	sets  int
	pings int
}

func (p *pingingBackend) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	p.sets++
	return p.failingBackend.Set(ctx, key, val, ttl)
}

func (p *pingingBackend) Ping(ctx context.Context) error {
	p.pings++
	if p.broken {
		return errBackendDown
	}
	return nil
}

func newPingingStore() (*RefreshTokenStore, *pingingBackend) {
	pb := &pingingBackend{failingBackend: failingBackend{inner: NewMemoryBackend()}}
	return NewRefreshTokenStore(pb, "test"), pb
}

// Ping is the cheap way to tell a miss from an outage. Falling back to a Set
// turns every unknown token into a write, so a client replaying stale tokens
// drives writes into the store it is failing to read from.
func TestRotate_ReachabilityIsSettledByPingNotAWrite(t *testing.T) {
	ctx := context.Background()
	s, pb := newPingingStore()

	issued, err := s.Issue(ctx, "user-1", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	pb.broken = true
	writes := pb.sets

	if _, err := s.Rotate(ctx, issued.RefreshToken, time.Hour); !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("Rotate during outage = %v, want ErrBackendUnavailable", err)
	}
	if pb.pings == 0 {
		t.Error("store never called Ping; it fell back to probing with a write")
	}
	if pb.sets != writes {
		t.Errorf("store wrote %d key(s) to test reachability, want 0", pb.sets-writes)
	}
}

func TestRotate_UnknownTokenDoesNotWriteToTheStore(t *testing.T) {
	ctx := context.Background()
	s, pb := newPingingStore()

	issued, err := s.Issue(ctx, "user-1", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := s.RevokeFamily(ctx, issued.FamilyID); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}

	writes := pb.sets
	if _, err := s.Rotate(ctx, issued.RefreshToken, time.Hour); errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("Rotate on a revoked family = %v, want a token error", err)
	}
	if pb.sets != writes {
		t.Errorf("store wrote %d key(s) for a token it could not find, want 0", pb.sets-writes)
	}
}
