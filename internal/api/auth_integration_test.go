//go:build !mutest

// Package api: auth integration tests using testcontainers.
//
// These tests require a real PostgreSQL database because AuthHandler.Login
// and Token call db.UserStore.GetByEmail which needs a real *sql.DB
// connection. Input-validation tests that pass nil stores are in
// auth_endpoint_test.go.
//
// Build tag: unit tests (no DB) run by default. Integration tests require tag.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// Integration test helpers

// seedUser inserts a user directly into the database and returns the plain
// password for later login attempts.
func seedUser(t *testing.T, pool db.DB, email, password string, roles []string) *struct {
	ID    uuid.UUID
	Email string
} {
	t.Helper()

	hash, err := auth.HashPassword("bcrypt", password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	var id uuid.UUID
	row, qrErr := pool.QueryRow(context.Background(),
		`INSERT INTO sys_users (email, password_hash, roles) VALUES ($1, $2, $3) RETURNING id`,
		email, hash, "{"+strings.Join(roles, ",")+"}",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	return &struct {
		ID    uuid.UUID
		Email string
	}{ID: id, Email: email}
}

// seedMFA enables TOTP for a user: encrypts a TOTP secret, stores it.
// Returns the plaintext secret so tests can generate valid codes.
func seedMFA(t *testing.T, pool db.DB, userID uuid.UUID, jwtSecret string) (plainSecret string) {
	t.Helper()

	rawSecret, _, err := auth.GenerateTOTP("test@mfa.dev", nil)
	if err != nil {
		t.Fatalf("generate TOTP: %v", err)
	}

	//lint:ignore SA1019 EncryptSecret kept for backward compat with existing encrypted TOTP secrets.
	encSecret, err := auth.EncryptSecret(rawSecret, jwtSecret)
	if err != nil {
		t.Fatalf("encrypt TOTP secret: %v", err)
	}

	// The MFA store's tables belong to whatever supplies the store, so the
	// test creates its own for simpleMFAStore to read.
	_, err = pool.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS test_mfa_totp (
			user_id UUID PRIMARY KEY,
			encrypted_secret TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT true
		)`)
	if err != nil {
		t.Fatalf("create mfa table: %v", err)
	}

	// The backup codes table, read the same way.
	_, err = pool.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS test_mfa_backup_codes (
			id SERIAL PRIMARY KEY,
			user_id UUID NOT NULL REFERENCES test_mfa_totp(user_id),
			code_hash TEXT NOT NULL,
			used_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
	if err != nil {
		t.Fatalf("create mfa backup codes table: %v", err)
	}

	_, err = pool.Exec(context.Background(),
		`INSERT INTO test_mfa_totp (user_id, encrypted_secret, enabled) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id) DO UPDATE SET encrypted_secret = $2, enabled = $3`,
		userID, encSecret, true,
	)
	if err != nil {
		t.Fatalf("seed MFA: %v", err)
	}

	return rawSecret
}

// LOGIN: Success (Postgres)

func TestAuthIntegration_Login_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	user := seedUser(t, pool, "login-test@example.com", "correct-horse-battery-staple", []string{"editor"})

	users := db.NewUserStore(pool)
	h := NewAuthHandler(users, nil, pool, secret, 3600, false)

	body := makeLoginBody("login-test@example.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
	}

	resp := decodeAuthResponse(t, rr)
	if resp["token"] == nil || resp["token"] == "" {
		t.Error("response should contain a token")
	}

	// Verify the issued token parses as a valid session JWT.
	tokenStr, _ := resp["token"].(string)
	claims, err := auth.Parse(secret, tokenStr)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	if claims.UserID != user.ID.String() {
		t.Errorf("token UserID = %q, want %q", claims.UserID, user.ID.String())
	}
	if claims.Email != "login-test@example.com" {
		t.Errorf("token Email = %q, want login-test@example.com", claims.Email)
	}

	_ = user
}

// LOGIN: Wrong password (Postgres)

func TestAuthIntegration_Login_WrongPassword(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	seedUser(t, pool, "wrong-pw@example.com", "correct-password", []string{"editor"})

	users := db.NewUserStore(pool)
	h := NewAuthHandler(users, nil, pool, secret, 3600, false)

	body := makeLoginBody("wrong-pw@example.com", "WRONG-PASSWORD")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (wrong password must be rejected)", rr.Code, http.StatusUnauthorized)
	}
}

// LOGIN: MFA required (Postgres)

func TestAuthIntegration_Login_MFARequired(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	u := seedUser(t, pool, "mfa-login@example.com", "correct-password", []string{"editor"})

	_ = seedMFA(t, pool, u.ID, secret)

	users := db.NewUserStore(pool)
	h := NewAuthHandler(users, &simpleMFAStore{pool: pool, secret: secret}, pool, secret, 3600, false)

	body := makeLoginBody("mfa-login@example.com", "correct-password")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
	}

	resp := decodeAuthResponse(t, rr)
	if mfa, ok := resp["mfa_required"].(bool); !ok || !mfa {
		t.Fatalf("expected mfa_required=true when MFA is enabled, got resp=%v", resp)
	}
	if resp["challenge_token"] == nil || resp["challenge_token"] == "" {
		t.Fatal("response should contain challenge_token when MFA is required")
	}
	if resp["token"] != nil {
		t.Error("response must NOT contain session token when MFA is required")
	}
}

// LOGIN: Risk-based MFA escalation vs the test-only suppression (Postgres)

// forceMFARiskAssessor is a fake DeviceRiskAssessor that always demands
// step-up MFA, the way an assessor can score a fresh, unrecognized device.
type forceMFARiskAssessor struct{}

func (forceMFARiskAssessor) Assess(context.Context, uuid.UUID, security.DeviceFingerprint) (*security.RiskAssessment, error) {
	return &security.RiskAssessment{Level: security.RiskMedium, Score: 35, Reason: "new device", RequireMFA: true}, nil
}
func (forceMFARiskAssessor) RecordLogin(context.Context, uuid.UUID, security.DeviceFingerprint, bool) error {
	return nil
}
func (forceMFARiskAssessor) TrustDevice(context.Context, uuid.UUID, security.DeviceFingerprint, string) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (forceMFARiskAssessor) UntrustDevice(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (forceMFARiskAssessor) ListTrustedDevices(context.Context, uuid.UUID, int, int) ([]security.TrustedDevice, int, error) {
	return nil, 0, nil
}

func TestAuthIntegration_Login_RiskMFASuppression(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"
	seedUser(t, pool, "risk-mfa@example.com", "correct-password", []string{"editor"})
	users := db.NewUserStore(pool)

	login := func(h *AuthHandler) map[string]any {
		body := makeLoginBody("risk-mfa@example.com", "correct-password")
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Login(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
		}
		return decodeAuthResponse(t, rr)
	}

	// An account with nothing enrolled cannot answer a challenge: mfa-verify
	// has no secret to check and every enrollment route sits behind the session
	// the challenge withholds. Escalating to a factor the user does not hold
	// only locks them out, so the session is issued and the risk logged.
	t.Run("risk escalation without an enrolled factor allows the login", func(t *testing.T) {
		h := NewAuthHandler(users, nil, pool, secret, 3600, false)
		h.WithDeviceRiskAssessor(forceMFARiskAssessor{})
		resp := login(h)
		if mfa, _ := resp["mfa_required"].(bool); mfa {
			t.Fatalf("unsatisfiable challenge issued to an account with no enrolled factor: %v", resp)
		}
		if tok, _ := resp["token"].(string); tok == "" {
			t.Fatal("expected a session token when the challenge could never be answered")
		}
	})

	// The same escalation must still bite when the user CAN answer it.
	t.Run("risk escalation challenges an enrolled account", func(t *testing.T) {
		u := seedUser(t, pool, "risk-enrolled@example.com", "correct-password", []string{"editor"})
		_ = seedMFA(t, pool, u.ID, secret)
		h := NewAuthHandler(users, &simpleMFAStore{pool: pool, secret: secret}, pool, secret, 3600, false)
		h.WithDeviceRiskAssessor(forceMFARiskAssessor{})
		body := makeLoginBody("risk-enrolled@example.com", "correct-password")
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Login(rr, req)
		resp := decodeAuthResponse(t, rr)
		if mfa, _ := resp["mfa_required"].(bool); !mfa {
			t.Fatalf("expected mfa_required=true for an enrolled account, got %v", resp)
		}
		if resp["token"] != nil {
			t.Error("must not issue a session token when an answerable challenge is pending")
		}
	})

	// Bypass ON: risk escalation suppressed -> session issued, no MFA challenge.
	t.Run("risk escalation suppressed", func(t *testing.T) {
		h := NewAuthHandler(users, nil, pool, secret, 3600, false)
		h.WithDeviceRiskAssessor(forceMFARiskAssessor{})
		h.WithRiskBasedMFADisabled(true)
		resp := login(h)
		if mfa, _ := resp["mfa_required"].(bool); mfa {
			t.Fatalf("risk-based MFA should be suppressed, got mfa_required=true: %v", resp)
		}
		if tok, _ := resp["token"].(string); tok == "" {
			t.Fatal("expected a session token when risk escalation is suppressed")
		}
	})

	// Bypass ON but user ENROLLED MFA: still challenged. The flag only
	// relaxes risk escalation, never a real enrolled second factor.
	t.Run("enrolled MFA still enforced under bypass", func(t *testing.T) {
		u := seedUser(t, pool, "enrolled-mfa@example.com", "correct-password", []string{"editor"})
		_ = seedMFA(t, pool, u.ID, secret)
		h := NewAuthHandler(users, &simpleMFAStore{pool: pool, secret: secret}, pool, secret, 3600, false)
		h.WithDeviceRiskAssessor(forceMFARiskAssessor{})
		h.WithRiskBasedMFADisabled(true)
		body := makeLoginBody("enrolled-mfa@example.com", "correct-password")
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Login(rr, req)
		resp := decodeAuthResponse(t, rr)
		if mfa, _ := resp["mfa_required"].(bool); !mfa {
			t.Fatalf("enrolled MFA must still be enforced under suppression, got %v", resp)
		}
	})
}

// MFA VERIFY: Valid TOTP (Postgres)

func TestAuthIntegration_MFAVerify_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	u := seedUser(t, pool, "mfa-verify@example.com", "correct-password", []string{"editor"})
	plainSecret := seedMFA(t, pool, u.ID, secret)

	users := db.NewUserStore(pool)
	mfaStore := &simpleMFAStore{pool: pool, secret: secret}
	h := NewAuthHandler(users, mfaStore, pool, secret, 3600, false)

	// Step 1: Login to get challenge token.
	loginBody := makeLoginBody("mfa-verify@example.com", "correct-password")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)

	resp := decodeAuthResponse(t, rr)
	challengeToken, ok := resp["challenge_token"].(string)
	if !ok || challengeToken == "" {
		t.Fatalf("no challenge token in response: %v", resp)
	}

	// Step 2: Generate a valid TOTP code.
	code, err := totp.GenerateCode(plainSecret, time.Now())
	if err != nil {
		t.Fatalf("generate TOTP code: %v", err)
	}

	// Step 3: Submit MFA verify.
	verifyBody, _ := json.Marshal(map[string]string{
		"challenge_token": challengeToken,
		"code":            code,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(string(verifyBody)))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()

	h.MFAVerify(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rr2.Code, http.StatusOK, rr2.Body.String())
	}

	resp2 := decodeAuthResponse(t, rr2)
	tokenStr, ok := resp2["token"].(string)
	if !ok || tokenStr == "" {
		t.Fatal("MFA verify should return a full session token")
	}

	// Verify the token is NOT an MFA-pending token.
	claims, err := auth.Parse(secret, tokenStr)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	if claims.MFAPending {
		t.Error("issued token must NOT have MFAPending=true")
	}
	if claims.UserID != u.ID.String() {
		t.Errorf("UserID = %q, want %q", claims.UserID, u.ID.String())
	}
}

// MFA VERIFY: Backup code (Postgres)

func TestAuthIntegration_MFAVerify_BackupCode(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	u := seedUser(t, pool, "mfa-backup@example.com", "correct-password", []string{"editor"})

	_ = seedMFA(t, pool, u.ID, secret)

	// Insert a backup code hash for the seeded user.
	backupCode := "ABCD-EFGH-IJKL-MNOP"
	codeHash := sha256.Sum256([]byte(backupCode))
	codeHashHex := hex.EncodeToString(codeHash[:])

	_, err := pool.Exec(context.Background(),
		`INSERT INTO test_mfa_backup_codes (user_id, code_hash) VALUES ($1, $2)`,
		u.ID, codeHashHex,
	)
	if err != nil {
		t.Fatalf("seed backup code: %v", err)
	}

	users := db.NewUserStore(pool)
	mfaStore := &simpleMFAStore{pool: pool, secret: secret}
	h := NewAuthHandler(users, mfaStore, pool, secret, 3600, false)

	// Step 1: Get challenge token.
	loginBody := makeLoginBody("mfa-backup@example.com", "correct-password")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)

	resp := decodeAuthResponse(t, rr)
	challengeToken, ok := resp["challenge_token"].(string)
	if !ok || challengeToken == "" {
		t.Fatalf("no challenge token: %v", resp)
	}

	// Step 2: Submit backup code.
	verifyBody, _ := json.Marshal(map[string]string{
		"challenge_token": challengeToken,
		"code":            backupCode,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(string(verifyBody)))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()

	h.MFAVerify(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body=%q", rr2.Code, http.StatusOK, rr2.Body.String())
	}
}

// MFA VERIFY: Invalid TOTP code (Postgres)

func TestAuthIntegration_MFAVerify_WrongCode(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	u := seedUser(t, pool, "mfa-wrong@example.com", "correct-password", []string{"editor"})
	_ = seedMFA(t, pool, u.ID, secret)

	users := db.NewUserStore(pool)
	mfaStore := &simpleMFAStore{pool: pool, secret: secret}
	h := NewAuthHandler(users, mfaStore, pool, secret, 3600, false)

	// Get challenge token.
	loginBody := makeLoginBody("mfa-wrong@example.com", "correct-password")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)

	resp := decodeAuthResponse(t, rr)
	challengeToken, ok := resp["challenge_token"].(string)
	if !ok || challengeToken == "" {
		t.Fatalf("no challenge token: %v", resp)
	}

	verifyBody, _ := json.Marshal(map[string]string{
		"challenge_token": challengeToken,
		"code":            "000000",
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(string(verifyBody)))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()

	h.MFAVerify(rr2, req2)

	if rr2.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d for wrong TOTP code", rr2.Code, http.StatusUnprocessableEntity)
	}
}

// TOKEN ENDPOINT: Success (Postgres)

func TestAuthIntegration_Token_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	seedUser(t, pool, "token-test@example.com", "correct-password", []string{"editor"})

	users := db.NewUserStore(pool)
	h := NewAuthHandler(users, nil, pool, secret, 3600, false)

	body := makeLoginBody("token-test@example.com", "correct-password")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Token(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
	}

	resp := decodeAuthResponse(t, rr)
	if resp["token"] == nil || resp["token"] == "" {
		t.Error("token response missing token")
	}
	if ei, ok := resp["expires_in"].(float64); !ok || ei != 3600 {
		t.Errorf("expires_in = %v, want 3600", resp["expires_in"])
	}

	// Token endpoint must NOT set a session cookie (Bearer-only protocol).
	for _, c := range rr.Result().Cookies() {
		if c.Name == security.SessionCookieNameInsecure {
			t.Error("Token endpoint must NOT set session cookie")
		}
	}
}

// Minimal MFAStore backed by real DB

type simpleMFAStore struct {
	pool   db.DB
	secret string
	steps  totpStepLog
}

func (s *simpleMFAStore) ConsumeTOTPStep(_ context.Context, userID uuid.UUID, step int64) (bool, error) {
	return s.steps.consume(userID, step), nil
}

func (s *simpleMFAStore) IsEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	var enabled bool
	row, qrErr := s.pool.QueryRow(ctx,
		`SELECT enabled FROM test_mfa_totp WHERE user_id = $1`,
		userID,
	)
	if qrErr != nil {
		return false, qrErr
	}
	err := row.Scan(&enabled)
	if err != nil {
		return false, nil
	}
	return enabled, nil
}

func (s *simpleMFAStore) GetEnabled(ctx context.Context, userID uuid.UUID) (string, []string, error) {
	var encSecret string
	row, qrErr := s.pool.QueryRow(ctx,
		`SELECT encrypted_secret FROM test_mfa_totp WHERE user_id = $1 AND enabled = true`,
		userID,
	)
	if qrErr != nil {
		return "", nil, qrErr
	}
	err := row.Scan(&encSecret)
	if err != nil {
		return "", nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT code_hash FROM test_mfa_backup_codes WHERE user_id = $1 AND used_at IS NULL`,
		userID,
	)
	if err != nil {
		return encSecret, nil, nil
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err == nil {
			hashes = append(hashes, h)
		}
	}
	return encSecret, hashes, rows.Err()
}

