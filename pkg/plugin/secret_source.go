package plugin

import (
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security/secrets"
)

// SecretSourceProvider is implemented by a plugin (e.g. a vault or kms plugin)
// that supplies a secrets.Source backed by an external custody system. The
// runtime chains the provided Source in front of the env-backed default, so
// raw secrets (the API-key pepper, the master KEK, the JWT signing secret) can
// move out of the process environment without any core change.
//
// This is the crypto-custody seam: algorithms stay in core (public by design),
// while the secret *material* moves behind a pluggable boundary.
type SecretSourceProvider interface {
	core.Plugin
	// SecretSource returns the custody-backed source, or nil to decline.
	SecretSource() secrets.Source
}

// SecretSource returns the secrets.Source from the first running plugin that
// implements SecretSourceProvider, or nil when none is active. Typically at
// most one such plugin runs. When several do, the choice among them is
// unspecified. Safe to call from any goroutine. Takes a read lock.
//
// The runtime composes the result as secrets.Chain{activator.SecretSource(),
// secrets.EnvSource{}} so a KMS/Vault plugin takes precedence and the process
// environment remains a local-dev fallback.
func (a *Activator) SecretSource() secrets.Source {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, ap := range a.active {
		if ap == nil || ap.plugin == nil || ap.phase != PhaseRunning {
			continue
		}
		if provider, ok := ap.plugin.(SecretSourceProvider); ok {
			if src := provider.SecretSource(); src != nil {
				return src
			}
		}
	}
	return nil
}
