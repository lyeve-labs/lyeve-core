package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

const csrfCookieName = "__Host-csrf"

// userStore is the subset of db.UserStore methods that AuthHandler uses.
// Satisfied by *db.UserStore (production) and test fakes.
type userStore interface {
	GetByEmail(ctx context.Context, email string) (*domain.User, error)
	GetByTenantProviderEmail(ctx context.Context, tenantID, address string) (*domain.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
	Count(ctx context.Context) (int64, error)
	Create(ctx context.Context, email, passwordHash string, roles []string, tenantID string) (*domain.User, error)
	CreateFirstAdmin(ctx context.Context, email, passwordHash, tenantID string) (*domain.User, error)
	GetTokenVersion(ctx context.Context, id uuid.UUID) (int, error)
	BumpTokenVersion(ctx context.Context, id uuid.UUID) error
}

// AuthHandler handles login, logout, setup, /me, and token refresh.
type AuthHandler struct {
	users              userStore
	memberships        core.MembershipReader // optional: cross-tenant membership grants
	mfaStore           security.MFAStore
	refreshTokenStore  *auth.RefreshTokenStore     // optional: refresh token rotation
	deviceRiskAssessor security.DeviceRiskAssessor // optional: device fingerprinting
	lockoutChecker     LockoutChecker              // optional: account lockout enforcement
	mfaLockoutChecker  MFALockoutChecker           // always wired. MFA-specific lockout
	pool               db.DB
	jwtSecret          string
	expirySecs         int64
	secureCook         bool                 // true in production (HTTPS)
	passwordHashAlgo   string               // "bcrypt" or "argon2id"
	passwordPolicy     auth.PasswordPolicy  // password complexity rules
	refreshTokenTTL    time.Duration        // TTL for refresh tokens (default 7 days)
	keyStore           *encryption.KeyStore // optional: per-tenant DEK encryption

	// providerWindows answers whether a provider account is inside its
	// tenant's window. Nil applies the default window.
	providerWindows core.ProviderWindowProvider

	// lockoutShared is set when both lockouts live in a backend every replica
	// shares, which changes what a lockout log line can promise an operator.
	lockoutShared bool

	// LockoutConfig controls account lockout behavior.
	MaxFailedAttempts int           // max failed logins before lockout (default 5)
	LockoutWindow     time.Duration // window for counting failures (default 15m)

	// riskBasedMFADisabled, when true, suppresses MFA escalation driven by the
	// device-risk assessor (e.g. "new device" scoring). Set only by in-package
	// tests to isolate risk-driven escalation. Users who have enrolled MFA are
	// STILL challenged. This relaxes risk escalation alone.
	riskBasedMFADisabled bool

	// multiTenant is set when the engine runs MULTI_TENANT=true. First-run
	// setup uses it to provision a default tenant so the bootstrap super_admin
	// has a home instead of being stamped with an empty tenant_id.
	multiTenant bool

	// The tenant roster is supplied by a plugin through core.DefaultTenantFunc
	// and core.TenantValidatorFunc: one registers the implicit default tenant
	// at first-run setup, one tells whether a slug is on the roster. Both are
	// nil when nothing supplies a roster.
	defaultTenant   core.DefaultTenantFunc
	tenantValidator core.TenantValidatorFunc

	// setupToken authorizes first-run setup. Nil refuses every claim.
	setupToken *SetupToken
}

// NewAuthHandler constructs an AuthHandler with the given user store, MFA
// store, DB pool, JWT secret, token expiry, cookie security flag, and optional
// password hash algorithm (default "bcrypt").
func NewAuthHandler(users userStore, mfaStore security.MFAStore, pool db.DB, jwtSecret string, expirySecs int64, secureCook bool, passwordHashAlgo ...string) *AuthHandler {
	algo := "bcrypt"
	if len(passwordHashAlgo) > 0 && passwordHashAlgo[0] != "" {
		algo = passwordHashAlgo[0]
	}
	maxFailedAttempts, lockoutWindow := lockoutPolicy()
	var windows core.ProviderWindowProvider
	if pool != nil {
		windows = config.NewProviderWindows(pool)
	}
	return &AuthHandler{
		providerWindows: windows,
		users:           users, mfaStore: mfaStore, pool: pool,
		jwtSecret: jwtSecret, expirySecs: expirySecs,
		secureCook: secureCook, passwordHashAlgo: algo,
		passwordPolicy:    auth.DefaultPasswordPolicy(),
		refreshTokenTTL:   7 * 24 * time.Hour,
		MaxFailedAttempts: maxFailedAttempts,
		LockoutWindow:     lockoutWindow,
		mfaLockoutChecker: processMFALockout{mem: NewInMemoryMFALockout(0, 0)},
		lockoutChecker:    NewInMemoryLockout(0, 0),
	}
}

// WithRefreshTokenStore enables refresh token rotation with reuse detection.
func (h *AuthHandler) WithRefreshTokenStore(store *auth.RefreshTokenStore, ttl time.Duration) {
	h.refreshTokenStore = store
	if ttl > 0 {
		h.refreshTokenTTL = ttl
	}
}

// WithDeviceRiskAssessor enables device fingerprinting risk assessment on login.
func (h *AuthHandler) WithDeviceRiskAssessor(ra security.DeviceRiskAssessor) {
	h.deviceRiskAssessor = ra
}

// WithMultiTenant marks the handler as running a multi-tenant deployment. It
// makes first-run setup provision a default tenant, so the bootstrap
// super_admin is stamped with a real tenant_id instead of an empty one.
func (h *AuthHandler) WithMultiTenant(v bool) {
	h.multiTenant = v
}

// WithTenantRegistry hands the handler the two roster operations first-run
// setup and login need. Both come from whichever plugin keeps the tenant
// roster, and both are left nil on an install without one.
func (h *AuthHandler) WithTenantRegistry(ensure core.DefaultTenantFunc, validate core.TenantValidatorFunc) {
	h.defaultTenant = ensure
	h.tenantValidator = validate
}

// WithSetupToken sets the credential POST /api/admin/setup demands. Left
// unset, setup refuses every caller.
func (h *AuthHandler) WithSetupToken(t *SetupToken) {
	h.setupToken = t
}

// LockoutChecker is an optional interface for checking account lockout status.
// A plugin may implement it to track failed logins and enforce a threshold.
type LockoutChecker interface {
	// CountRecentFailures returns the number of failed login attempts for
	// the given user within the specified window.
	CountRecentFailures(ctx context.Context, userID uuid.UUID, since time.Time) (int, error)
	// ReserveAttempt counts one password attempt for the user before the
	// password is checked, in one step with reading the count, and returns
	// the window's count. It reports false and counts nothing once the window
	// holds limit attempts. A check that read the count and recorded a
	// failure afterwards let every concurrent request that read before the
	// first failure landed through to the password check.
	ReserveAttempt(ctx context.Context, userID uuid.UUID, limit int) (int, bool, error)
	// ResetFailedAttempts clears the user's count after a correct password,
	// which also returns the attempt that password reserved.
	ResetFailedAttempts(ctx context.Context, userID uuid.UUID) error
}

// WithLockoutChecker wires an optional lockout checker. When set, the Login
// handler reserves an attempt before password verification and refuses once
// the threshold is reached.
func (h *AuthHandler) WithLockoutChecker(lc LockoutChecker) {
	h.lockoutChecker = lc
}

// WithSharedLockoutBackend moves the password and MFA lockouts into backend,
// which every replica of the deployment shares. The attempt limits then hold
// across replicas and survive a restart, and a used MFA challenge is refused
// on every replica. A nil backend keeps the in-process lockouts.
func (h *AuthHandler) WithSharedLockoutBackend(backend core.CacheBackend) {
	if backend == nil {
		return
	}
	h.lockoutChecker = NewSharedLockout(backend, h.LockoutWindow)
	h.mfaLockoutChecker = NewSharedMFALockout(backend, 0, 0)
	h.lockoutShared = true
}

// clearPasswordAttempts clears the account's lockout count after a correct
// password, which also returns the attempt that password reserved. A clear
// that fails is logged and changes no answer.
func (h *AuthHandler) clearPasswordAttempts(ctx context.Context, userID uuid.UUID) {
	if h.lockoutChecker == nil {
		return
	}
	if err := h.lockoutChecker.ResetFailedAttempts(ctx, userID); err != nil {
		slog.WarnContext(ctx, "failed to clear lockout attempts", "err", err, "user_id", userID)
	}
}

// releaseMFAAttempt gives back the code attempt a request reserved when the
// store failed before the code could be judged, so an outage never counts a
// code against its owner. A release that fails is logged.
func (h *AuthHandler) releaseMFAAttempt(ctx context.Context, userID uuid.UUID, r Reservation) {
	if err := h.mfaLockoutChecker.ReleaseMFAAttempt(ctx, userID, r); err != nil {
		slog.WarnContext(ctx, "failed to release mfa attempt", "err", err, "user_id", userID)
	}
}

// lockoutScopeNote says where a lockout lives, for the lines that report one.
func (h *AuthHandler) lockoutScopeNote() string {
	if h.lockoutShared {
		return "the response is identical to a wrong password by design; the lock is in the shared cache backend and holds on every replica"
	}
	return "the response is identical to a wrong password by design; the lock is in-process and clears on restart"
}

// WithKeyStore wires the per-tenant DEK encryption KeyStore.
// When set, MFAVerify decrypts TOTP secrets using core/encryption.DecryptValue
// with a per-tenant DEK instead of the deprecated auth.DecryptSecret.
func (h *AuthHandler) WithKeyStore(ks *encryption.KeyStore) {
	h.keyStore = ks
}

// WithPasswordPolicy overrides the default password policy.
func (h *AuthHandler) WithPasswordPolicy(p auth.PasswordPolicy) {
	h.passwordPolicy = p
}

// collectFingerprint extracts the device fingerprint signals from an HTTP request.
// Callers should run this AFTER proxy IP resolution (middleware.ClientAddress).
func collectFingerprint(r *http.Request) security.DeviceFingerprint {
	ip := reqparse.ClientIP(r)
	// Build a stable header hash from fingerprint-relevant headers.
	var headerParts []string
	for _, k := range []string{"Accept", "Accept-Language", "Accept-Encoding"} {
		if v := r.Header.Get(k); v != "" {
			headerParts = append(headerParts, k+"="+v)
		}
	}
	for _, k := range []string{"Sec-CH-UA", "Sec-CH-UA-Platform", "Sec-CH-UA-Mobile"} {
		if v := r.Header.Get(k); v != "" {
			headerParts = append(headerParts, k+"="+v)
		}
	}
	headerHash := ""
	if len(headerParts) > 0 {
		h := sha256.Sum256([]byte(strings.Join(headerParts, "|")))
		headerHash = hex.EncodeToString(h[:])
	}

	return security.DeviceFingerprint{
		UserAgent:      r.UserAgent(),
		IP:             ip,
		TLSFingerprint: r.Header.Get("X-TLS-Fingerprint"),
		HeaderHash:     headerHash,
	}
}

// SetupStatus reports whether initial setup has been completed.
