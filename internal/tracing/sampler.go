package tracing

import (
	"context"
	"math/rand"
	"sync"

	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// tenantContextKey mirrors tenant.Key to avoid importing core
// (which creates an import cycle through config -> db -> tracing).
// Uses the same type (tenant.ContextKey) so context.Value lookups
// match values set by core.WithTenantID and tenant.WithID.
var tenantContextKey = tenant.Key

// TenantSampler decides whether to sample a span based on the tenant
// extracted from the parent context.
//
// Root spans (typically created by HTTPMiddleware before auth/tenant
// middleware runs) always use fallbackRate. Child spans created within
// plugin handlers or store operations, where the tenant ID has been
// injected into the context by TenantHeader middleware, are sampled at
// the per-tenant rate.
//
// Usage: wrap with sdktrace.ParentBased(ts) so children inherit their
// parent's sampling decision before any per-tenant rate check applies.
type TenantSampler struct {
	mu           sync.RWMutex
	tenantRates  map[string]float64
	fallbackRate float64
}

// NewTenantSampler creates a TenantSampler.
//
// tenantRates maps tenant slugs to sampling rates (0.0-1.0). Nil or empty
// means every tenant uses fallbackRate.
//
// fallbackRate is the global sampling rate used when no tenant is present
// or the tenant is not found in tenantRates. Values outside [0.0, 1.0]
// are clamped.
func NewTenantSampler(tenantRates map[string]float64, fallbackRate float64) *TenantSampler {
	if fallbackRate < 0 {
		fallbackRate = 1.0
	}
	if fallbackRate > 1.0 {
		fallbackRate = 1.0
	}
	rates := make(map[string]float64, len(tenantRates))
	for k, v := range tenantRates {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		rates[k] = v
	}
	return &TenantSampler{
		tenantRates:  rates,
		fallbackRate: fallbackRate,
	}
}

// ShouldSample implements sdktrace.Sampler. Called by sdktrace.ParentBased
// only for root spans (no parent, or parent without trace flags).
func (s *TenantSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	s.mu.RLock()
	rate := s.fallbackRate
	if tenantID := tenantIDFromCtx(p.ParentContext); tenantID != "" {
		if r, ok := s.tenantRates[tenantID]; ok {
			rate = r
		}
	}
	s.mu.RUnlock()

	if rate >= 1.0 {
		return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
	}
	if rate <= 0.0 {
		return sdktrace.SamplingResult{Decision: sdktrace.Drop}
	}

	// rand.Float64 uses the package-level source, which is safe for
	// concurrent use: ShouldSample runs on every span across goroutines.
	if rand.Float64() < rate {
		return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
	}
	return sdktrace.SamplingResult{Decision: sdktrace.Drop}
}

// Description implements sdktrace.Sampler.
func (s *TenantSampler) Description() string {
	return "TenantSampler"
}

// UpdateRates atomically replaces the per-tenant rate map without
// restarting the tracer provider. Safe for concurrent use.
func (s *TenantSampler) UpdateRates(tenantRates map[string]float64, fallbackRate float64) {
	if fallbackRate < 0 {
		fallbackRate = 1.0
	}
	if fallbackRate > 1.0 {
		fallbackRate = 1.0
	}
	rates := make(map[string]float64, len(tenantRates))
	for k, v := range tenantRates {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		rates[k] = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenantRates = rates
	s.fallbackRate = fallbackRate
}

// tenantIDFromCtx reads the tenant ID from ctx using the canonical
// core.TenantContextKey. Duplicated here to avoid import cycles.
func tenantIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(tenantContextKey).(string); ok {
		return v
	}
	return ""
}
