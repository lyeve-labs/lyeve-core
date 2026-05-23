package core

import "testing"

// secretsHost adds a SecretsProvider to stubHost, which is the shape the
// engine host has in production.
type secretsHost struct {
	stubHost
	vals  map[string]string
	lists map[string][]string
}

func (h *secretsHost) Secret(key string) (string, bool) {
	v, ok := h.vals[key]
	return v, ok && v != ""
}

func (h *secretsHost) Secrets(key string) ([]string, bool) {
	v, ok := h.lists[key]
	return v, ok && len(v) > 0
}

func TestSecret_PrefersProviderOverRedactedConfig(t *testing.T) {
	// A host that redacts the key through Config, which is what configAdapter
	// does for everything in SecretKeys.
	h := &secretsHost{
		stubHost: stubHost{cfg: &stubConfig{vals: map[string]string{"encryption_key": ""}}},
		vals:     map[string]string{"encryption_key": "real-key"},
	}
	if got := Secret(h, "encryption_key"); got != "real-key" {
		t.Errorf("Secret = %q, want the provider value", got)
	}
}

func TestSecret_FallsBackToConfig(t *testing.T) {
	// Test hosts implement Config only.
	h := &stubHost{cfg: &stubConfig{vals: map[string]string{"smtp_user": "postmaster"}}}
	if got := Secret(h, "smtp_user"); got != "postmaster" {
		t.Errorf("Secret = %q, want the config value", got)
	}
}

func TestSecret_UnsetEverywhere(t *testing.T) {
	h := &secretsHost{stubHost: stubHost{cfg: &stubConfig{}}}
	if got := Secret(h, "redis_url"); got != "" {
		t.Errorf("Secret = %q, want empty", got)
	}
}

func TestSecretList_PrefersProviderOverRedactedConfig(t *testing.T) {
	h := &secretsHost{
		stubHost: stubHost{cfg: &stubConfig{}},
		lists:    map[string][]string{"jwt_secrets": {"current", "previous"}},
	}
	got := SecretList(h, "jwt_secrets")
	if len(got) != 2 || got[0] != "current" {
		t.Errorf("SecretList = %v, want the provider list", got)
	}
}

func TestSecretList_UnsetEverywhere(t *testing.T) {
	h := &secretsHost{stubHost: stubHost{cfg: &stubConfig{}}}
	if got := SecretList(h, "jwt_secrets"); len(got) != 0 {
		t.Errorf("SecretList = %v, want empty", got)
	}
}

func TestSecret_NilHost(t *testing.T) {
	// Plugin helpers can run before Start assigns the host.
	if got := Secret(nil, "encryption_key"); got != "" {
		t.Errorf("Secret = %q, want empty", got)
	}
	if got := SecretList(nil, "jwt_secrets"); got != nil {
		t.Errorf("SecretList = %v, want nil", got)
	}
}
