package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// API keys declared in the configuration tree.
//
// A stateless engine has no table to keep keys in, so it reads them from the
// same files as its settings:
//
//	api_keys:
//	  - name: stripe-relay
//	    sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
//	    scopes: ["flows:write"]
//	  - name: operator
//	    sha256: 60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752
//	    roles: [super_admin]
//	    expires_at: 2027-01-01T00:00:00Z
//
// A key is declared by the hex SHA-256 of its value, never by the value. The
// hash is unsalted, so it protects only a key with real entropy: generate each
// one with openssl rand -hex 32, and treat the file as a secret all the same.
// The section composes like schemas, through $include and conf.d.

// DeclaredAPIKey is one entry of the api_keys section.
type DeclaredAPIKey struct {
	Name      string    `yaml:"name"`
	SHA256    string    `yaml:"sha256"`
	Roles     []string  `yaml:"roles"`
	Scopes    []string  `yaml:"scopes"`
	ExpiresAt time.Time `yaml:"expires_at"`
}

// declarableRoles are the roles a declared key may carry. A stateless engine
// has no user table, so a role means only what the route gates read from it:
// admin opens the admin routes, super_admin the super-admin ones too.
var declarableRoles = map[string]bool{
	core.RoleAdmin:      true,
	core.RoleSuperAdmin: true,
}

// LoadDeclaredAPIKeys reads the api_keys section of the configuration tree. An
// empty path searches the conventional locations. A malformed entry stops the
// boot: a key the operator believes works and the engine skipped would be
// found only when a caller is refused.
func LoadDeclaredAPIKeys(path string) ([]DeclaredAPIKey, error) {
	if path == "" {
		path = discoverConfigPath()
		if path == "" {
			return nil, nil
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		// LoadFiles reports a missing path first.
		return nil, nil
	}
	files, err := expandPath(path, info.IsDir())
	if err != nil {
		return nil, err
	}

	var keys []DeclaredAPIKey
	names := map[string]string{}
	hashes := map[string]string{}
	loaded := map[string]bool{}
	for _, f := range files {
		err := collectSection(f, apiKeysKey, loaded, 0, func(val *yaml.Node, file string) error {
			if val.Kind != yaml.SequenceNode {
				return fmt.Errorf("config %s:%d: api_keys must be a list of keys", file, val.Line)
			}
			for _, item := range val.Content {
				where := fmt.Sprintf("config %s:%d", file, item.Line)
				var k DeclaredAPIKey
				if err := item.Decode(&k); err != nil {
					return fmt.Errorf("%s: %w", where, err)
				}
				if err := k.normalize(); err != nil {
					return fmt.Errorf("%s: api key %q: %w", where, k.Name, err)
				}
				if prev, dup := names[k.Name]; dup {
					return fmt.Errorf("%s: api key name %q is already declared at %s", where, k.Name, prev)
				}
				if prev, dup := hashes[k.SHA256]; dup {
					return fmt.Errorf("%s: api key %q has the same sha256 as the key declared at %s", where, k.Name, prev)
				}
				names[k.Name] = where
				hashes[k.SHA256] = where
				keys = append(keys, k)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// normalize checks one declaration and lower-cases its hash, the form the
// engine hashes a presented key into.
func (k *DeclaredAPIKey) normalize() error {
	k.Name = strings.TrimSpace(k.Name)
	if k.Name == "" {
		return fmt.Errorf("name is required")
	}
	k.SHA256 = strings.ToLower(strings.TrimSpace(k.SHA256))
	if raw, err := hex.DecodeString(k.SHA256); err != nil || len(raw) != 32 {
		return fmt.Errorf("sha256 must be 64 hex characters, the SHA-256 of the key; the key itself never goes in the file")
	}
	for _, r := range k.Roles {
		if !declarableRoles[r] {
			return fmt.Errorf("role %q is not one a declared key can carry; use %q or %q, or scopes", r, core.RoleAdmin, core.RoleSuperAdmin)
		}
	}
	for _, s := range k.Scopes {
		resource, action, ok := strings.Cut(s, ":")
		if !ok || resource == "" || action == "" {
			return fmt.Errorf("scope %q must be resource:action, for example flows:write", s)
		}
	}
	if len(k.Roles) == 0 && len(k.Scopes) == 0 {
		return fmt.Errorf("a key with no roles and no scopes can reach nothing; give it one or the other")
	}
	// A role grants its full reach and the scope check skips the key, so
	// scopes beside a role would read as a limit that is never applied.
	if len(k.Roles) > 0 && len(k.Scopes) > 0 {
		return fmt.Errorf("give a key roles or scopes, not both: a role is not narrowed by scopes")
	}
	return nil
}
