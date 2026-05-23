package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// Token version invalidation tests
//
// These tests verify that:
//  1. Logout bumps token_version on the user record
//  2. The tokenVersionCheck middleware rejects tokens with a stale tv claim
//  3. Current tv is accepted
//  4. tv=0 (legacy tokens) skips the check (backwards compatible)
//  5. nil getter skips the check
//  6. getter returning 0 skips the check
//  7. End-to-end: logout -> stale token rejected -> fresh token accepted

// - tokenVersionCheck middleware unit tests ------------------

func TestTokenVersionCheck_AcceptsCurrentVersion(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("x", 32)
	uid := uuid.New()

	tok, err := auth.Sign(secret, 3600, uid, "current@test.com", []string{"editor"}, "", 3)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	getter := func(_ context.Context, userID string) (int, error) {
		if userID == uid.String() {
			return 3, nil
		}
		return 0, nil
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	r.Use(tokenVersionCheck(getter))
	called := false
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (current tv must pass)", rr.Code, http.StatusOK)
	}
	if !called {
		t.Error("handler was not called")
	}
}

func TestTokenVersionCheck_RejectsStaleVersion(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("x", 32)
	uid := uuid.New()

	tok, err := auth.Sign(secret, 3600, uid, "stale@test.com", []string{"editor"}, "", 1)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	getter := func(_ context.Context, userID string) (int, error) {
		if userID == uid.String() {
			return 2, nil
		}
		return 0, nil
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	r.Use(tokenVersionCheck(getter))
	called := false
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (stale tv must be rejected)", rr.Code, http.StatusUnauthorized)
	}
	if called {
		t.Error("handler was called despite stale tv")
	}
}

// tv=0 is a real version: a token signed at version 0 must be rejected once
// the user's version has been bumped (e.g. by logout on another device).
func TestTokenVersionCheck_ZeroVersionCheckedAgainstStore(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("x", 32)
	uid := uuid.New()

	tok, err := auth.Sign(secret, 3600, uid, "legacy@test.com", []string{"editor"}, "", 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	getter := func(_ context.Context, userID string) (int, error) {
		return 99, nil
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	r.Use(tokenVersionCheck(getter))
	called := false
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (tv=0 stale vs store 99 must be rejected)", rr.Code, http.StatusUnauthorized)
	}
	if called {
		t.Error("handler was called despite stale tv=0")
	}
}

func TestTokenVersionCheck_NilGetterSkipsCheck(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("x", 32)
	uid := uuid.New()

	tok, err := auth.Sign(secret, 3600, uid, "nil-getter@test.com", []string{"editor"}, "", 1)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	r.Use(tokenVersionCheck(nil))
	called := false
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (nil getter must skip check)", rr.Code, http.StatusOK)
	}
	if !called {
		t.Error("handler was not called")
	}
}

func TestTokenVersionCheck_GetterReturnsZeroSkipsCheck(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("x", 32)
	uid := uuid.New()

	tok, err := auth.Sign(secret, 3600, uid, "zero-getter@test.com", []string{"editor"}, "", 1)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	getter := func(_ context.Context, userID string) (int, error) {
		return 0, nil
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	r.Use(tokenVersionCheck(getter))
	called := false
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (getter returning 0 must skip check)", rr.Code, http.StatusOK)
	}
	if !called {
		t.Error("handler was not called")
	}
}

