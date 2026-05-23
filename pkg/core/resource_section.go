package core

import (
	"slices"
	"sync"
)

// A resource section is a top-level list in the engine's configuration file
// that a plugin builds its resources from when the engine runs with no
// database. The engine reads a section for the plugin through
// DeclaredResources and never learns the shape of an entry. The settings
// loader leaves every registered section out of the settings, since an entry
// describes a resource rather than a setting, and a secret flattened into a
// setting would be listed by the configuration route.

// resourceSections holds the registered section names. The kernel registers
// none of its own: every section arrives from the plugin that reads it.
var resourceSections = struct {
	mu    sync.RWMutex
	names map[string]bool
}{names: map[string]bool{}}

// RegisterResourceSection declares a top-level section of the configuration
// file as a list of a plugin's resources. A plugin registers it from init,
// because the settings loader reads the file at boot, before any plugin
// starts. Registering a known name changes nothing. It panics on an empty
// name, which no file could declare.
func RegisterResourceSection(name string) {
	if name == "" {
		panic("core.RegisterResourceSection: name is empty")
	}
	resourceSections.mu.Lock()
	defer resourceSections.mu.Unlock()
	resourceSections.names[name] = true
}

// ResourceSections returns the registered section names, sorted.
func ResourceSections() []string {
	resourceSections.mu.RLock()
	defer resourceSections.mu.RUnlock()
	names := make([]string, 0, len(resourceSections.names))
	for name := range resourceSections.names {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
