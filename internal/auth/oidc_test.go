package auth

import (
	"context"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testClientID = "client-abc"

type idTokenOpts struct {
	issuer   string
	audience jwt.ClaimStrings
	azp      string
	nonce    string
	exp      time.Time
	omitIAT  bool
	omitExp  bool
}

func signIDToken(t *testing.T, priv *rsa.PrivateKey, kid string, o idTokenOpts) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":            "subject-1",
		"email":          "user@example.com",
		"email_verified": true,
		"iss":            o.issuer,
		"aud":            o.audience,
	}
	if !o.omitIAT {
		claims["iat"] = time.Now().Add(-time.Minute).Unix()
	}
	if !o.omitExp {
		exp := o.exp
		if exp.IsZero() {
			exp = time.Now().Add(time.Hour)
		}
		claims["exp"] = exp.Unix()
	}
	if o.azp != "" {
		claims["azp"] = o.azp
	}
	if o.nonce != "" {
		claims["nonce"] = o.nonce
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	require.NoError(t, err)
	return s
}

func TestParseIDToken_AcceptsAWellFormedToken(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID},
		nonce:    "nonce-xyz",
	})

	claims, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "nonce-xyz")
	require.NoError(t, err)
	assert.Equal(t, "subject-1", claims["sub"])
	assert.Equal(t, "user@example.com", claims["email"])
	assert.Equal(t, true, claims["email_verified"])
}

func TestParseIDToken_RefusesAnotherIssuersToken(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   "https://evil.example.com",
		audience: jwt.ClaimStrings{testClientID},
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected issuer")
}

func TestParseIDToken_RefusesATokenMintedForAnotherClient(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{"some-other-client"},
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audience does not include this client")
}

func TestParseIDToken_RefusesASharedTokenWithNoAuthorizedParty(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID, "another-client"},
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no azp claim")
}

func TestParseIDToken_RefusesASharedTokenAuthorizedForAnotherParty(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID, "another-client"},
		azp:      "another-client",
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "azp names a different client")
}

func TestParseIDToken_AcceptsASharedTokenAuthorizedForThisClient(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID, "another-client"},
		azp:      testClientID,
	})

	claims, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.NoError(t, err)
	assert.Equal(t, "subject-1", claims["sub"])
}

func TestParseIDToken_RefusesAReplayedTokenFromAnotherLogin(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID},
		nonce:    "nonce-from-a-different-login",
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "nonce-for-this-login")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonce mismatch")
}

func TestParseIDToken_RefusesATokenCarryingNoNonceWhenOneWasMinted(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID},
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "nonce-for-this-login")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no nonce claim")
}

func TestParseIDToken_RefusesAnExpiredToken(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID},
		exp:      time.Now().Add(-time.Hour),
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestParseIDToken_RefusesATokenWithNoExpiry(t *testing.T) {
	pub, priv := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	tok := signIDToken(t, priv, "k1", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID},
		omitExp:  true,
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
}

// An ID token is verified with the provider's public key. If the parser
// admitted HMAC, a caller could sign a token of their own using that public
// key as the shared secret and it would verify.
func TestParseIDToken_RefusesASymmetricallySignedToken(t *testing.T) {
	pub, _ := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "k1", pub: pub}})
	defer srv.Close()

	claims := jwt.MapClaims{
		"sub": "attacker",
		"iss": srv.URL,
		"aud": jwt.ClaimStrings{testClientID},
		"iat": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	forged.Header["kid"] = "k1"
	tok, err := forged.SignedString(pub.N.Bytes())
	require.NoError(t, err)

	_, err = ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
	// Assert the algorithm allowlist is what refused it. Without the allowlist
	// the token is still refused, but only because the HMAC verifier is handed
	// an *rsa.PublicKey and reports a key-type error. That is incidental, and a
	// test that accepts it would pass with the allowlist removed.
	assert.Contains(t, err.Error(), "signing method HS256 is invalid")
}

func TestParseIDToken_RefusesAnUnknownKeyIDAgainstAMultiKeyProvider(t *testing.T) {
	pubA, _ := genRSA2048(t)
	pubB, _ := genRSA2048(t)
	srv := jwksServer(t, []rsaJWK{{kid: "a", pub: pubA}, {kid: "b", pub: pubB}})
	defer srv.Close()

	_, privX := genRSA2048(t)
	tok := signIDToken(t, privX, "x", idTokenOpts{
		issuer:   srv.URL,
		audience: jwt.ClaimStrings{testClientID},
	})

	_, err := ParseIDToken(context.Background(), tok, srv.URL, srv.URL, testClientID, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no matching key for kid "x"`)
}

func TestParseIDToken_RefusesAnUnconfiguredClient(t *testing.T) {
	_, err := ParseIDToken(context.Background(), "irrelevant", "https://idp.example.com/jwks", "https://idp.example.com", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no client id configured")
}