// Verifies the HTTP response is correct on version mismatch (log output
// can't easily be asserted without a logger buffer).
func TestTokenVersionCheck_LoggedWarningOnMismatch(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("x", 32)
	uid := uuid.New()

	tok, err := auth.Sign(secret, 3600, uid, "warn@test.com", []string{"editor"}, "", 5)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	getter := func(_ context.Context, userID string) (int, error) {
		return 7, nil
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	r.Use(tokenVersionCheck(getter))
	called := false
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if called {
		t.Error("handler was called despite mismatch")
	}
}

// - Logout bumps token version ------------------------------

func TestLogout_BumpsTokenVersion_WithVersionAwareFake(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	user := &domain.User{
		ID:           uid,
		Email:        "tv-bump@test.com",
		PasswordHash: "$2a$10$placeholderhashfortvbumptestXXXXX",
		Roles:        []string{"editor"},
		TokenVersion: 5,
	}

	users := &versionAwareUserStore{
		byEmail: map[string]*domain.User{"tv-bump@test.com": user},
	}

	h := NewAuthHandler(users, nil, nil, "test-secret-32-bytes-minimum-len!!", 3600, true)

	tok, err := auth.Sign(h.jwtSecret, h.expirySecs, user.ID, user.Email, user.Roles, "", user.TokenVersion)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	claims, err := auth.ParseMulti([]string{"test-secret-32-bytes-minimum-len!!"}, tok)
	if err != nil {
		t.Fatalf("ParseMulti: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.Logout(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Logout status = %d, want %d", rr.Code, http.StatusOK)
	}
	if user.TokenVersion != 6 {
		t.Errorf("TokenVersion after logout = %d, want 6", user.TokenVersion)
	}
}

func TestLogout_BumpsTokenVersion_CookiePath(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	user := &domain.User{
		ID:           uid,
		Email:        "tv-cookie@test.com",
		PasswordHash: "$2a$10$placeholderhashfortvcookietestXXXX",
		Roles:        []string{"editor"},
		TokenVersion: 1,
	}

	users := &versionAwareUserStore{
		byEmail: map[string]*domain.User{"tv-cookie@test.com": user},
	}

	h := NewAuthHandler(users, nil, nil, "test-secret-32-bytes-minimum-len!!", 3600, true)

	tok, err := auth.Sign(h.jwtSecret, h.expirySecs, user.ID, user.Email, user.Roles, "", user.TokenVersion)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	claims, err := auth.ParseMulti([]string{"test-secret-32-bytes-minimum-len!!"}, tok)
	if err != nil {
		t.Fatalf("ParseMulti: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	req.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: tok})
	rr := httptest.NewRecorder()
	h.Logout(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Logout status = %d, want %d", rr.Code, http.StatusOK)
	}
	if user.TokenVersion != 2 {
		t.Errorf("TokenVersion after cookie logout = %d, want 2", user.TokenVersion)
	}
}

// - Integration: logout -> stale token rejected --------------

func TestTokenVersion_LogoutInvalidatesJWT(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("x", 32)

	uid := uuid.New()
	user := &domain.User{
		ID:           uid,
		Email:        "tv-integration@test.com",
		PasswordHash: "$2a$10$placeholderhashfortvintegrationXXX",
		Roles:        []string{"editor"},
		TokenVersion: 3,
	}

	users := &versionAwareUserStore{
		byEmail: map[string]*domain.User{"tv-integration@test.com": user},
	}

	getter := tokenVersionGetter(users)

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	r.Use(tokenVersionCheck(getter))
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	// Token at version 3: should pass.
	tokV3, err := auth.Sign(secret, 3600, user.ID, user.Email, user.Roles, "", 3)
	if err != nil {
		t.Fatalf("Sign(v3): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tokV3)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pre-logout request: status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Bump version (simulating logout).
	user.TokenVersion = 4

	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.Header.Set("Authorization", "Bearer "+tokV3)
	rr2 := httptest.NewRecorder()
	r.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("post-logout stale token: status = %d, want %d", rr2.Code, http.StatusUnauthorized)
	}

	// Fresh v4 token: should pass.
	tokV4, err := auth.Sign(secret, 3600, user.ID, user.Email, user.Roles, "", 4)
	if err != nil {
		t.Fatalf("Sign(v4): %v", err)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req3.Header.Set("Authorization", "Bearer "+tokV4)
	rr3 := httptest.NewRecorder()
	r.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusOK {
		t.Errorf("post-logout fresh token: status = %d, want %d", rr3.Code, http.StatusOK)
	}
}

// - Key rotation + token version compat ----------------------

func TestTokenVersion_KeyRotation_RespectsTV(t *testing.T) {
	t.Parallel()
	newSecret := strings.Repeat("y", 32)
	oldSecret := strings.Repeat("x", 32)
	uid := uuid.New()

	tokOld, err := auth.Sign(oldSecret, 3600, uid, "rotate-tv@test.com", []string{"editor"}, "", 2)
	if err != nil {
		t.Fatalf("Sign(old): %v", err)
	}

	getter := func(_ context.Context, userID string) (int, error) {
		if userID == uid.String() {
			return 2, nil
		}
		return 0, nil
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{newSecret, oldSecret}, false))
	r.Use(tokenVersionCheck(getter))
	called := false
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r.Get("/test", func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(w, req) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tokOld)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("key rotation + TV compat: status = %d, want %d", rr.Code, http.StatusOK)
	}
	if !called {
		t.Error("handler was not called")
	}
}

// - Helper: version-aware user store -------------------------

// versionAwareUserStore extends fakeUserStore with a real GetTokenVersion
// and BumpTokenVersion that read and write TokenVersion.
type versionAwareUserStore struct {
	byEmail map[string]*domain.User
	mu      sync.RWMutex
}

func (s *versionAwareUserStore) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byEmail[email]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}

func (s *versionAwareUserStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.byEmail {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *versionAwareUserStore) Count(ctx context.Context) (int64, error) {
	return int64(len(s.byEmail)), nil
}

func (s *versionAwareUserStore) Create(ctx context.Context, email, passwordHash string, roles []string, tenantID string) (*domain.User, error) {
	return nil, errors.New("not implemented")
}

func (s *versionAwareUserStore) CreateFirstAdmin(ctx context.Context, email, passwordHash, tenantID string) (*domain.User, error) {
	return nil, errors.New("not implemented")
}

func (s *versionAwareUserStore) GetTokenVersion(ctx context.Context, id uuid.UUID) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.byEmail {
		if u.ID == id {
			return u.TokenVersion, nil
		}
	}
	return 0, domain.ErrNotFound
}

func (s *versionAwareUserStore) BumpTokenVersion(ctx context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.byEmail {
		if u.ID == id {
			u.TokenVersion++
			return nil
		}
	}
	return domain.ErrNotFound
}

// A database that cannot answer is an outage, not a revocation: answering 401
// would sign every caller out through it. A user who is gone, or a subject
// that is no user id, has no session to keep.
func TestTokenVersionCheck_StatusByCause(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		getter  func(context.Context, string) (int, error)
		want    int
	}{
		{"database unavailable", uuid.NewString(), func(context.Context, string) (int, error) {
			return 0, fmt.Errorf("get token version: %w", errors.New("connection refused"))
		}, http.StatusServiceUnavailable},
		{"user gone", uuid.NewString(), func(context.Context, string) (int, error) {
			return 0, domain.ErrNotFound
		}, http.StatusUnauthorized},
		{"subject not a user id", "auth0|abc", tokenVersionGetter(&versionAwareUserStore{}), http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := tokenVersionCheck(tc.getter)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			claims := makeClaims(uuid.New(), "cause@test.com", []string{"editor"})
			claims.UserID = tc.subject
			ctx := context.WithValue(context.Background(), auth.ClaimsKey, claims)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/test", nil).WithContext(ctx))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