func (s *simpleMFAStore) HasWebAuthn(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, nil
}

func (s *simpleMFAStore) DisableByAdmin(ctx context.Context, targetUserID, adminUserID uuid.UUID, reason string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM test_mfa_totp WHERE user_id = $1`, targetUserID)
	return err
}

func (s *simpleMFAStore) GracePeriodHours() int { return 0 }

func (s *simpleMFAStore) RegenerateBackupCodes(ctx context.Context, userID uuid.UUID, n int) ([]string, error) {
	return nil, nil
}

func (s *simpleMFAStore) UpdateBackupCodes(ctx context.Context, userID uuid.UUID, hashes []string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM test_mfa_backup_codes WHERE user_id = $1`, userID)
	return err
}

// A bearer token issued before logout must stop authenticating the moment
// logout bumps the user's token_version, well before the token's natural
// expiry. The middleware only compares versions when the token carries a tv
// claim, and the token can only carry one when the user read that feeds
// signing returns the live column. An in-memory user store with the version
// set by hand cannot see a real store that drops the column, so this test
// runs against the real one.
func TestAuthIntegration_LogoutRevokesOutstandingAccessToken(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	pool := testdb.Postgres(t)
	secret := "test-secret-at-least-32-bytes-long!!"

	seedUser(t, pool, "logout-revoke@example.com", "correct-horse-battery-staple", []string{"editor"})

	users := db.NewUserStore(pool)
	h := NewAuthHandler(users, nil, pool, secret, 3600, false)

	body := makeLoginBody("logout-revoke@example.com", "correct-horse-battery-staple")
	loginReq := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	h.Login(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body=%q", loginRec.Code, loginRec.Body.String())
	}
	token, _ := decodeAuthResponse(t, loginRec)["token"].(string)
	if token == "" {
		t.Fatal("login returned no token")
	}

	claims, err := auth.Parse(secret, token)
	if err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	if claims.TokenVersion < 1 {
		t.Fatalf("issued token carries tv %d, want at least 1", claims.TokenVersion)
	}

	protected := jwtAuth([]string{secret}, false)(tokenVersionCheck(tokenVersionGetter(users))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))

	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call(); code != http.StatusOK {
		t.Fatalf("pre-logout status = %d, want 200", code)
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", strings.NewReader("{}"))
	logoutReq.Header.Set("Authorization", "Bearer "+token)
	logoutRec := httptest.NewRecorder()
	jwtAuth([]string{secret}, false)(http.HandlerFunc(h.Logout)).ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200; body=%q", logoutRec.Code, logoutRec.Body.String())
	}

	if code := call(); code != http.StatusUnauthorized {
		t.Errorf("replay after logout status = %d, want 401", code)
	}
}

