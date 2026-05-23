package plugin

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// ConfigSchemaProvider is an optional interface plugins implement to expose
// a JSON Schema representation of their configuration struct. The runtime
// detects it after Start() and serves it via
// GET /api/admin/plugins/{name}/schema so the admin dashboard can auto-
// generate configuration forms without hard-coding plugin knowledge.
type ConfigSchemaProvider interface {
	core.Plugin

	// ConfigSchema returns a JSON Schema document describing the plugin's
	// configuration struct. Return nil if the plugin has no configuration.
	ConfigSchema() map[string]any
}
