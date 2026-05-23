// Every login path other than the password mints its session through
// SignSessionToken, so the token it signs has to carry the account's tenant
// and token version.
//
// A token with no tenant claim resolves to no tenant, which a multi-tenant
// install refuses. A token with no version reads as version zero, so any
// account whose version has ever been bumped would reject its own fresh
// session on the very next request.
package enginehost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

func sessionFactsRoundTrip(t *testing.T, pool db.DB) {
	t.Helper()
	ctx := context.Background()

	// roles is TEXT[] on PostgreSQL and JSON on the other two.
	emptyRoles := "[]"
	if pool.Engine() == "postgres" {
		emptyRoles = "{}"
	}

	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, tenant_id, token_version)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, "session-facts-"+userID.String()[:8]+"@example.com", "x", emptyRoles, "acme", 4,
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	host := &engineHost{
		cfg:   &config.Config{JWTSecret: jwtTestSecret, JWTExpirySecs: 3600},
		pool:  pool,
		rawDB: pool.SQLDB(),
	}

	token, err := host.SignSessionToken(ctx, userID, "session-facts@example.com", []string{"editor"})
	if err != nil {
		t.Fatalf("SignSessionToken: %v", err)
	}

	claims, err := security.ParseJWT(jwtTestSecret, token)
	if err != nil {
		t.Fatalf("ParseJWT: %v", err)
	}
	if claims.TenantID != "acme" {
		t.Errorf("tenant_id = %q, want %q", claims.TenantID, "acme")
	}

	parsed, err := parseTokenVersion(token)
	if err != nil {
		t.Fatalf("read tv claim: %v", err)
	}
	if parsed != 4 {
		t.Errorf("tv = %d, want 4", parsed)
	}

	// The password login refuses a disabled or expired account, and so must
	// every path that signs through here.
	for _, tc := range []struct {
		name, set string
		args      []any
	}{
		{"disabled", `UPDATE sys_users SET disabled = $1 WHERE id = $2`, []any{true, userID}},
		{"expired", `UPDATE sys_users SET disabled = $1, expires_at = $2 WHERE id = $3`, []any{false, time.Now().Add(-time.Hour).UTC(), userID}},
		{"erased", `UPDATE sys_users SET expires_at = NULL, anonymized = $1 WHERE id = $2`, []any{true, userID}},
	} {
		if _, err := pool.Exec(ctx, tc.set, tc.args...); err != nil {
			t.Fatalf("%s: mark account: %v", tc.name, err)
		}
		tok, err := host.SignSessionToken(ctx, userID, "session-facts@example.com", []string{"editor"})
		if !errors.Is(err, core.ErrAccountInactive) {
			t.Errorf("%s: SignSessionToken error = %v, want ErrAccountInactive", tc.name, err)
		}
		if tok != "" {
			t.Errorf("%s: a token was signed for an inactive account", tc.name)
		}
	}

	// No row is no account to sign for.
	if _, err := host.SignSessionToken(ctx, uuid.New(), "nobody@example.com", nil); !errors.Is(err, core.ErrAccountInactive) {
		t.Errorf("unknown account: SignSessionToken error = %v, want ErrAccountInactive", err)
	}

	// An expiry still ahead is an active account.
	if _, err := pool.Exec(ctx, `UPDATE sys_users SET anonymized = $1, expires_at = $2 WHERE id = $3`, false, time.Now().Add(time.Hour).UTC(), userID); err != nil {
		t.Fatalf("future expiry: %v", err)
	}
	if _, err := host.SignSessionToken(ctx, userID, "session-facts@example.com", nil); err != nil {
		t.Errorf("an account expiring later was refused: %v", err)
	}
}

func TestSessionFacts_Postgres(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("skipping postgres (CI_DIALECT restriction)")
	}
	sessionFactsRoundTrip(t, testdb.Postgres(t))
}

func TestSessionFacts_MySQL(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("skipping mysql (CI_DIALECT restriction)")
	}
	sessionFactsRoundTrip(t, testdb.MySQL(t))
}

func TestSessionFacts_MSSQL(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("skipping mssql (CI_DIALECT restriction)")
	}
	sessionFactsRoundTrip(t, testdb.MSSQL(t))
}

// parseTokenVersion reads the "tv" claim straight off the payload. ParseJWT
// returns the shared claim struct, which does not carry it.
func parseTokenVersion(token string) (int, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("token has %d segments, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, err
	}
	var payload struct {
		TokenVersion int `json:"tv"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, err
	}
	return payload.TokenVersion, nil
}