// TestSetup_MultiTenant_RegistersTheDefaultTenant verifies that a first-run
// setup on a multi-tenant deployment stamps the bootstrap super_admin with the
// default tenant and asks the roster to register it.
//
// The roster belongs to whatever supplies it, so the engine owes the call,
// not the row.
func TestSetup_MultiTenant_RegistersTheDefaultTenant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	run := func(t *testing.T, ensure core.DefaultTenantFunc) (*httptest.ResponseRecorder, *db.UserStore) {
		t.Helper()
		pool := testdb.Postgres(t)
		users := db.NewUserStore(pool)
		h := NewAuthHandler(users, nil, pool, "test-secret-at-least-32-bytes-long!!", 3600, false)
		h.WithMultiTenant(true)
		h.WithSetupToken(NewSetupTokenFromEnv("operator-setup-token-0123"))
		h.WithTenantRegistry(ensure, nil)

		req := httptest.NewRequest(http.MethodPost, "/api/admin/setup",
			strings.NewReader(makeLoginBody("admin@test.com", "CorrectHorseBatteryStaple1!")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(SetupTokenHeader, "operator-setup-token-0123")
		rr := httptest.NewRecorder()
		h.Setup(rr, req)
		return rr, users
	}

	t.Run("the roster is asked to register it", func(t *testing.T) {
		asked := 0
		rr, users := run(t, func(context.Context) error { asked++; return nil })
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup status = %d, want %d; body=%q", rr.Code, http.StatusCreated, rr.Body.String())
		}
		if asked != 1 {
			t.Errorf("roster asked %d times, want 1", asked)
		}
		u, err := users.GetByEmail(context.Background(), "admin@test.com")
		if err != nil {
			t.Fatalf("get bootstrap admin: %v", err)
		}
		if u.TenantID != "default" {
			t.Errorf("bootstrap admin tenant_id = %q, want %q", u.TenantID, "default")
		}
	})

	// An install with no roster supplier has no roster at all. Setup still
	// has to produce a usable account, because refusing here would leave the
	// operator with no way in.
	t.Run("no roster still creates the account", func(t *testing.T) {
		rr, users := run(t, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup status = %d, want %d; body=%q", rr.Code, http.StatusCreated, rr.Body.String())
		}
		u, err := users.GetByEmail(context.Background(), "admin@test.com")
		if err != nil {
			t.Fatalf("get bootstrap admin: %v", err)
		}
		if u.TenantID != "default" {
			t.Errorf("bootstrap admin tenant_id = %q, want %q", u.TenantID, "default")
		}
	})

	// A roster that refuses stops setup, rather than leaving an account scoped
	// to a tenant nobody registered.
	t.Run("a roster that refuses stops setup", func(t *testing.T) {
		rr, _ := run(t, func(context.Context) error { return errors.New("roster is not writable") })
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("setup status = %d, want %d; body=%q", rr.Code, http.StatusServiceUnavailable, rr.Body.String())
		}
	})
}
