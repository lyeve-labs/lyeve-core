package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSwappableHandler_ServeHTTP(t *testing.T) {
	h1 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("v1"))
	})
	swapper := NewSwappableHandler(h1)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	swapper.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "v1" {
		t.Fatalf("expected v1, got %q", rec.Body.String())
	}
}

func TestSwappableHandler_Swap(t *testing.T) {
	h1 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("v1"))
	})
	h2 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("v2"))
	})
	swapper := NewSwappableHandler(h1)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	swapper.ServeHTTP(rec, req)
	if rec.Body.String() != "v1" {
		t.Fatalf("expected v1 before swap, got %q", rec.Body.String())
	}

	swapper.Swap(h2)

	rec = httptest.NewRecorder()
	swapper.ServeHTTP(rec, req)
	if rec.Body.String() != "v2" {
		t.Fatalf("expected v2 after swap, got %q", rec.Body.String())
	}
}

func TestSwappableHandler_Concurrent(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	swapper := NewSwappableHandler(h)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				rec := httptest.NewRecorder()
				swapper.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Errorf("unexpected status %d", rec.Code)
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			swapper.Swap(h)
		}()
	}

	wg.Wait()
}
