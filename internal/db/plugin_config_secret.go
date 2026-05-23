package db

import (
	"errors"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// Credentials in stored plugin configuration.
//
// Operator-set configuration exists so an API key can be supplied from the
// admin UI instead of a redeploy. That only holds if the key is protected the
// way every other credential the engine stores is: encrypted at rest with the
// tenant's derived key, and never handed back out.
//
// A sealed value is written as a self-describing object rather than a bare
// string, so ciphertext can never be confused with a value an operator typed:
//
//	{"example_pass": {"$secret": "a91f..."}}
//
// Which keys are credentials is decided by core.IsSecretKey, the same rule the
// Config redaction guard uses. A key that reads as secret on one path cannot
// read as ordinary on the other.

// SecretEnvelopeField is the object key holding ciphertext for a sealed value.
const SecretEnvelopeField = "$secret"

// SecretMask is what a set credential reads as through the admin API. Writing
// it back preserves the stored value, so a client that reads a whole
// configuration object, edits one field and submits it does not overwrite every
// credential with the mask.
const SecretMask = "********"

// ErrNoSealer is returned when a credential is saved on an engine with no
// encryption key configured. Storing it in plain text instead would put the
// credential in the clear in a table the admin API can read.
var ErrNoSealer = errors.New("cannot store a credential without an encryption key: set ENCRYPTION_KEY")

// ConfigSealer encrypts and decrypts stored credentials. Satisfied by
// pkg/security/encryption.KeyStore.
type ConfigSealer interface {
	EncryptPlaintext(tenantID string, plaintext []byte) (string, error)
	DecryptPlaintext(tenantID, encHex string) ([]byte, error)
}

// sealValues encrypts every credential in cfg, leaving ordinary settings alone.
// The second return names credentials the caller must carry over unchanged from
// what is already stored.
//
// The three ways a credential can arrive are distinguished, because a save that
// cannot tell them apart either destroys credentials or makes them impossible
// to clear:
//
//	a value      replaces what is stored
//	the mask     leaves what is stored alone, so a client that reads a whole
//	             configuration, edits one field and submits it does not
//	             overwrite every credential with the mask it was shown
//	empty        clears the credential
func sealValues(sealer ConfigSealer, tenantID string, cfg map[string]any) (map[string]any, []string, error) {
	out := make(map[string]any, len(cfg))
	var preserve []string
	for key, val := range cfg {
		if !core.IsSecretKey(key) {
			out[key] = val
			continue
		}
		s, ok := val.(string)
		if !ok {
			// A credential is a string. Anything else is a client error rather
			// than something to encrypt the JSON encoding of.
			return nil, nil, fmt.Errorf("credential %q must be text", key)
		}
		switch s {
		case SecretMask:
			preserve = append(preserve, key)
		case "":
			// Explicitly cleared: neither stored nor carried over.
		default:
			if sealer == nil {
				return nil, nil, fmt.Errorf("%q: %w", key, ErrNoSealer)
			}
			enc, err := sealer.EncryptPlaintext(tenantID, []byte(s))
			if err != nil {
				return nil, nil, fmt.Errorf("encrypt credential %q: %w", key, err)
			}
			out[key] = map[string]any{SecretEnvelopeField: enc}
		}
	}
	return out, preserve, nil
}

// unsealValues decrypts sealed credentials so the configuration layer can serve
// them to the plugin that owns them.
//
// A credential stored as a bare string is returned as it stands, and the next
// save seals it. A credential that cannot be decrypted
// is dropped rather than surfaced, because handing a plugin ciphertext as
// though it were an API key produces a failure at the far end of an integration
// with nothing pointing back here.
func unsealValues(sealer ConfigSealer, tenantID string, cfg map[string]any) (map[string]any, []string) {
	out := make(map[string]any, len(cfg))
	var failed []string
	for key, val := range cfg {
		enc, sealed := secretEnvelope(val)
		if !sealed {
			out[key] = val
			continue
		}
		if sealer == nil {
			failed = append(failed, key)
			continue
		}
		plain, err := sealer.DecryptPlaintext(tenantID, enc)
		if err != nil {
			failed = append(failed, key)
			continue
		}
		out[key] = string(plain)
	}
	return out, failed
}

// maskValues replaces every credential with the mask, so a configuration can be
// shown to an operator without disclosing what is set. An unset credential is
// absent rather than masked, which is how the UI tells "configured" from
// "empty".
func maskValues(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for key, val := range cfg {
		if _, sealed := secretEnvelope(val); sealed {
			out[key] = SecretMask
			continue
		}
		if core.IsSecretKey(key) {
			// A bare string is plain text in the table. It still must not
			// leave the process.
			if s, ok := val.(string); ok && s == "" {
				continue
			}
			out[key] = SecretMask
			continue
		}
		out[key] = val
	}
	return out
}

// secretEnvelope reports whether a stored value is sealed ciphertext and
// returns it.
func secretEnvelope(val any) (string, bool) {
	obj, ok := val.(map[string]any)
	if !ok || len(obj) != 1 {
		return "", false
	}
	enc, ok := obj[SecretEnvelopeField].(string)
	if !ok || enc == "" {
		return "", false
	}
	return enc, true
}

// NewConfigSealer builds the sealer that protects stored credentials from the
// master encryption key.
//
// Deliberately keyed on ENCRYPTION_KEY rather than on the KEK a plugin secret
// source may supply. That source is resolved from the activator, which does not
// exist until plugins have started, and stored configuration has to be readable
// before that. Keying both the save path and the boot path on the same value
// keeps what one writes readable by the other.
//
// Returns nil when no key is configured, which makes the store refuse to save
// credentials rather than write them in plain text.
func NewConfigSealer(masterKey string) ConfigSealer {
	if masterKey == "" {
		return nil
	}
	ks, err := encryption.NewKeyStore(masterKey)
	if err != nil {
		return nil
	}
	return ks
}
