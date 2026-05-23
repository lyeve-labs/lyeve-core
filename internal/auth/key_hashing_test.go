package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/ssrf"
)

// TestMain allows loopback (127.0.0.1) so httptest.NewServer-based tests work
// through the SSRF-safe HTTP client. Production SSRF protection is unchanged.
func TestMain(m *testing.M) {
	_ = ssrf.AddAllowlist("127.0.0.1/32")
	os.Exit(m.Run())
}

// OAuth: PKCE, State, BuildAuthURL

func TestGeneratePKCE_Format(t *testing.T) {
	verifier, challenge, err := security.GeneratePKCE()
	require.NoError(t, err)
	assert.NotEmpty(t, verifier)
	assert.NotEmpty(t, challenge)

	// Verifier is 43 base64url chars (32 random bytes).
	assert.Len(t, verifier, 43)
	assert.Regexp(t, `^[A-Za-z0-9_-]{43}$`, verifier)

	// Challenge is 43 base64url chars (SHA-256 of verifier).
	assert.Len(t, challenge, 43)
	assert.Regexp(t, `^[A-Za-z0-9_-]{43}$`, challenge)

	// Verifier and challenge differ.
	assert.NotEqual(t, verifier, challenge)

	// Challenge is SHA-256(verifier).
	expectedChallenge := computeS256Challenge(verifier)
	assert.Equal(t, expectedChallenge, challenge, "challenge must be S256(verifier)")
}

func TestGeneratePKCE_Unique(t *testing.T) {
	verifiers := make(map[string]bool, 50)
	for i := 0; i < 50; i++ {
		v, _, err := security.GeneratePKCE()
		require.NoError(t, err)
		assert.False(t, verifiers[v], "duplicate PKCE verifier")
		verifiers[v] = true
	}
}

func TestGenerateState_Format(t *testing.T) {
	state, err := security.GenerateState()
	require.NoError(t, err)
	// 16 random bytes -> 22 base64url chars (no padding).
	assert.Len(t, state, 22)
	assert.Regexp(t, `^[A-Za-z0-9_-]{22}$`, state)
}

func TestGenerateState_Unique(t *testing.T) {
	states := make(map[string]bool, 50)
	for i := 0; i < 50; i++ {
		s, err := security.GenerateState()
		require.NoError(t, err)
		assert.False(t, states[s], "duplicate state token")
		states[s] = true
	}
}

func TestBuildAuthURL_ConstructsCorrectURL(t *testing.T) {
	url := security.BuildAuthURL(
		"https://accounts.example.com/authorize",
		"client-abc",
		"https://myapp.example.com/callback",
		"random-state-token",
		"code-challenge-abc",
		[]string{"openid", "profile", "email"},
	)

	assert.Contains(t, url, "https://accounts.example.com/authorize?")
	assert.Contains(t, url, "response_type=code")
	assert.Contains(t, url, "client_id=client-abc")
	assert.Contains(t, url, "redirect_uri=https%3A%2F%2Fmyapp.example.com%2Fcallback")
	assert.Contains(t, url, "state=random-state-token")
	assert.Contains(t, url, "code_challenge=code-challenge-abc")
	assert.Contains(t, url, "code_challenge_method=S256")
	assert.Contains(t, url, "scope=openid+profile+email")
}

func TestBuildAuthURL_SingleScope(t *testing.T) {
	url := security.BuildAuthURL(
		"https://auth.example.com/auth",
		"c1", "https://app/cb", "s", "ch", []string{"openid"},
	)
	assert.Contains(t, url, "scope=openid")
	assert.NotContains(t, url, "+")
}

func TestBuildAuthURL_EmptyScopes(t *testing.T) {
	url := security.BuildAuthURL(
		"https://auth.example.com/auth",
		"c1", "https://app/cb", "s", "ch", nil,
	)
	// Empty scopes -> scope parameter is empty string.
	assert.Contains(t, url, "scope=")
}

func TestExtractEmail_Present(t *testing.T) {
	claims := map[string]any{"email": "user@example.com"}
	assert.Equal(t, "user@example.com", security.ExtractEmail(claims))
}

func TestExtractEmail_Missing(t *testing.T) {
	assert.Equal(t, "", security.ExtractEmail(map[string]any{}))
	assert.Equal(t, "", security.ExtractEmail(map[string]any{"sub": "123"}))
}

func TestExtractEmail_NotString(t *testing.T) {
	assert.Equal(t, "", security.ExtractEmail(map[string]any{"email": 12345}))
}

func TestExtractRoles_StringClaim(t *testing.T) {
	claims := map[string]any{"roles": "admin"}
	roles := security.ExtractRoles(claims, "roles", nil)
	assert.Equal(t, []string{"admin"}, roles)
}

