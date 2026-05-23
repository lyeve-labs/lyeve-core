package middleware_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

func TestCompress_GzipAccepted(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte(`{"msg":"hello world"}`), 50))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want %q", enc, "gzip")
	}

	if vary := resp.Header.Get("Vary"); vary != "Accept-Encoding" {
		t.Errorf("Vary = %q, want %q", vary, "Accept-Encoding")
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()
	decompressed, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("failed to decompress: %v", err)
	}
	if !bytes.Contains(decompressed, []byte("hello world")) {
		t.Errorf("decompressed body missing original content")
	}
}

func TestCompress_BrotliAccepted(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte(`{"msg":"hello world"}`), 50))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	if enc := resp.Header.Get("Content-Encoding"); enc != "br" {
		t.Fatalf("Content-Encoding = %q, want %q", enc, "br")
	}

	br := brotli.NewReader(resp.Body)
	decompressed, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("failed to decompress brotli: %v", err)
	}
	if !bytes.Contains(decompressed, []byte("hello world")) {
		t.Errorf("decompressed body missing original content")
	}
}

func TestCompress_BrotliPreferredOverGzip(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte(`{"data":"test"}`), 100))
	}))

	// Brotli preferred at equal q-value.
	for _, accept := range []string{"gzip, br", "br, gzip"} {
		t.Run(accept, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			req.Header.Set("Accept-Encoding", accept)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			resp := rec.Result()
			if enc := resp.Header.Get("Content-Encoding"); enc != "br" {
				t.Errorf("Content-Encoding = %q, want %q (brotli should be preferred)", enc, "br")
			}
		})
	}
}

func TestCompress_GzipExplicitQuality(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte(`{"data":"test"}`), 100))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "gzip, br;q=0.5")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q (gzip wins by q-value)", enc, "gzip")
	}
}

func TestCompress_NoEncodingAccepted(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`hello`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (no Accept-Encoding)", enc)
	}
}

func TestCompress_IdentityEncoding(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`hello`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (identity)", enc)
	}
}

func TestCompress_SkipsImages(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("x"), 2000))
	}))

	req := httptest.NewRequest(http.MethodGet, "/img/photo.png", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (images should not be compressed)", enc)
	}
}

func TestCompress_SkipsVideo(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("x"), 2000))
	}))

	req := httptest.NewRequest(http.MethodGet, "/vid/demo.mp4", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (video should not be compressed)", enc)
	}
}

func TestCompress_SkipsAudio(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("x"), 2000))
	}))

	req := httptest.NewRequest(http.MethodGet, "/audio/track.mp3", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (audio should not be compressed)", enc)
	}
}

func TestCompress_SkipsZipContentType(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("x"), 2000))
	}))

	req := httptest.NewRequest(http.MethodGet, "/download/archive.zip", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (zip should not be compressed)", enc)
	}
}

func TestCompress_CompressesTextHTML(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte("<html><body><p>test</p></body></html>"), 10))
	}))

	req := httptest.NewRequest(http.MethodGet, "/page", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q (text/html should be compressed)", enc, "gzip")
	}
}

func TestCompress_SkipsRangeRequests(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(bytes.Repeat([]byte("x"), 2000))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-499")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (range requests should not be compressed)", enc)
	}
}

func TestCompress_SkipsWebSocketUpgrade(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (websocket upgrade)", enc)
	}
}

func TestCompress_ContentTypeWithCharset(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte(`{"key":"value"}`), 50))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q", enc, "gzip")
	}
}

func TestCompress_SkipsErrorStatusCodes(t *testing.T) {
	tests := []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusInternalServerError,
	}

	for _, code := range tests {
		t.Run(http.StatusText(code), func(t *testing.T) {
			mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				w.Write(bytes.Repeat([]byte(`{"error":"test"}`), 50))
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			resp := rec.Result()
			if enc := resp.Header.Get("Content-Encoding"); enc != "" {
				t.Errorf("Content-Encoding = %q for status %d, want empty", enc, code)
			}
		})
	}
}

func TestCompress_RespectsExistingContentEncoding(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip") // pre-compressed at source
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	req.Header.Set("Accept-Encoding", "br") // client wants brotli
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q (should preserve existing)", enc, "gzip")
	}
}

func TestCompress_VaryHeaderAlwaysSet(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if vary := resp.Header.Get("Vary"); vary != "Accept-Encoding" {
		t.Errorf("Vary = %q, want %q", vary, "Accept-Encoding")
	}
}

func TestCompress_CustomLevels(t *testing.T) {
	mw := apimw.CompressWithLevelFunc(func(string, int) (int, int) { return 9, 11 })
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(bytes.Repeat([]byte(`{"data":"compressible payload"}`), 60))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "br" {
		t.Errorf("Content-Encoding = %q, want %q", enc, "br")
	}

	br := brotli.NewReader(resp.Body)
	decompressed, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("failed to decompress brotli: %v", err)
	}
	if !bytes.Contains(decompressed, []byte("compressible")) {
		t.Error("decompressed body missing original content")
	}
}

func TestCompress_WriteBeforeWriteHeader(t *testing.T) {
	// Write before WriteHeader implicitly sends 200.
	mw := apimw.CompressWithLevelFunc(apimw.ContentTypeLevelFunc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bytes.Repeat([]byte(`{"implicit":"200"}`), 50))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q", enc, "gzip")
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()
	decompressed, _ := io.ReadAll(gr)
	if !bytes.Contains(decompressed, []byte("implicit")) {
		t.Error("decompressed body missing original content")
	}
}
