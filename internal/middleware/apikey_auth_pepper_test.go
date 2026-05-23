package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// fakeKeyStore is an in-memory key store: it maps a stored hash -> claims, and
// supports the upgrade-on-use rewrite.
type fakeKeyStore struct {
	mu       sync.Mutex
	byHash   map[string]*core.AuthClaims
	upgrades [][2]string // recorded (oldHash, newHash) upgrade calls
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{byHash: map[string]*core.AuthClaims{}}
}

func (f *fakeKeyStore) lookup(_ context.Context, hash string) (*core.AuthClaims, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byHash[hash], nil
}

func (f *fakeKeyStore) upgrade(_ context.Context, oldHash, newHash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.byHash[oldHash]; ok {
		delete(f.byHash, oldHash)
		f.byHash[newHash] = c
	}
	f.upgrades = append(f.upgrades, [2]string{oldHash, newHash})
}

func (f *fakeKeyStore) upgradeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upgrades)
}

// claimsCapture is a downstream handler that records the claims it observed.
type claimsCapture struct{ got *core.AuthClaims }

func (c *claimsCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.got = core.GetClaims(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func doRequest(t *testing.T, h http.Handler, rawKey string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	if rawKey != "" {
		req.Header.Set("X-API-Key", rawKey)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestAPIKeyAuth_NoPepper_PlainLookup(t *testing.T) {
	if security.APIKeyPepperConfigured() {
		t.Fatal("precondition: no pepper expected")
	}
	const raw = "ly_plainkey"
	store := newFakeKeyStore()
	store.byHash[security.HashKey(raw)] = &core.AuthClaims{UserID: "k1", IsAPIKey: true}

	cap := &claimsCapture{}
	mw := middleware.APIKeyAuth(store.lookup)
	doRequest(t, mw(cap.handler()), raw)

	if cap.got == nil || cap.got.UserID != "k1" {
		t.Fatalf("claims not resolved without pepper: %+v", cap.got)
	}
}

func TestAPIKeyAuth_Peppered_DirectHit(t *testing.T) {
	security.SetAPIKeyPepper([]byte("pep"))
	t.Cleanup(func() { security.SetAPIKeyPepper(nil) })

	const raw = "ly_newkey"
	store := newFakeKeyStore()
	// A key minted under the pepper is stored under the peppered hash.
	store.byHash[security.HashKeyPeppered(raw)] = &core.AuthClaims{UserID: "k2", IsAPIKey: true}

	cap := &claimsCapture{}
	mw := middleware.APIKeyAuthWithUpgrade(store.lookup, store.upgrade)
	doRequest(t, mw(cap.handler()), raw)

	if cap.got == nil || cap.got.UserID != "k2" {
		t.Fatalf("peppered key not resolved on direct hit: %+v", cap.got)
	}
	if n := store.upgradeCount(); n != 0 {
		t.Fatalf("upgrade called %d times for a non-legacy key, want 0", n)
	}
}

func TestAPIKeyAuth_Peppered_LegacyFallbackAndUpgrade(t *testing.T) {
	security.SetAPIKeyPepper([]byte("pep"))
	t.Cleanup(func() { security.SetAPIKeyPepper(nil) })

	const raw = "ly_legacykey"
	legacyHash := security.HashKey(raw)
	pepperedHash := security.HashKeyPeppered(raw)
	if legacyHash == pepperedHash {
		t.Fatal("test setup invalid: legacy and peppered hashes coincide")
	}

	store := newFakeKeyStore()
	// A pre-pepper key exists only under the plain SHA-256 hash.
	store.byHash[legacyHash] = &core.AuthClaims{UserID: "k3", IsAPIKey: true}

	mw := middleware.APIKeyAuthWithUpgrade(store.lookup, store.upgrade)

	// First request: peppered miss -> legacy hit -> claims resolved + upgrade.
	cap1 := &claimsCapture{}
	doRequest(t, mw(cap1.handler()), raw)
	if cap1.got == nil || cap1.got.UserID != "k3" {
		t.Fatalf("legacy key not resolved via fallback: %+v", cap1.got)
	}
	if store.upgradeCount() != 1 {
		t.Fatalf("upgrade calls = %d, want 1", store.upgradeCount())
	}
	if _, stillLegacy := store.byHash[legacyHash]; stillLegacy {
		t.Fatal("legacy hash not migrated after upgrade")
	}
	if _, peppered := store.byHash[pepperedHash]; !peppered {
		t.Fatal("peppered hash not present after upgrade")
	}

	// Second request: now a direct peppered hit, no further fallback/upgrade.
	cap2 := &claimsCapture{}
	doRequest(t, mw(cap2.handler()), raw)
	if cap2.got == nil || cap2.got.UserID != "k3" {
		t.Fatalf("upgraded key not resolved on second request: %+v", cap2.got)
	}
	if store.upgradeCount() != 1 {
		t.Fatalf("upgrade called again after migration: count = %d, want 1", store.upgradeCount())
	}
}

func TestAPIKeyAuth_UnknownKey_PassesThroughUnauthenticated(t *testing.T) {
	security.SetAPIKeyPepper([]byte("pep"))
	t.Cleanup(func() { security.SetAPIKeyPepper(nil) })

	store := newFakeKeyStore() // empty
	cap := &claimsCapture{}
	mw := middleware.APIKeyAuthWithUpgrade(store.lookup, store.upgrade)
	doRequest(t, mw(cap.handler()), "ly_unknown")

	if cap.got != nil {
		t.Fatalf("unknown key resolved to claims: %+v", cap.got)
	}
	if store.upgradeCount() != 0 {
		t.Fatalf("upgrade called for unknown key: count = %d", store.upgradeCount())
	}
}