func TestExtractRoles_ArrayClaim(t *testing.T) {
	claims := map[string]any{"lyeve_roles": []any{"admin", "editor"}}
	roles := security.ExtractRoles(claims, "lyeve_roles", nil)
	assert.Equal(t, []string{"admin", "editor"}, roles)
}

func TestExtractRoles_MixedArrayDropsNonStrings(t *testing.T) {
	claims := map[string]any{"roles": []any{"admin", 42, true, "editor"}}
	roles := security.ExtractRoles(claims, "roles", nil)
	assert.Equal(t, []string{"admin", "editor"}, roles)
}

func TestExtractRoles_MissingClaimUsesDefault(t *testing.T) {
	defaultRoles := []string{"user"}
	roles := security.ExtractRoles(map[string]any{}, "roles", defaultRoles)
	assert.Equal(t, defaultRoles, roles)
}

func TestExtractRoles_EmptyStringClaimUsesDefault(t *testing.T) {
	roles := security.ExtractRoles(
		map[string]any{"roles": ""}, "roles", []string{"user"},
	)
	assert.Equal(t, []string{"user"}, roles)
}

func TestExtractRoles_EmptyArrayClaimUsesDefault(t *testing.T) {
	roles := security.ExtractRoles(
		map[string]any{"roles": []any{}}, "roles", []string{"user"},
	)
	assert.Equal(t, []string{"user"}, roles)
}

func TestExtractRoles_NilClaimKeyUsesDefault(t *testing.T) {
	roles := security.ExtractRoles(map[string]any{"roles": "admin"}, "", []string{"user"})
	assert.Equal(t, []string{"user"}, roles)
}

// JWT Edge Cases

func TestSign_WithTenantID(t *testing.T) {
	secret := "test-sign-with-tenant-secret32b"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "tenant@example.com", []string{"editor"}, "acme", 1)
	require.NoError(t, err)

	// The raw payload, read without verification, must carry the tenant the
	// token was signed for.
	raw, _, err := new(jwt.Parser).ParseUnverified(token, &auth.Claims{})
	require.NoError(t, err)
	c := raw.Claims.(*auth.Claims)
	assert.Equal(t, "acme", c.TenantID, "Sign must write the tenant into the tenant_id claim")
}

func TestSign_NilRoles(t *testing.T) {
	secret := "test-nil-roles-secret-32-bytes"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "noroles@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Nil(t, claims.Roles)
}

func TestParse_TamperedPayload(t *testing.T) {
	secret := "tamper-test-secret-32-bytes-ok"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "tamper@example.com", nil, "", 1)
	require.NoError(t, err)

	// Corrupt the signature by appending garbage.
	_, err = auth.Parse(secret, token+"x")
	assert.Error(t, err, "corrupted token should fail verification")
}

func TestParse_EmptyToken(t *testing.T) {
	_, err := auth.Parse("secret", "")
	assert.Error(t, err)
}

func TestParseMulti_EmptyTokenFails(t *testing.T) {
	_, err := auth.ParseMulti([]string{"secret"}, "")
	assert.Error(t, err)
}

// JWKS Parsing: parseJWK (indirect via ParseExternal)

func TestParseExternal_WithMockedJWKS(t *testing.T) {
	// Generate an RSA keypair and sign a token, then mock a JWKS endpoint
	// serving the public key.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": "rsa-key-1",
				"alg": "RS256",
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   "user-1",
		"email": "rsa-user@example.com",
		"roles": []any{"admin"},
		"iss":   srv.URL,
		"aud":   security.JWTAudience,
		"iat":   12345,
		"exp":   9999999999,
	})
	tok.Header["kid"] = "rsa-key-1"
	tokenStr, err := tok.SignedString(rsaKey)
	require.NoError(t, err)

	claims, err := auth.ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "user-1", claims.UserID)
	assert.Equal(t, "rsa-user@example.com", claims.Email)
	assert.Equal(t, []string{"admin"}, claims.Roles)
}

func TestParseExternal_WithECDSAKey(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	ecdhKey, err := ecKey.ECDH()
	require.NoError(t, err)
	pubBytes := ecdhKey.PublicKey().Bytes()
	coordLen := (len(pubBytes) - 1) / 2

	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "EC",
				"kid": "ec-key-1",
				"alg": "ES256",
				"use": "sig",
				"crv": "P-256",
				"x":   base64.RawURLEncoding.EncodeToString(pubBytes[1 : 1+coordLen]),
				"y":   base64.RawURLEncoding.EncodeToString(pubBytes[1+coordLen:]),
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"sub":   "ec-user",
		"email": "ec-user@example.com",
		"iss":   srv.URL,
		"aud":   security.JWTAudience,
		"iat":   12345,
		"exp":   9999999999,
	})
	tok.Header["kid"] = "ec-key-1"
	tokenStr, err := tok.SignedString(ecKey)
	require.NoError(t, err)

	claims, err := auth.ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "ec-user", claims.UserID)
}

