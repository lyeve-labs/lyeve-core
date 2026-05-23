package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// RateLimiter benchmarks: per-IP token bucket check

// BenchmarkRateLimiter_Allow measures the hot path where the rate limit fires
// (bucket check + tokens decrement). Creates a fresh rate limiter per iteration
// to avoid token exhaustion across Go benchmark rounds.
func BenchmarkRateLimiter_Allow(b *testing.B) {
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.RemoteAddr = "192.0.2.1:12345"

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rateLimiter := mw.RateLimiter(200, 100)
		handler := rateLimiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("unexpected status %d", rec.Code)
		}
	}
}

// BenchmarkRateLimiter_Contention measures contention when many distinct IPs
// hit the rate limiter concurrently: exercises the map+mutex path.
func BenchmarkRateLimiter_Contention(b *testing.B) {
	rateLimiter := mw.RateLimiter(200, 100)
	handler := rateLimiter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ips := []string{
		"192.0.2.1:1000", "192.0.2.2:1001", "192.0.2.3:1002", "192.0.2.4:1003",
		"192.0.2.5:1004", "192.0.2.6:1005", "192.0.2.7:1006", "192.0.2.8:1007",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		idx := b.N % len(ips)
		req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		req.RemoteAddr = ips[idx]
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// SecurityHeaders benchmarks

func BenchmarkSecurityHeaders_NoHSTS(b *testing.B) {
	sh := mw.SecurityHeaders(false)
	handler := sh(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkSecurityHeaders_HSTS(b *testing.B) {
	sh := mw.SecurityHeaders(true)
	handler := sh(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// ResponseTime middleware benchmark

func BenchmarkResponseTime(b *testing.B) {
	rt := mw.ResponseTime()
	handler := rt(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// MaxBodySize benchmark

func BenchmarkMaxBodySize(b *testing.B) {
	mbs := mw.MaxBodySize(1 << 20) // 1 MiB
	handler := mbs(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// TenantHeader benchmarks: multi-tenancy middleware (no-op path)

func BenchmarkTenantHeader_Disabled(b *testing.B) {
	th := mw.TenantHeader(false) // multi-tenant disabled -> no-op
	handler := th(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// Combined middleware stack benchmark (realistic request path)

func BenchmarkFullStack(b *testing.B) {
	// Simulates the actual middleware stack applied in the CMS router:
	// MaxBodySize -> RateLimiter -> ResponseTime -> SecurityHeaders -> handler
	mbs := mw.MaxBodySize(1 << 20)
	rl := mw.RateLimiter(1_000_000, 100_000)
	rt := mw.ResponseTime()
	sh := mw.SecurityHeaders(false)

	handler := mbs(rl(rt(sh(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})))))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.RemoteAddr = "192.0.2.1:12345"

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("unexpected status %d", rec.Code)
		}
	}
}

// Compress benchmarks: gzip and brotli with per-content-type levels

func BenchmarkCompressWithLevels_Gzip_JSON(b *testing.B) {
	comp := mw.CompressWithLevelFunc(mw.ContentTypeLevelFunc)
	handler := comp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","count":42,"items":["a","b","c"]}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkCompressWithLevels_Brotli_JSON(b *testing.B) {
	comp := mw.CompressWithLevelFunc(mw.ContentTypeLevelFunc)
	handler := comp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","count":42,"items":["a","b","c"]}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.Header.Set("Accept-Encoding", "br")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

func BenchmarkCompress_NoEncoding(b *testing.B) {
	comp := mw.CompressWithLevelFunc(mw.ContentTypeLevelFunc)
	handler := comp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","count":42}`))
	}))

	// No Accept-Encoding -> pass-through.
	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// JSON handler benchmark: allocation-sensitive payload

func BenchmarkJSONHandler_LargePayload(b *testing.B) {
	// Simulates a real handler returning a moderately large JSON array.
	payload := make([]map[string]any, 100)
	for i := range payload {
		payload[i] = map[string]any{
			"id":    i,
			"title": "example item with moderate-length name",
			"slug":  "example-item-" + string(rune('a'+i%26)),
		}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		data, _ := json.Marshal(payload)
		w.Write(data)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
