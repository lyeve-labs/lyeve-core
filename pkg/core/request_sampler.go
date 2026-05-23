package core

import (
	"context"
	"net/http"
	"sync"
)

// The request sampler slot.
//
// A plugin that samples every request both routers serve records duration,
// status and, when configured, the allocations and heap state around the
// handler. A plugin cannot wrap the routers itself, so the engine keeps one
// slot for it. The plugin implements RequestSamplerProvider, which one plugin
// holds. The activator reads it from the running plugin that implements it,
// and the runtime mounts what it returns on the admin and the API chain in
// one fixed position: below the rate limiters and the query monitor, above
// analytics, the WAF and PII masking. That position is the capability: what
// the sampler times is the handler plus everything mounted below it, and
// moving it changes every number it reports.
//
// The sampler attributes each request to the plugin that served it through
// PluginSlot. Routing happens below the sampler on a context it never sees,
// so a label written there by the engine is invisible to the sampler's own
// request. The sampler installs a slot before the chain runs, the engine
// fills it when it dispatches to a plugin route, and the sampler reads it
// afterwards. A request the engine served itself leaves it empty, and
// EnginePluginLabel names that.
//
// Usage in the plugin:
//
//	func (p *Plugin) RequestSampler() func(http.Handler) http.Handler {
//	    return func(next http.Handler) http.Handler {
//	        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	            ctx, slot := core.WithPluginSlot(r.Context())
//	            start := time.Now()
//	            next.ServeHTTP(w, r.WithContext(ctx))
//	            p.record(slot.PluginOr(core.EnginePluginLabel), r, time.Since(start))
//	        })
//	    }
//	}

// RequestSamplerProvider is implemented by a plugin that samples requests.
// Returning nil mounts nothing.
type RequestSamplerProvider interface {
	RequestSampler() func(http.Handler) http.Handler
}

// EnginePluginLabel is the plugin label of a request the engine served
// itself: the metrics registry and a request sampler both report it as
// "core".
const EnginePluginLabel = "core"

// PluginSlot carries the name of the plugin a request was routed to back
// out to middleware that wrapped the whole chain. It is the TenantSlot
// pattern for the route owner.
type PluginSlot struct {
	mu   sync.Mutex
	name string
}

// Set records the plugin the request was dispatched to. Safe for
// concurrent use and on a nil slot.
func (s *PluginSlot) Set(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
}

// Get returns the recorded plugin, or "" when the engine served the request
// itself or nothing installed a slot.
func (s *PluginSlot) Get() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// PluginOr returns the recorded plugin, or fallback when there is none.
func (s *PluginSlot) PluginOr(fallback string) string {
	if name := s.Get(); name != "" {
		return name
	}
	return fallback
}

type pluginSlotKey struct{}

// WithPluginSlot returns a context carrying a fresh slot, and the slot.
func WithPluginSlot(ctx context.Context) (context.Context, *PluginSlot) {
	s := &PluginSlot{}
	return context.WithValue(ctx, pluginSlotKey{}, s), s
}

// PluginSlotFrom returns the slot on the context, or nil when there is none.
func PluginSlotFrom(ctx context.Context) *PluginSlot {
	s, _ := ctx.Value(pluginSlotKey{}).(*PluginSlot)
	return s
}
