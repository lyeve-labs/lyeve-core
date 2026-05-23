package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Ed25519 signing key: persisted to disk, used for EdDSA JWT signing.

// signingKey is the active Ed25519 private key for JWT signing.
// It is nil when the user has not opted into EdDSA (i.e. JWT_ALG is unset
// or set to "HS256") or when the key file is not yet initialized.
var (
	signingKey   ed25519.PrivateKey
	signingKeyMu sync.RWMutex
	signingKid   string // deterministic key ID derived from the public key
	eddsaActive  bool   // true when the signing key is available for use
)

// keyFile represents the on-disk representation of an Ed25519 keypair.
// only seed and public key are persisted: the full 64-byte private key
// is reconstructed from the seed at load time.
type keyFile struct {
	Seed      []byte `json:"seed"`       // 32-byte seed
	PublicKey []byte `json:"public_key"` // 32-byte public key
	Kid       string `json:"kid"`        // deterministic key ID
	CreatedAt string `json:"created_at"` // RFC 3339 timestamp
}

// Public API

// InitJWTSigning loads an Ed25519 keypair from disk or generates a new one.
// Call once during boot before any JWT signing or verification.
//
// Behavior:
//
//	If JWT_ALG == "HS256"  -> no Ed25519, keep using HMAC (legacy behavior).
//	If keyPath file exists   -> load it, activate EdDSA.
//	If keyPath file absent  -> generate a new keypair, save with mode 0600, activate EdDSA.
//
// keyPath should come from the JWT_KEY_PATH env var (default /var/lib/lyeve/jwt_key.json).
func InitJWTSigning(keyPath string) error {
	alg := os.Getenv("JWT_ALG")
	if alg == "HS256" {
		// Explicit opt-out: keep legacy HMAC-only behavior.
		return nil
	}

	// EdDSA is the default. An empty or unrecognized JWT_ALG still enables it.

	key, kid, err := loadOrGenerateKey(keyPath)
	if err != nil {
		return fmt.Errorf("init jwt signing: %w", err)
	}

	signingKeyMu.Lock()
	signingKey = key
	signingKid = kid
	eddsaActive = true
	signingKeyMu.Unlock()
	return nil
}

// IsEdDSAActive reports whether EdDSA signing is enabled.
// When false, Sign and SignChallenge fall back to HMAC-SHA256.
func IsEdDSAActive() bool {
	signingKeyMu.RLock()
	defer signingKeyMu.RUnlock()
	return eddsaActive
}

// PublicKey returns the active Ed25519 public key, or nil when EdDSA is
// not active. External consumers (e.g. security.ParseJWT) use this to
// validate EdDSA-signed tokens.
func PublicKey() ed25519.PublicKey {
	signingKeyMu.RLock()
	defer signingKeyMu.RUnlock()
	if signingKey == nil {
		return nil
	}
	pub := signingKey.Public()
	if pub == nil {
		return nil
	}
	key, ok := pub.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil
	}
	return key
}

// The signing key has no exported accessor on purpose.

// JWKSHandler serves the Ed25519 public key in JWKS (RFC 7517) format so
// external services can verify CMS-issued tokens without a shared secret.
// GET /.well-known/jwks.json: public, never requires auth.
func JWKSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		signingKeyMu.RLock()
		active := eddsaActive
		var pub ed25519.PublicKey
		if signingKey != nil {
			pub = signingKey.Public().(ed25519.PublicKey)
		}
		kid := signingKid
		signingKeyMu.RUnlock()

		if !active || len(pub) != ed25519.PublicKeySize {
			// No Ed25519 key: return an empty keyset. This is valid JWKS
			// and signals "no asymmetric keys available" to clients.
			writeJWKS(w, []jwkEntry{})
			return
		}

		writeJWKS(w, []jwkEntry{{
			Kty: "OKP",
			Crv: "Ed25519",
			X:   base64.RawURLEncoding.EncodeToString(pub),
			Use: "sig",
			Alg: "EdDSA",
			Kid: kid,
		}})
	}
}

// Internal: key persistence

// loadOrGenerateKey tries to load the key from path. If the file doesn't
// exist, a fresh Ed25519 keypair is generated and persisted with mode 0600.
func loadOrGenerateKey(path string) (ed25519.PrivateKey, string, error) {
	key, kid, err := loadKey(path)
	if err == nil {
		return key, kid, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate ed25519 keypair: %w", err)
	}
	kid = deriveKid(pub)
	seed := priv.Seed()

	if err := saveKey(path, seed, pub, kid); err != nil {
		return nil, "", fmt.Errorf("save key to %s: %w", path, err)
	}
	return priv, kid, nil
}

// loadKey reads and validates a key file from disk.
func loadKey(path string) (ed25519.PrivateKey, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var kf keyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, "", fmt.Errorf("parse key file: %w", err)
	}
	if len(kf.Seed) != ed25519.SeedSize {
		return nil, "", fmt.Errorf("invalid seed length: got %d, want %d", len(kf.Seed), ed25519.SeedSize)
	}
	if len(kf.PublicKey) != ed25519.PublicKeySize {
		return nil, "", fmt.Errorf("invalid public key length: got %d, want %d", len(kf.PublicKey), ed25519.PublicKeySize)
	}
	// Reconstruct the full 64-byte private key from the seed.
	priv := ed25519.NewKeyFromSeed(kf.Seed)
	// Verify the reconstructed public key matches what's on disk (defense in depth).
	derivedPub := priv.Public().(ed25519.PublicKey)
	if !ed25519.PublicKey(kf.PublicKey).Equal(derivedPub) {
		return nil, "", fmt.Errorf("public key in file does not match seed-derived key")
	}
	return priv, kf.Kid, nil
}

// saveKey writes the keypair to path atomically with mode 0600.
func saveKey(path string, seed, pub []byte, kid string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	kf := keyFile{
		Seed:      seed,
		PublicKey: pub,
		Kid:       kid,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	// Write to a temp file then rename for atomicity.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// deriveKid produces a deterministic, collision-resistant key ID from the
// Ed25519 public key. Uses the first 8 bytes of SHA-256(pub), base64url-encoded.
func deriveKid(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(h[:8])
}

// JWKS serialization

// jwkEntry is a single key in a JWKS response. It uses a minimal struct rather
// than the one in jwks.go (which is for *parsing* external keys) to keep
// serialization precise and avoid leaking internal types.
type jwkEntry struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	Kid string `json:"kid,omitempty"`
}

type jwksResponse struct {
	Keys []jwkEntry `json:"keys"`
}

func writeJWKS(w http.ResponseWriter, keys []jwkEntry) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	// Ensure keys is never nil in the JSON output: marshal as empty array.
	if keys == nil {
		keys = []jwkEntry{}
	}
	_ = json.NewEncoder(w).Encode(jwksResponse{Keys: keys}) // err suppressed: response write to client
}
