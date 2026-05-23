package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireJSONContentType(t *testing.T) {
	mw := RequireJSONContentType()

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})

	tests := []struct {
		name         string
		method       string
		contentType  string
		body         string
		wantStatus   int
		wantPassThru bool
	}{
		// Mutation verbs with valid types
		{
			name:         "POST application/json passes",
			method:       http.MethodPost,
			contentType:  "application/json",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "POST application/json; charset=utf-8 passes",
			method:       http.MethodPost,
			contentType:  "application/json; charset=utf-8",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "PUT application/json passes",
			method:       http.MethodPut,
			contentType:  "application/json",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "PATCH application/json passes",
			method:       http.MethodPatch,
			contentType:  "application/json",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "POST multipart/form-data passes",
			method:       http.MethodPost,
			contentType:  "multipart/form-data; boundary=----WebKitFormBoundary",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "PUT multipart/form-data passes",
			method:       http.MethodPut,
			contentType:  "multipart/form-data; boundary=something",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},

		// Mutation verbs with invalid types
		{
			name:        "POST text/plain rejected",
			method:      http.MethodPost,
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "POST application/x-www-form-urlencoded rejected",
			method:      http.MethodPost,
			contentType: "application/x-www-form-urlencoded",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "POST application/xml rejected",
			method:      http.MethodPost,
			contentType: "application/xml",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			// A body with no declared type is the case worth refusing: it is
			// what a cross-origin form can send with no preflight.
			name:       "POST body with empty Content-Type rejected",
			method:     http.MethodPost,
			body:       `{"a":1}`,
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:        "PUT text/html rejected",
			method:      http.MethodPut,
			contentType: "text/html",
			wantStatus:  http.StatusUnsupportedMediaType,
		},
		{
			name:        "PATCH image/png rejected",
			method:      http.MethodPatch,
			contentType: "image/png",
			wantStatus:  http.StatusUnsupportedMediaType,
		},

		// Non-mutation verbs pass through regardless
		{
			name:         "GET passes without Content-Type",
			method:       http.MethodGet,
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "DELETE passes without Content-Type",
			method:       http.MethodDelete,
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "OPTIONS passes without Content-Type",
			method:       http.MethodOptions,
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "HEAD passes without Content-Type",
			method:       http.MethodHead,
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "GET with random Content-Type passes",
			method:       http.MethodGet,
			contentType:  "application/x-www-form-urlencoded",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		// A bare POST carries no entity, so it has no media type to declare.
		// Rollback, reset, archive and restore are all bodyless, and 415 on
		// those refuses a request that was never malformed.
		{
			name:         "POST with no body and no content type passes",
			method:       http.MethodPost,
			contentType:  "",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
		{
			name:         "PUT with no body and no content type passes",
			method:       http.MethodPut,
			contentType:  "",
			wantStatus:   http.StatusOK,
			wantPassThru: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reqBody io.Reader
			if tt.body != "" {
				reqBody = strings.NewReader(tt.body)
			}
			req := httptest.NewRequest(tt.method, "/test", reqBody)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := httptest.NewRecorder()

			handler := mw(okHandler)
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantPassThru {
				if rec.Body.String() != "ok" {
					t.Errorf("expected handler to pass through, got body %q", rec.Body.String())
				}
			} else if tt.wantStatus == http.StatusUnsupportedMediaType {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("error response Content-Type = %q, want application/json", ct)
				}
				if got := rec.Body.String(); got == "ok" {
					t.Error("expected rejection, but handler passed through")
				}
			}
		})
	}
}

// The schema export writes YAML by default and the import parses it, so the
// guard lets application/yaml through on the import path and nowhere else.
func TestRequireJSONContentType_AllowMediaTypesIsScopedToItsPath(t *testing.T) {
	mw := RequireJSONContentType(AllowMediaTypes("/api/admin/schemas/import", "application/yaml", "text/yaml"))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		path, ct string
		want     int
	}{
		{"/api/admin/schemas/import", "application/yaml", http.StatusNoContent},
		{"/api/admin/schemas/import", "Text/YAML; charset=utf-8", http.StatusNoContent},
		{"/api/admin/schemas/import", "text/plain", http.StatusUnsupportedMediaType},
		{"/api/admin/schemas", "application/yaml", http.StatusUnsupportedMediaType},
		{"/api/admin/schemas/import/extra", "application/yaml", http.StatusUnsupportedMediaType},
	} {
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("a: 1"))
		r.Header.Set("Content-Type", tc.ct)
		w := httptest.NewRecorder()
		mw(ok).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s: got %d, want %d", tc.path, tc.ct, w.Code, tc.want)
		}
	}
}

// The guard runs ahead of routing, so a router that knows which route a
// request reaches answers for it. The guard hands that answer the media type
// lower-cased and without parameters, and keeps the rule for anything the
// answer refuses.
func TestRequireJSONContentType_AllowMediaTypesWhenAsksTheCaller(t *testing.T) {
	var asked []string
	mw := RequireJSONContentType(AllowMediaTypesWhen(func(r *http.Request, mediaType string) bool {
		asked = append(asked, mediaType)
		return r.URL.Path == "/acs" && mediaType == "application/x-www-form-urlencoded"
	}), AllowMediaTypesWhen(nil))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, tc := range []struct {
		path, ct string
		want     int
	}{
		{"/acs", "Application/X-WWW-Form-Urlencoded; charset=utf-8", http.StatusNoContent},
		{"/acs", "application/json", http.StatusNoContent},
		{"/acs", "text/plain", http.StatusUnsupportedMediaType},
		{"/other", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
	} {
		r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("a=1"))
		r.Header.Set("Content-Type", tc.ct)
		w := httptest.NewRecorder()
		mw(ok).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s: got %d, want %d", tc.path, tc.ct, w.Code, tc.want)
		}
	}
	if len(asked) == 0 || asked[0] != "application/x-www-form-urlencoded" {
		t.Errorf("admit was asked %q, want the bare lower-case media type first", asked)
	}
}
