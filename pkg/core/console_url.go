package core

import (
	"errors"
	"strings"
)

// ConfigKeyConsoleURL is the configuration key that holds the admin console's
// public URL, set by LYEVE_CONSOLE_URL. A link mailed to a person opens a page
// the console serves, so it is built on this. A listen address names a socket
// on the engine and a base URL names the engine, and neither is where that
// page is.
const ConfigKeyConsoleURL = "console_url"

// DevConsoleURL is where the console's development server listens. It is the
// console URL outside production when LYEVE_CONSOLE_URL is unset.
const DevConsoleURL = "http://localhost:5173"

// ErrConsoleURLUnset is returned in production when LYEVE_CONSOLE_URL is unset.
// A link built on a guess points nowhere, and the person it was mailed to sees
// a dead page rather than a refusal anyone can act on.
var ErrConsoleURLUnset = errors.New("LYEVE_CONSOLE_URL is unset: set it to the admin console's public URL, the address its sign-in page is opened at")

// ConsoleURL returns the console's public URL without a trailing slash.
//
// The engine validates the value at boot, so a set value is absolute and safe
// to append a path to. Unset, it is DevConsoleURL outside production and
// ErrConsoleURLUnset in production.
func ConsoleURL(cfg Config) (string, error) {
	if cfg == nil {
		return "", ErrConsoleURLUnset
	}
	if v := strings.TrimRight(strings.TrimSpace(cfg.String(ConfigKeyConsoleURL)), "/"); v != "" {
		return v, nil
	}
	if cfg.Bool("is_production") {
		return "", ErrConsoleURLUnset
	}
	return DevConsoleURL, nil
}
