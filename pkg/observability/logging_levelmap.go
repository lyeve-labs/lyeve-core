package observability

import (
	"log/slog"
	"sync"
)

// LogLevelManager provides dynamic log level resolution per (tenant, plugin).
type LogLevelManager interface {
	Level(tenant, plugin string) slog.Level
	DefaultLevel() slog.Level
}

// LogLevelMap provides thread-safe per-tenant and per-plugin log level resolution.
// Lookup priority: "tenant:plugin" composite -> plugin -> tenant -> default.
type LogLevelMap struct {
	mu           sync.RWMutex
	defaultLevel slog.Level
	tenants      map[string]slog.Level
	plugins      map[string]slog.Level
}

// NewLogLevelMap creates a LogLevelMap with the given default level.
func NewLogLevelMap(defaultLevel slog.Level) *LogLevelMap {
	if defaultLevel == 0 {
		defaultLevel = slog.LevelInfo
	}
	return &LogLevelMap{
		defaultLevel: defaultLevel,
		tenants:      make(map[string]slog.Level),
		plugins:      make(map[string]slog.Level),
	}
}

// DefaultLevel returns the fallback level used when no override matches.
func (m *LogLevelMap) DefaultLevel() slog.Level {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultLevel
}

// Level resolves the effective log level for a (tenant, plugin) pair.
// Resolution is most-specific-first: an exact "tenant:plugin" override, then
// the plugin-wide level, then the tenant-wide level, then the default.
func (m *LogLevelMap) Level(tenant, plugin string) slog.Level {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if tenant != "" && plugin != "" {
		if lv, ok := m.plugins[tenant+":"+plugin]; ok {
			return lv
		}
	}
	if plugin != "" {
		if lv, ok := m.plugins[plugin]; ok {
			return lv
		}
	}
	if tenant != "" {
		if lv, ok := m.tenants[tenant]; ok {
			return lv
		}
	}
	return m.defaultLevel
}

// SetTenantLevel sets the level for a tenant. A zero level removes the override.
func (m *LogLevelMap) SetTenantLevel(tenant string, level slog.Level) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if level == 0 {
		delete(m.tenants, tenant)
	} else {
		m.tenants[tenant] = level
	}
}

// SetPluginLevel sets the level for a plugin key ("plugin" or "tenant:plugin").
// A zero level removes the override.
func (m *LogLevelMap) SetPluginLevel(key string, level slog.Level) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if level == 0 {
		delete(m.plugins, key)
	} else {
		m.plugins[key] = level
	}
}

// SetDefaultLevel replaces the fallback level.
func (m *LogLevelMap) SetDefaultLevel(level slog.Level) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultLevel = level
}

// Snapshot returns a copy of the current level configuration for serialization.
func (m *LogLevelMap) Snapshot() LogLevelSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenants := make(map[string]LogLevelJSON, len(m.tenants))
	for k, v := range m.tenants {
		tenants[k] = LogLevelJSON(v)
	}
	plugins := make(map[string]LogLevelJSON, len(m.plugins))
	for k, v := range m.plugins {
		plugins[k] = LogLevelJSON(v)
	}
	return LogLevelSnapshot{
		DefaultLevel: m.defaultLevel,
		Tenants:      tenants,
		Plugins:      plugins,
	}
}

// Merge replaces the map's configuration with the given snapshot.
func (m *LogLevelMap) Merge(s LogLevelSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultLevel = s.DefaultLevel
	m.tenants = make(map[string]slog.Level, len(s.Tenants))
	for k, v := range s.Tenants {
		m.tenants[k] = v.Level()
	}
	m.plugins = make(map[string]slog.Level, len(s.Plugins))
	for k, v := range s.Plugins {
		m.plugins[k] = v.Level()
	}
}
