package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/ssrf"
)

// JWKS key cache

const (
	jwksCacheTTL         = time.Hour
	jwksNegativeCacheTTL = time.Minute
	jwksBodyLimit        = 1 << 20 // 1 MiB
)

type jwksEntry struct {
	keys map[string]any // kid -> *rsa.PublicKey or *ecdsa.PublicKey
	err  error          // non-nil for negative cache entries
	at   time.Time
}

var (
	jwksMu    sync.RWMutex
	jwksCache = map[string]*jwksEntry{}
)

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// fetchJWKS returns the JWKS public keys for jwksURL, indexed by kid.
// Successes are cached for jwksCacheTTL. Failures are negatively cached for
// jwksNegativeCacheTTL so a down IdP is not re-hit on every login.
func fetchJWKS(ctx context.Context, jwksURL string) (map[string]any, error) {
	jwksMu.RLock()
	entry := jwksCache[jwksURL]
	jwksMu.RUnlock()
	if entry != nil {
		if entry.err != nil {
			if time.Since(entry.at) < jwksNegativeCacheTTL {
				return nil, entry.err
			}
		} else if time.Since(entry.at) < jwksCacheTTL {
			return entry.keys, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		err = fmt.Errorf("jwks: create request: %w", err)
		cacheNegative(jwksURL, err)
		return nil, err
	}
	resp, err := ssrf.NewSafeHTTPClient(10*time.Second, 3).Do(req)
	if err != nil {
		err = fmt.Errorf("jwks: fetch: %w", err)
		cacheNegative(jwksURL, err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("jwks: status %d", resp.StatusCode)
		cacheNegative(jwksURL, err)
		return nil, err
	}

	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, jwksBodyLimit)).Decode(&set); err != nil {
		err = fmt.Errorf("jwks: decode: %w", err)
		cacheNegative(jwksURL, err)
		return nil, err
	}

	keys := make(map[string]any, len(set.Keys))
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := parseJWK(k)
		if err != nil {
			continue // skip malformed keys
		}
		keys[k.Kid] = pub
	}

	jwksMu.Lock()
	jwksCache[jwksURL] = &jwksEntry{keys: keys, at: time.Now()}
	jwksMu.Unlock()
	return keys, nil
}

// cacheNegative stores a failure entry so repeated ParseExternal calls
// to a down/erroring provider do not each hit the network within the
// negative TTL (jwksNegativeCacheTTL).
func cacheNegative(jwksURL string, err error) {
	jwksMu.Lock()
	jwksCache[jwksURL] = &jwksEntry{err: err, at: time.Now()}
	jwksMu.Unlock()
}

func parseJWK(k jwk) (any, error) {
	switch k.Kty {
	case "RSA":
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("rsa n: %w", err)
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("rsa e: %w", err)
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: int(new(big.Int).SetBytes(eb).Int64()),
		}, nil

	case "EC":
		xb, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("ec x: %w", err)
		}
		yb, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, fmt.Errorf("ec y: %w", err)
		}
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("ec: unsupported curve %s", k.Crv)
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xb),
			Y:     new(big.Int).SetBytes(yb),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported kty: %s", k.Kty)
	}
}

// ParseExternal validates a JWT issued by a trusted external OIDC/OAuth2 server.
// jwksURL should be the full URL to the issuer's JWKS endpoint, e.g.
// https://accounts.google.com/.well-known/jwks.json
// expectedIssuer is the issuer URL that the token's "iss" claim must match.
// The token's "aud" claim must include JWTAudience ("lyeve-api").
// On success the returned Claims have Roles set from the "roles" or "lyeve_roles" claim.
func ParseExternal(ctx context.Context, tokenStr, jwksURL, expectedIssuer string) (*Claims, error) {
	keys, err := fetchJWKS(ctx, jwksURL)
	if err != nil {
		return nil, fmt.Errorf("external jwt: %w", err)
	}

	// Use pre-built audienceOnlyParser (avoids per-request jwt.NewParser).
	// The issuer is verified from the decoded claims since it varies per
	// trusted OIDC provider.
	tok, err := audienceOnlyParser.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		// Only accept asymmetric signing algorithms.
		switch t.Method.(type) {
		case *jwt.SigningMethodRSA, *jwt.SigningMethodECDSA:
		default:
			alg, _ := t.Header["alg"].(string)
			slog.WarnContext(ctx, "external jwt rejected: unsupported signing algorithm",
				"alg", alg,
			)
			return nil, fmt.Errorf("unexpected signing method")
		}
		kid, _ := t.Header["kid"].(string)
		if key, ok := keys[kid]; ok {
			return key, nil
		}
		// Kid absent or unrecognized. For a single-key JWKS, fall back to the
		// sole key (some OIDC providers omit kid or use a non-matching value).
		// For a multi-key set, an unknown kid MUST be rejected: the caller
		// cannot know which key to use, and returning a random key from the
		// (non-deterministic) map iteration is incorrect.
		if len(keys) == 1 {
			for _, k := range keys {
				return k, nil
			}
		}
		return nil, fmt.Errorf("no matching key for kid %q", kid)
	})
	if err != nil {
		return nil, fmt.Errorf("external jwt: parse: %w", err)
	}

	claims, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, fmt.Errorf("external jwt: invalid claims")
	}

	// The parser is audience-only, so verify the issuer here: it varies per
	// trusted OIDC provider.
	if claims.Issuer != expectedIssuer {
		return nil, fmt.Errorf("external jwt: expected issuer %q, got %q", expectedIssuer, claims.Issuer)
	}

	// Another issuer's tenant claim is taken as the request's tenant. Every
	// tenant here is a slug, and on MySQL and MSSQL, where tenant columns fold
	// case, any other spelling would address a real tenant under a second
	// name.
	if claims.TenantID != "" && !core.IsValidTenantSlug(claims.TenantID) {
		return nil, fmt.Errorf("external jwt: tenant_id %q is not a tenant slug", claims.TenantID)
	}

	// Normalize a missing roles claim to an empty slice.
	if claims.Roles == nil {
		claims.Roles = []string{}
	}
	return claims, nil
}
