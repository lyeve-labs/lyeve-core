package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
)

// Where the setup token an operator must present comes from. The admin's
// first-run page names the place, so these are part of the setup response.
const (
	SetupTokenFromEnv = "env" // LYEVE_SETUP_TOKEN
	SetupTokenFromLog = "log" // generated at boot and printed once
)

// SetupTokenHeader carries the setup token on requests that have no body.
const SetupTokenHeader = "X-Setup-Token"

// SetupToken is the credential first-run setup demands. Without it the first
// caller to reach a fresh install would become its super admin, and the admin
// listener binds every interface by default.
//
// Only a digest is kept, and presented tokens are digested before comparison,
// so the comparison runs in constant time whatever the presented length. A
// nil or empty SetupToken matches nothing: setup that nobody wired stays
// closed rather than open.
type SetupToken struct {
	mu      sync.RWMutex
	digest  [sha256.Size]byte
	active  bool
	source  string
	retired bool
}

// NewSetupTokenFromEnv returns the token an operator configured. An empty
// value yields a token that matches nothing.
func NewSetupTokenFromEnv(value string) *SetupToken {
	t := &SetupToken{source: SetupTokenFromEnv}
	if value != "" {
		t.digest = sha256.Sum256([]byte(value))
		t.active = true
	}
	return t
}

// GenerateSetupToken returns a random token and its plaintext. The caller
// prints the plaintext once. Nothing else keeps it.
func GenerateSetupToken() (*SetupToken, string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, "", fmt.Errorf("generate setup token: %w", err)
	}
	plain := hex.EncodeToString(raw[:])
	return &SetupToken{digest: sha256.Sum256([]byte(plain)), active: true, source: SetupTokenFromLog}, plain, nil
}

// Matches reports whether presented is the setup token.
func (t *SetupToken) Matches(presented string) bool {
	if t == nil || presented == "" {
		return false
	}
	got := sha256.Sum256([]byte(presented))
	t.mu.RLock()
	defer t.mu.RUnlock()
	ok := subtle.ConstantTimeCompare(got[:], t.digest[:]) == 1
	return ok && t.active && !t.retired
}

// Retire ends the token once the first super admin exists. The account check
// in the store refuses a second claim on its own. Retiring also drops the
// digest so a token read from an old log line opens nothing on this process.
func (t *SetupToken) Retire() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.retired = true
	t.digest = [sha256.Size]byte{}
}

// Source names where the operator finds the token, or "" when there is none
// to find.
func (t *SetupToken) Source() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.active || t.retired {
		return ""
	}
	return t.source
}