func TestParseExternal_RejectsHMAC(t *testing.T) {
	// ParseExternal only accepts asymmetric keys (RSA, ECDSA).
	jwks := map[string]any{"keys": []map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user",
		"iss": srv.URL,
		"aud": security.JWTAudience,
		"iat": 12345,
		"exp": 9999999999,
	})
	tokenStr, err := tok.SignedString([]byte("secret"))
	require.NoError(t, err)

	_, err = auth.ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	assert.Error(t, err, "HS256 token should be rejected by ParseExternal")
	assert.Contains(t, err.Error(), "unexpected signing method")
	assert.NotContains(t, err.Error(), "HS256", "error must not leak attacker-controlled alg value")
}

func TestParseExternal_JWKSHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := auth.ParseExternal(context.Background(), "any.token", jwksURLFor(t, srv), srv.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestParseExternal_InvalidJWKSJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := auth.ParseExternal(context.Background(), "any.token", jwksURLFor(t, srv), srv.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "jwks:")
}

func TestParseExternal_JWKSMalformedKey_Skipped(t *testing.T) {
	// Keys with unsupported kty are skipped, so no valid key = parse failure.
	jwks := map[string]any{
		"keys": []map[string]any{
			{"kty": "oct", "kid": "bad", "use": "sig"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	_, err := auth.ParseExternal(context.Background(), "any.token", jwksURLFor(t, srv), srv.URL)
	assert.Error(t, err)
}

func TestParseExternal_NoMatchingKid(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": "different-key",
				"alg": "RS256",
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user",
		"iss": srv.URL,
		"aud": security.JWTAudience,
		"iat": 12345,
		"exp": 9999999999,
	})
	tok.Header["kid"] = "my-key" // doesn't match "different-key"
	tokenStr, err := tok.SignedString(rsaKey)
	require.NoError(t, err)

	// The kid doesn't match, but a single-key JWKS falls back to the sole key  --
	// which is the signing key here, so the token still parses.
	claims, err := auth.ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "user", claims.UserID)
}

func TestParseExternal_ExpiredToken(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user",
		"iss": srv.URL,
		"aud": security.JWTAudience,
		"iat": 1,
		"exp": 1, // already expired
	})
	tok.Header["kid"] = "k1"
	tokenStr, err := tok.SignedString(rsaKey)
	require.NoError(t, err)

	_, err = auth.ParseExternal(context.Background(), tokenStr, jwksURLFor(t, srv), srv.URL)
	assert.Error(t, err)
}

func TestParseExternal_JWKSCaching(t *testing.T) {
	callCount := 0
	jwks := map[string]any{
		"keys": []map[string]any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()

	// First call fetches. Second call returns from cache.
	_, _ = auth.ParseExternal(context.Background(), "x.y", jwksURLFor(t, srv), srv.URL)
	_, _ = auth.ParseExternal(context.Background(), "x.y", jwksURLFor(t, srv), srv.URL)

	assert.Equal(t, 1, callCount, "second call should use cached JWKS")
}

// OIDC Discovery (via HTTP mock)

func TestDiscoverOIDC_Success(t *testing.T) {
	baseURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/.well-known/openid-configuration", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 baseURL,
			"authorization_endpoint": baseURL + "/authorize",
			"token_endpoint":         baseURL + "/token",
			"userinfo_endpoint":      baseURL + "/userinfo",
		})
	}))
	baseURL = srv.URL
	defer srv.Close()

	cfg, err := security.DiscoverOIDC(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, srv.URL, cfg.Issuer)
	assert.Equal(t, srv.URL+"/authorize", cfg.AuthorizationEndpoint)
	assert.Equal(t, srv.URL+"/token", cfg.TokenEndpoint)
	assert.Equal(t, srv.URL+"/userinfo", cfg.UserinfoEndpoint)
}

func TestDiscoverOIDC_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := security.DiscoverOIDC(context.Background(), srv.URL)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "oidc discover")
}

func TestDiscoverOIDC_Caching(t *testing.T) {
	callCount := 0
	baseURL := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": baseURL,
		})
	}))
	baseURL = srv.URL
	defer srv.Close()

	_, _ = security.DiscoverOIDC(context.Background(), srv.URL)
	_, _ = security.DiscoverOIDC(context.Background(), srv.URL)

	assert.Equal(t, 1, callCount, "second call should use cached OIDC config")
}

