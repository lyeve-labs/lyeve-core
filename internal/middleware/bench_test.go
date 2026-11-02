package middleware_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// RateLimiter benchmarks: per-IP token bucket check

// BenchmarkRateLimiter_Allow measures one admitted request through a limiter
// built once, as the router builds it. The rate is high enough that no bucket
// runs dry, so every iteration takes the allow path.
func BenchmarkRateLimiter_Allow(b *testing.B) {
	handler := mw.RateLimiter(1e9, 1_000_000)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	b.Run("hot_key", func(b *testing.B) {
		req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		req.RemoteAddr = "192.0.2.1:12345"
		b.ReportAllocs()
		for b.Loop() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("unexpected status %d", rec.Code)
			}
		}
	})

	// 1,024 addresses stay under DefaultMaxKeys, so each one keeps its bucket
	// and the loop measures lookups across a populated map, not eviction.
	b.Run("1024_keys", func(b *testing.B) {
		reqs := make([]*http.Request, 1024)
		for i := range reqs {
			reqs[i] = httptest.NewRequest(http.MethodGet, "/api/test", nil)
			reqs[i].RemoteAddr = fmt.Sprintf("198.51.%d.%d:12345", i/256, i%256)
		}
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, reqs[i%len(reqs)])
			if rec.Code != http.StatusOK {
				b.Fatalf("unexpected status %d", rec.Code)
			}
			i++
		}
	})
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
