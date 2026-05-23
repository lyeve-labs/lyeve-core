package provider

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Registry is the global provider registry. Plugins and middleware use
// Registry.Get(name) or Registry.Detect(dsn) to resolve the right provider
// at runtime.
var Registry = &registry{
	providers: make(map[string]Provider),
}

// registry is a thread-safe map of engine name -> Provider.
type registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// Register adds a provider to the global registry. Call during init() or
// early boot. Panics on duplicate registration.
//
// Panic is intentional: this is an init-time invariant. All DB providers
// register via init() functions. A duplicate means two engine providers
// shipped with the same name, which is a compile-time code bug.
func (r *registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := p.Name()
	if _, exists := r.providers[name]; exists {
		panic(fmt.Sprintf("provider: duplicate registration for %q", name))
	}
	r.providers[name] = p
	slog.Debug("provider: registered", "engine", name)
}

// Get returns the provider for the named engine, or nil if not registered.
func (r *registry) Get(name string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// Detect resolves the right provider by inspecting a DSN string. Uses the
// same heuristics as db.EngineFromDSN. Returns the postgres provider as a
// safe default for unrecognized DSNs.
func (r *registry) Detect(dsn string) Provider {
	engine := r.engineFromDSN(dsn)
	if p := r.Get(engine); p != nil {
		return p
	}
	return r.Get("postgres")
}

// Names returns all registered provider engine names.
func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	return names
}

// MustGet returns the provider for name or panics. Use for early-boot code
// that must not proceed without a known provider.
//
// Panic is intentional: this is used in test code where a missing provider
// is a fatal test setup error. Callers should have validated the engine name
// at startup before reaching this code path.
func (r *registry) MustGet(name string) Provider {
	p := r.Get(name)
	if p == nil {
		panic(fmt.Sprintf("provider: %q not registered", name))
	}
	return p
}

// engineFromDSN detects the engine from a DSN string. Maps to the same
// engines recognized by db.EngineFromDSN: postgres, mysql, mssql.
func (r *registry) engineFromDSN(dsn string) string {
	// URL-scheme parsing
	if idx := strings.Index(dsn, "://"); idx >= 0 {
		scheme := strings.ToLower(dsn[:idx])
		switch scheme {
		case "mysql":
			return "mysql"
		case "sqlserver", "mssql":
			return "mssql"
		case "postgresql", "postgres":
			return "postgres"
		}
	}
	// MySQL DSN without scheme: user:pass@tcp(host:port)/dbname
	if strings.Contains(dsn, "@tcp(") {
		return "mysql"
	}
	// Fallback: assume PostgreSQL keyword=value DSN
	return "postgres"
}
