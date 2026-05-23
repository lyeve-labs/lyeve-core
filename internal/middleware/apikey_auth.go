package middleware

import (
	"context"
	"net/http"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// APIKeyLookupFn resolves a key hash to auth claims. Returns nil, nil when
// the key is not found, disabled, or expired. Returns nil, error on store
// errors (caller should fail open: treat as unauthenticated).
type APIKeyLookupFn func(ctx context.Context, hash string) (*core.AuthClaims, error)

// APIKeyUpgradeFn rewrites a key's stored hash from oldHash to newHash. It is
// invoked fire-and-forget when a key created before the pepper was installed
// authenticates via its legacy plain-SHA-256 hash, migrating it to the
// peppered HMAC form on first use. May be nil (no upgrade performed).
type APIKeyUpgradeFn func(ctx context.Context, oldHash, newHash string)

// APIKeyAuth returns middleware that authenticates requests via the
// X-API-Key header. See APIKeyAuthWithUpgrade. This is the no-upgrade variant,
// for callers that do not migrate legacy hashes.
func APIKeyAuth(lookupFn APIKeyLookupFn) func(http.Handler) http.Handler {
	return APIKeyAuthWithUpgrade(lookupFn, nil)
}

// APIKeyAuthWithUpgrade returns middleware that authenticates requests via the
// X-API-Key header. When a valid key is present and no JWT claims exist, it
// delegates to lookupFn to resolve the key hash to auth claims and stores them
// in the request context under core.ClaimsKey. JWT-authenticated requests
// bypass API key auth.
//
// Lookup uses the peppered hash (security.HashKeyPeppered). When a pepper is
// configured and the peppered lookup misses, it falls back to the legacy
// plain-SHA-256 hash so keys minted before the pepper was installed keep
// working. On a legacy hit, upgradeFn (when non-nil) rewrites the stored hash
// to the peppered form so the fallback is needed at most once per key.
//
// lookupFn must handle the full key lookup: hash -> claims, including
// enabled/expired checks. Returns (nil, nil) for invalid/disabled/expired
// keys. Returns (nil, error) on transient store failures.
func APIKeyAuthWithUpgrade(lookupFn APIKeyLookupFn, upgradeFn APIKeyUpgradeFn) func(http.Handler) http.Handler {
	if lookupFn == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Already authenticated: skip.
			if core.GetClaims(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}

			raw := r.Header.Get("X-API-Key")
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}

			peppered := security.HashKeyPeppered(raw)
			claims, err := lookupFn(r.Context(), peppered)

			// Legacy fallback: keys minted before the pepper was installed are
			// stored under plain SHA-256. Only retry when a pepper is configured
			// (otherwise peppered == legacy and the retry is pointless).
			if (err != nil || claims == nil) && security.APIKeyPepperConfigured() {
				if legacy := security.HashKey(raw); legacy != peppered {
					if legacyClaims, legacyErr := lookupFn(r.Context(), legacy); legacyErr == nil && legacyClaims != nil {
						claims, err = legacyClaims, nil
						if upgradeFn != nil {
							upgradeFn(r.Context(), legacy, peppered)
						}
					}
				}
			}

			if err != nil || claims == nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx := context.WithValue(r.Context(), core.ClaimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
