package plugintest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"context"

	"github.com/go-chi/chi/v5"
)

// HTTPRecorder wraps httptest with assertion helpers tailored for plugin
// handler testing. It handles chi URL param injection, JSON body
// encoding/decoding, and status assertions.
type HTTPRecorder struct {
	t T
}

// NewHTTP creates an HTTPRecorder bound to the given test.
func NewHTTP(t T) *HTTPRecorder {
	return &HTTPRecorder{t: t}
}

// ReqOption configures a test HTTP request.
type ReqOption func(req *http.Request)

// Req creates an httptest.NewRequest with the given method, path, and options.
func Req(method, path string, opts ...ReqOption) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	for _, o := range opts {
		o(req)
	}
	return req
}

// WithChiParam injects a chi URL parameter into the request context.
func WithChiParam(key, value string) ReqOption {
	return func(req *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add(key, value)
		req.Header.Set("Content-Type", "application/json")
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		*req = *req.WithContext(ctx)
	}
}

// WithChiParams injects multiple chi URL parameters into the request context.
func WithChiParams(params map[string]string) ReqOption {
	return func(req *http.Request) {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		*req = *req.WithContext(ctx)
	}
}

// WithJSONBody sets the request body to the JSON encoding of v and sets
// Content-Type to application/json. Panics on marshal failure.
func WithJSONBody(v any) ReqOption {
	return func(req *http.Request) {
		b, err := json.Marshal(v)
		if err != nil {
			panic("plugintest.WithJSONBody: marshal failed: " + err.Error())
		}
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(string(b)))
		req.ContentLength = int64(len(b))
	}
}

// WithRawBody sets the request body and Content-Type to the given values.
func WithRawBody(contentType, raw string) ReqOption {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", contentType)
		req.Body = io.NopCloser(strings.NewReader(raw))
		req.ContentLength = int64(len(raw))
	}
}

// WithHeader sets a single request header.
func WithHeader(key, value string) ReqOption {
	return func(req *http.Request) {
		req.Header.Set(key, value)
	}
}

// Response wrapper

// Response wraps httptest.ResponseRecorder with chainable assertion helpers.
type Response struct {
	t  T
	rr *httptest.ResponseRecorder
	// BodyString is the response body as a string, captured once on Do.
	BodyString string
}

// Do executes the handler with the given request and returns a Response.
func (r *HTTPRecorder) Do(handler http.HandlerFunc, req *http.Request) *Response {
	rr := httptest.NewRecorder()
	handler(rr, req)
	body, _ := io.ReadAll(rr.Result().Body)
	return &Response{t: r.t, rr: rr, BodyString: string(body)}
}

// Code returns the HTTP status code of the recorded response.
func (r *Response) Code() int {
	return r.rr.Code
}

// Header returns the response header map.
func (r *Response) Header() http.Header {
	return r.rr.Header()
}

// AssertStatus fails if the response code does not match want.
func (r *Response) AssertStatus(want int) *Response {
	r.t.Helper()
	if r.rr.Code != want {
		r.t.Errorf("expected status %d, got %d: %s", want, r.rr.Code, truncate(r.BodyString, 500))
	}
	return r
}

// AssertJSON decodes the response body as JSON into dest. Fails on invalid JSON.
func (r *Response) AssertJSON(dest any) *Response {
	r.t.Helper()
	if err := json.Unmarshal([]byte(r.BodyString), dest); err != nil {
		r.t.Errorf("expected valid JSON response, got decode error: %v\nBody: %s",
			err, truncate(r.BodyString, 500))
	}
	return r
}

// AssertJSONContains fails if the dot-separated path does not exist or does
// not match want in the JSON response (e.g. "data.title", "errors.0.field").
func (r *Response) AssertJSONContains(path string, want any) *Response {
	r.t.Helper()
	var v any
	if err := json.Unmarshal([]byte(r.BodyString), &v); err != nil {
		r.t.Errorf("expected valid JSON: %v", err)
		return r
	}
	got := extractPath(v, strings.Split(path, "."))
	if got == notFound {
		r.t.Errorf("path %q not found in response: %s", path, truncate(r.BodyString, 500))
		return r
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		r.t.Errorf("path %q: got %s, want %s", path, gotJSON, wantJSON)
	}
	return r
}

// AssertBodyContains fails if substr is not found in the response body string.
func (r *Response) AssertBodyContains(substr string) *Response {
	r.t.Helper()
	if !strings.Contains(r.BodyString, substr) {
		r.t.Errorf("expected body to contain %q, got: %s", substr, truncate(r.BodyString, 500))
	}
	return r
}

// Result returns the underlying httptest.ResponseRecorder.
func (r *Response) Result() *httptest.ResponseRecorder {
	return r.rr
}

// helpers

var notFound = struct{ sentinel bool }{true}

func extractPath(v any, parts []string) any {
	if len(parts) == 0 {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		next, ok := t[parts[0]]
		if !ok {
			return notFound
		}
		return extractPath(next, parts[1:])
	case []any:
		// Numeric index for arrays
		var idx int
		for i := 0; i < len(parts[0]); i++ {
			c := parts[0][i]
			if c >= '0' && c <= '9' {
				idx = idx*10 + int(c-'0')
			} else {
				return notFound
			}
		}
		if len(parts[0]) == 0 {
			return notFound
		}
		if idx < 0 || idx >= len(t) {
			return notFound
		}
		return extractPath(t[idx], parts[1:])
	default:
		return notFound
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
