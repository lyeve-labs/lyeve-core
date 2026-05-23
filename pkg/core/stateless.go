package core

import (
	"encoding/json"
	"errors"
)

// Stateless mode is an engine booted with no database at all, opted into with
// LYEVE_MODE=stateless. Its API keys come from the configuration file, it runs
// one implicit tenant, and it keeps nothing across a restart. Only a plugin
// that declares it can run that way is started. Every other plugin is listed
// as inactive with the reason.

// EngineModeStateless is the value of LYEVE_MODE, and of the engine_mode
// configuration key a plugin reads, on an engine running with no database.
const EngineModeStateless = "stateless"

// ConfigKeyEngineMode is the configuration key that names the engine's mode:
// EngineModeStateless, or empty on an engine with a database.
const ConfigKeyEngineMode = "engine_mode"

// ErrNoDatabase is the error every query returns on a stateless engine. A
// plugin that reaches the database there has a store it did not replace, and
// the error says why rather than surfacing as a connection failure.
var ErrNoDatabase = errors.New("no database: the engine is running in stateless mode")

// StatelessCapable is implemented by a plugin that can run on an engine with
// no database. It answers per instance so a plugin can decline when its own
// configuration needs one. A plugin that does not implement it is never
// started in stateless mode.
type StatelessCapable interface {
	StatelessCapable() bool
}

// IsStatelessCapable reports whether p declares that it runs with no database.
func IsStatelessCapable(p any) bool {
	sc, ok := p.(StatelessCapable)
	return ok && sc.StatelessCapable()
}

// IsStateless reports whether cfg belongs to an engine running with no
// database. A plugin reads it in Start to choose its in-memory or file-backed
// store.
func IsStateless(cfg Config) bool {
	return cfg != nil && cfg.String(ConfigKeyEngineMode) == EngineModeStateless
}

// DeclaredResourceProvider is implemented by the engine host and forwarded by
// the scoped host. It hands a plugin the entries of one resource section of
// the configuration file, each as a JSON document with ${NAME} references
// already replaced from the environment. A stateless plugin builds its
// resources from them. The engine does not know their shape.
type DeclaredResourceProvider interface {
	DeclaredResources(section string) ([]json.RawMessage, error)
}

// DeclaredResources returns the entries of section from the host, or nil when
// the host declares none.
func DeclaredResources(h Host, section string) ([]json.RawMessage, error) {
	p, ok := h.(DeclaredResourceProvider)
	if !ok {
		return nil, nil
	}
	return p.DeclaredResources(section)
}