// Inline JWKS handler edge cases

func TestJWKSHandler_EdDSAActive_KeyFieldsValid(t *testing.T) {
	ensureEdDSA(t)

	handler := auth.JWKSHandler()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp jwksResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	require.Len(t, resp.Keys, 1)

	key := resp.Keys[0]
	// X should be valid base64url of 32 bytes (Ed25519 public key).
	xBytes, err := base64.RawURLEncoding.DecodeString(key.X)
	require.NoError(t, err)
	assert.Len(t, xBytes, 32, "Ed25519 public key should be 32 bytes")
}

// Claims edge cases

func TestClaims_HasRole_Nil(t *testing.T) {
	c := &auth.Claims{Roles: nil}
	assert.False(t, c.HasRole("anything"))
}

func TestClaims_HasRole_EmptySlice(t *testing.T) {
	c := &auth.Claims{Roles: []string{}}
	assert.False(t, c.HasRole("admin"))
}

func TestClaims_AuthClaims_NilRoles(t *testing.T) {
	c := &auth.Claims{
		UserID: "u1",
		Email:  "u1@test.com",
		Roles:  nil,
	}
	ac := c.AuthClaims()
	assert.Nil(t, ac.Roles)
}

// Token type purpose claims (typ)

func TestSign_TokenTypeSession(t *testing.T) {
	secret := "test-typ-session-secret-32bytes"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "alice@example.com", []string{"admin"}, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Equal(t, "session", claims.TokenType,
		"Sign() should set typ=session on session tokens")
}

func TestSignChallenge_TokenTypeChallenge(t *testing.T) {
	secret := "test-typ-challenge-secret-32b"
	id := uuid.New()
	token, err := auth.SignChallenge(secret, id, "bob@example.com", []string{"user"}, "")
	require.NoError(t, err)

	claims, err := auth.ParseMulti([]string{secret}, token)
	require.NoError(t, err)
	assert.Equal(t, "challenge", claims.TokenType,
		"SignChallenge() should set typ=challenge on MFA challenge tokens")
	assert.True(t, claims.MFAPending,
		"SignChallenge() should set MFAPending=true")
}

func TestSign_MFAPendingFalse(t *testing.T) {
	secret := "test-mfa-false-secret-32bytes!"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "carol@example.com", []string{"editor"}, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.False(t, claims.MFAPending,
		"Sign() should NOT set MFAPending on session tokens")
}

// ExchangeCode + FetchUserinfo

func TestExchangeCode_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token-abc",
			"id_token":      "id-token-abc",
			"refresh_token": "refresh-token-abc",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	tok, err := security.ExchangeCode(
		context.Background(), srv.URL, "client-1", "secret-1",
		"auth-code", "pkce-verifier", "https://app/cb",
	)
	require.NoError(t, err)
	assert.Equal(t, "access-token-abc", tok.AccessToken)
	assert.Equal(t, "id-token-abc", tok.IDToken)
	assert.Equal(t, "refresh-token-abc", tok.RefreshToken)
	assert.Equal(t, "Bearer", tok.TokenType)
	assert.Equal(t, 3600, tok.ExpiresIn)
}

func TestExchangeCode_NoClientSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify client_secret was NOT sent.
		_ = r.ParseForm()
		assert.NotContains(t, r.Form, "client_secret")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok",
			"token_type":   "Bearer",
		})
	}))
	defer srv.Close()

	tok, err := security.ExchangeCode(
		context.Background(), srv.URL, "client-1", "",
		"auth-code", "pkce-verifier", "https://app/cb",
	)
	require.NoError(t, err)
	assert.Equal(t, "tok", tok.AccessToken)
}

func TestExchangeCode_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	_, err := security.ExchangeCode(
		context.Background(), srv.URL, "c", "s", "code", "v", "cb",
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
}

func TestFetchUserinfo_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-access-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":   "user-1",
			"email": "user1@example.com",
			"name":  "Test User",
		})
	}))
	defer srv.Close()

	claims, err := security.FetchUserinfo(context.Background(), srv.URL, "test-access-token")
	require.NoError(t, err)
	assert.Equal(t, "user-1", claims["sub"])
	assert.Equal(t, "user1@example.com", claims["email"])
}

func TestFetchUserinfo_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := security.FetchUserinfo(context.Background(), srv.URL, "bad-token")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")
}

func TestFetchUserinfo_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := security.FetchUserinfo(context.Background(), srv.URL, "token")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

// Helpers

func computeS256Challenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(hash[:])
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
