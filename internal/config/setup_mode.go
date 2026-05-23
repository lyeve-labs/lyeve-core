package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SetupSetting is one setting the engine needs before it can serve anything,
// named the way an operator sets it in each layer.
type SetupSetting struct {
	EnvKey   string // the environment variable, e.g. DATABASE_URL
	YAMLPath string // the dotted path in lyeve.yaml, e.g. database.url
	Secret   bool   // a random value is a valid answer, so setup mode can offer one
}

// setupSettings are the settings whose absence setup mode reports, in the
// order the operator should set them.
var setupSettings = []SetupSetting{
	{EnvKey: "DATABASE_URL", YAMLPath: "database.url"},
	{EnvKey: "JWT_SECRET", YAMLPath: "jwt.secret", Secret: true},
	{EnvKey: "ENCRYPTION_KEY", YAMLPath: "encryption.key", Secret: true},
}

// SetupRequiredError is what Load returns instead of a configuration when
// LYEVE_SETUP_MODE is on and a setting the engine cannot run without is
// missing. It carries what a setup-mode server needs and nothing else: no
// database, no secret, no plugin configuration.
type SetupRequiredError struct {
	Missing         []SetupSetting
	AdminListenAddr string
	APIListenAddr   string
	SetupToken      string // LYEVE_SETUP_TOKEN, empty when unset
	InstanceID      string
	// TLSCertFile and TLSKeyFile carry the listener pair into setup mode, so a
	// deployment that set them does not send the setup token in clear.
	TLSCertFile string
	TLSKeyFile  string
}

func (e *SetupRequiredError) Error() string {
	keys := make([]string, len(e.Missing))
	for i, s := range e.Missing {
		keys[i] = s.EnvKey
	}
	return "setup mode: " + strings.Join(keys, ", ") + " not set"
}

// IsSetupRequired reports whether err asks the caller to serve setup mode.
func IsSetupRequired(err error) (*SetupRequiredError, bool) {
	var s *SetupRequiredError
	if errors.As(err, &s) {
		return s, true
	}
	return nil, false
}

// checkSetupMode returns a SetupRequiredError when the operator opted into
// setup mode and a required setting is missing, and nil otherwise, so a boot
// without the opt-in fails on a missing setting.
//
// A missing ENCRYPTION_KEY counts here even though a normal boot falls back to
// JWT_SECRET with a warning: an operator who opted in is still choosing the
// values, so setup mode offers a separate key rather than the deprecated reuse.
func checkSetupMode() error {
	raw := lookup("LYEVE_SETUP_MODE")
	if raw == "" {
		return nil
	}
	on, err := strconv.ParseBool(raw)
	if err != nil {
		return fmt.Errorf("LYEVE_SETUP_MODE must be true or false, got %q", raw)
	}
	if !on {
		return nil
	}
	var missing []SetupSetting
	for _, s := range setupSettings {
		if lookup(s.EnvKey) == "" {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	token := lookup("LYEVE_SETUP_TOKEN")
	if token != "" && len(token) < minSetupTokenLen {
		return fmt.Errorf("LYEVE_SETUP_TOKEN must be at least %d characters", minSetupTokenLen)
	}
	return &SetupRequiredError{
		Missing:         missing,
		AdminListenAddr: envOr("ADMIN_LISTEN_ADDR", defaultAdminListenAddr),
		APIListenAddr:   envOr("API_LISTEN_ADDR", defaultAPIListenAddr),
		SetupToken:      token,
		InstanceID:      lookup("INSTANCE_ID"),
		TLSCertFile:     lookup("TLS_CERT_FILE"),
		TLSKeyFile:      lookup("TLS_KEY_FILE"),
	}
}
