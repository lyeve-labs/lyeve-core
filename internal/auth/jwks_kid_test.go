package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ParseExternal: kid-based key resolution.

func TestParseExternal_KidMatch(t *testing.T) {
	keyD, privD := genRSA2048(t)
	keyE, _ := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "d", pub: keyD},
		{kid: "e", pub: keyE},
	})

	tokenStr := signExternalJWT(t, privD, "d", "user-d@test.com", srv.URL, security.JWTAudience)

	claims, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "user-d@test.com", claims.Email)
}

func TestParseExternal_UnknownKidMultiKey(t *testing.T) {
	keyD, _ := genRSA2048(t)
	keyE, _ := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "d", pub: keyD},
		{kid: "e", pub: keyE},
	})

	_, privX := genRSA2048(t)
	tokenStr := signExternalJWT(t, privX, "x", "user-x@test.com", srv.URL, security.JWTAudience)

	_, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no matching key for kid "x"`)
}

func TestParseExternal_NoKidSingleKey(t *testing.T) {
	keyA, privA := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "a", pub: keyA},
	})

	tokenStr := signExternalJWTNoKid(t, privA, "user-a@test.com", srv.URL, security.JWTAudience)

	claims, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "user-a@test.com", claims.Email)
}

func TestParseExternal_UnknownKidSingleKey(t *testing.T) {
	keyA, privA := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "a", pub: keyA},
	})

	// Sign with key "a" but put kid "z" in header. Single-key JWKS -> accepted.
	tokenStr := signExternalJWT(t, privA, "z", "user-z@test.com", srv.URL, security.JWTAudience)

	claims, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "user-z@test.com", claims.Email)
}

func TestParseExternal_WronglySignedUnknownKid(t *testing.T) {
	// Token signed with key "d" but claims kid "x" (unknown in JWKS).
	keyD, privD := genRSA2048(t)
	keyE, _ := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "d", pub: keyD},
		{kid: "e", pub: keyE},
	})

	tokenStr := signExternalJWT(t, privD, "x", "user-x@test.com", srv.URL, security.JWTAudience)

	_, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no matching key for kid "x"`)
}

// ParseExternal: iss/aud validation, real rejection tests

func TestParseExternal_ValidIssuerAndAudience(t *testing.T) {
	keyA, privA := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "a", pub: keyA},
	})

	tokenStr := signExternalJWT(t, privA, "a", "user@test.com", srv.URL, security.JWTAudience)
	claims, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "user@test.com", claims.Email)
	assert.Equal(t, srv.URL, claims.Issuer)
	assert.Equal(t, jwt.ClaimStrings{security.JWTAudience}, claims.Audience)
}

func TestParseExternal_RejectsWrongIssuer(t *testing.T) {
	keyA, privA := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "a", pub: keyA},
	})

	tokenStr := signExternalJWT(t, privA, "a", "user@test.com", "http://evil-issuer", security.JWTAudience)
	_, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iss") // jwt/v5 error mentions "iss"
}

func TestParseExternal_RejectsWrongAudience(t *testing.T) {
	keyA, privA := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "a", pub: keyA},
	})

	tokenStr := signExternalJWT(t, privA, "a", "user@test.com", srv.URL, "wrong-service")
	_, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aud") // jwt/v5 error mentions "aud"
}

func TestParseExternal_RejectsEmptyIssuer(t *testing.T) {
	keyA, privA := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "a", pub: keyA},
	})

	tokenStr := signExternalJWT(t, privA, "a", "user@test.com", "", security.JWTAudience)
	_, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.Error(t, err)
}

func TestParseExternal_RejectsMissingAudience(t *testing.T) {
	keyA, privA := genRSA2048(t)

	srv := jwksServer(t, []rsaJWK{
		{kid: "a", pub: keyA},
	})

	tokenStr := signExternalJWT(t, privA, "a", "user@test.com", srv.URL, "")
	_, err := ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.Error(t, err)
}

// helpers

type rsaJWK struct {
	kid string
	pub *rsa.PublicKey
}

func genRSA2048(t *testing.T) (*rsa.PublicKey, *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return &priv.PublicKey, priv
}

func jwksServer(t *testing.T, keys []rsaJWK) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type jwkResp struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		}
		var set struct {
			Keys []jwkResp `json:"keys"`
		}
		for _, k := range keys {
			eInt := k.pub.E
			set.Keys = append(set.Keys, jwkResp{
				Kty: "RSA",
				Kid: k.kid,
				Alg: "RS256",
				Use: "sig",
				N:   base64.RawURLEncoding.EncodeToString(k.pub.N.Bytes()),
				E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(eInt)).Bytes()),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
}

func signExternalJWT(t *testing.T, priv *rsa.PrivateKey, kid, email, issuer, audience string) string {
	t.Helper()
	now := time.Now()
	var aud jwt.ClaimStrings
	if audience != "" {
		aud = jwt.ClaimStrings{audience}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, Claims{
		UserID: "external-user",
		Email:  email,
		Roles:  []string{"user"},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "external-user",
			Issuer:    issuer,
			Audience:  aud,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	})
	tok.Header["kid"] = kid
	tokenStr, err := tok.SignedString(priv)
	require.NoError(t, err)
	return tokenStr
}

func signExternalJWTNoKid(t *testing.T, priv *rsa.PrivateKey, email, issuer, audience string) string {
	t.Helper()
	now := time.Now()
	var aud jwt.ClaimStrings
	if audience != "" {
		aud = jwt.ClaimStrings{audience}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, Claims{
		UserID: "external-user",
		Email:  email,
		Roles:  []string{"user"},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "external-user",
			Issuer:    issuer,
			Audience:  aud,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	})
	tokenStr, err := tok.SignedString(priv)
	require.NoError(t, err)
	return tokenStr
}

// jwksURLFor returns a JWKS URL unique to the calling test. ParseExternal
// caches keys by URL for an hour in a package-level map, and httptest binds an
// ephemeral port the OS is free to hand to a later server. Two tests sharing a
// bare srv.URL can therefore read each other's cached keys, which turns a
// deliberately broken endpoint into a silent success.
func jwksURLFor(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return srv.URL + "/" + t.Name()
}
