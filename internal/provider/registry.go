package provider

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Registry is the global provider registry. All built-in providers self-register
// via init(). Plugins and middleware use Registry.Get(name) or Registry.Detect(dsn)
// to resolve the right provider at runtime.
var Registry = &registry{
	providers: make(map[string]Provider),
}

// registry is a thread-safe map of provider name -> Provider.
type registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// Register adds a provider to the global registry. Call during init() or
// early boot. Panics on duplicate registration: a duplicate means two
// packages claimed the same name, which is a code bug.
func (r *registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := p.Name()
	if _, exists := r.providers[name]; exists {
		panic(fmt.Sprintf("provider: duplicate registration for %q", name))
	}
	r.providers[name] = p
	slog.Debug("provider: registered", "name", name, "category", p.Category())
}

// Get returns the provider for the named provider, or nil if not registered.
func (r *registry) Get(name string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// Detect resolves the right provider by attempting auto-detection with every
// registered provider of the given category. Returns the first match or nil.
func (r *registry) Detect(category Category, dsn string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if p.Category() == category && p.AutoDetect(dsn) {
			return p
		}
	}
	return nil
}

// MustGet returns the provider for name or panics. Use for early-boot code
// where a missing provider is a fatal configuration error.
func (r *registry) MustGet(name string) Provider {
	p := r.Get(name)
	if p == nil {
		panic(fmt.Sprintf("provider: %q not registered", name))
	}
	return p
}

// Names returns all registered provider names.
func (r *registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	return names
}

// ByCategory returns all registered providers of the given category.
func (r *registry) ByCategory(category Category) []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Provider
	for _, p := range r.providers {
		if p.Category() == category {
			out = append(out, p)
		}
	}
	return out
}

// HealthCheckAll runs HealthCheck on every registered provider concurrently
// and returns the results. Supports context cancellation/timeout.
func (r *registry) HealthCheckAll(ctx context.Context) []HealthStatus {
	r.mu.RLock()
	providers := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		providers = append(providers, p)
	}
	r.mu.RUnlock()

	results := make([]HealthStatus, len(providers))
	g, gCtx := errgroup.WithContext(ctx)
	for i, p := range providers {
		g.Go(func() error {
			results[i] = runHealthCheck(gCtx, p)
			return nil
		})
	}
	_ = g.Wait() // all funcs return nil
	return results
}

func runHealthCheck(ctx context.Context, p Provider) HealthStatus {
	start := time.Now()
	err := p.HealthCheck(ctx)
	elapsed := time.Since(start)

	status := HealthStatus{
		Provider:  p.Name(),
		Category:  p.Category(),
		Healthy:   err == nil,
		Latency:   elapsed,
		CheckedAt: start,
	}
	if err != nil {
		status.Error = err.Error()
	}
	if hc, ok := p.(HealthChecker); ok {
		detail := hc.DetailedHealth(ctx)
		detail.CheckedAt = start
		return detail
	}
	return status
}
