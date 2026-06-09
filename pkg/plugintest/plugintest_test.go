package plugintest

import (
	"context"
	"net/http"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// HTTPRecorder tests

func TestHTTPRecorder_Do(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}

	resp := rec.Do(handler, Req("GET", "/test"))
	resp.AssertStatus(http.StatusOK)
	resp.AssertBodyContains(`"status":"ok"`)
}

func TestHTTPRecorder_WithChiParam(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		// chi.URLParam would be used by real handlers
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}

	req := Req("GET", "/api/bookmarks/abc123", WithChiParam("id", "abc123"))
	resp := rec.Do(handler, req)
	resp.AssertStatus(http.StatusOK)
}

func TestHTTPRecorder_WithJSONBody(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		// Echo back Content-Type for verification
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"created":true}`))
	}

	req := Req("POST", "/api/bookmarks", WithJSONBody(map[string]any{
		"title": "Test",
		"url":   "https://example.com",
	}))
	resp := rec.Do(handler, req)
	resp.AssertStatus(http.StatusCreated)
	resp.AssertJSONContains("created", true)
}

func TestHTTPRecorder_WithRawBody(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}

	req := Req("POST", "/text", WithRawBody("text/plain", "hello"))
	resp := rec.Do(handler, req)
	resp.AssertStatus(http.StatusOK)
	resp.AssertBodyContains("hello")
}

func TestHTTPRecorder_WithHeader(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Custom"); got != "value" {
			t.Errorf("expected header X-Custom: value, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}

	req := Req("GET", "/test", WithHeader("X-Custom", "value"))
	resp := rec.Do(handler, req)
	resp.AssertStatus(http.StatusOK)
}

func TestHTTPRecorder_AssertJSON(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"abc","name":"Alice"}`))
	}

	resp := rec.Do(handler, Req("GET", "/users/abc"))
	resp.AssertStatus(http.StatusOK)

	var result struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	resp.AssertJSON(&result)
	if result.ID != "abc" || result.Name != "Alice" {
		t.Errorf("unexpected JSON decode: %+v", result)
	}
}

func TestHTTPRecorder_AssertJSONContains(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"title":"Hello"},"errors":[{"field":"email","message":"required"}]}`))
	}

	resp := rec.Do(handler, Req("GET", "/data"))
	resp.AssertJSONContains("data.title", "Hello")
	resp.AssertJSONContains("errors.0.field", "email")
}

func TestHTTPRecorder_AssertBodyContains(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"bookmark not found"}`))
	}

	resp := rec.Do(handler, Req("GET", "/bookmarks/missing"))
	resp.AssertStatus(http.StatusNotFound)
	resp.AssertBodyContains("not found")
}

func TestHTTPRecorder_Code(t *testing.T) {
	rec := NewHTTP(t)

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}

	resp := rec.Do(handler, Req("DELETE", "/bookmarks/abc"))
	if resp.Code() != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.Code())
	}
}

// HookSpy tests

func TestHookSpy_AssertPublished(t *testing.T) {
	spy := NewHookSpy()
	ctx := context.Background()

	spy.Publish(ctx, core.Event{
		Type:   "after_create",
		Schema: "bookmarks",
		Data:   map[string]any{"id": "abc"},
	})

	spy.AssertPublished(t, "after_create", "bookmarks")
}

func TestHookSpy_AssertNotPublished(t *testing.T) {
	spy := NewHookSpy()
	ctx := context.Background()

	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "bookmarks"})

	spy.AssertNotPublished(t, "before_delete", "bookmarks")
}

func TestHookSpy_AssertSubscribed(t *testing.T) {
	spy := NewHookSpy()

	spy.Subscribe("bookmarks", "after_create", func(ctx context.Context, e core.Event) error {
		return nil
	})

	spy.AssertSubscribed(t, "after_create", "bookmarks")
}

func TestHookSpy_AssertEventCount(t *testing.T) {
	spy := NewHookSpy()
	ctx := context.Background()

	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "a"})
	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "b"})
	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "c"})

	spy.AssertEventCount(t, 3)
}

func TestHookSpy_Reset(t *testing.T) {
	spy := NewHookSpy()
	ctx := context.Background()

	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "a"})
	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "b"})
	spy.Reset()

	if n := spy.PublishedCount(); n != 0 {
		t.Errorf("expected 0 events after Reset, got %d", n)
	}
}

func TestHookSpy_Published(t *testing.T) {
	spy := NewHookSpy()
	ctx := context.Background()

	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "a"})
	spy.Publish(ctx, core.Event{Type: "before_delete", Schema: "b"})

	events := spy.Published()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Schema != "a" {
		t.Errorf("first event schema: got %q, want 'a'", events[0].Schema)
	}
	if events[1].Schema != "b" {
		t.Errorf("second event schema: got %q, want 'b'", events[1].Schema)
	}
}

func TestHookSpy_PublishedCount(t *testing.T) {
	spy := NewHookSpy()
	ctx := context.Background()

	if n := spy.PublishedCount(); n != 0 {
		t.Errorf("expected 0, got %d", n)
	}

	spy.Publish(ctx, core.Event{Type: "after_create", Schema: "a"})

	if n := spy.PublishedCount(); n != 1 {
		t.Errorf("expected 1, got %d", n)
	}
}
