//go:build !mutest

package api

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// seedUserToken writes an account holding role and returns a session token for
// it, so a router test can reach a gated route as that role.
func seedUserToken(t *testing.T, ctx context.Context, pool db.DB, cfg *config.Config, email, role string) string {
	t.Helper()
	userID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles) VALUES ($1, $2, $3, $4)`,
		userID, email, "$2a$10$placeholder-hash-for-test", `{"`+role+`"}`)
	require.NoError(t, err)

	now := time.Now()
	claims := auth.Claims{
		UserID:       userID.String(),
		TokenVersion: 1,
		Email:        email,
		TokenType:    "session",
		TenantID:     "acme",
		Roles:        []string{role},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.New().String(),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{security.JWTAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.JWTSecret))
	require.NoError(t, err)
	return signed
}
