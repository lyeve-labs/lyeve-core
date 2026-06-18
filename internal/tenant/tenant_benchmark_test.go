package tenant_test

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/tenant"
)

// Benchmark_ID benchmarks the hot path: extracting tenant ID from a
// context that always carries one. This is called on every request.
func Benchmark_ID(b *testing.B) {
	ctx := tenant.WithID(context.Background(), "perf_bench")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = tenant.ID(ctx)
	}
}

// Benchmark_WithID benchmarks setting a tenant ID on a fresh context.
// Called once per request (or per middleware chain entry).
func Benchmark_WithID(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = tenant.WithID(context.Background(), "perf_bench")
	}
}

// Benchmark_ID_Empty benchmarks reading tenant ID from a context that
// has none (the cold path: happens on non-tenant requests).
func Benchmark_ID_Empty(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = tenant.ID(ctx)
	}
}
