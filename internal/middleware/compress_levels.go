package middleware

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

// Package middleware: per-level compression writer pools.
//
// gzip.NewWriter and brotli.NewWriter fix the level at construction time.
// Per-level pools avoid allocating a fresh writer per request. Levels 4
// (JSON fast) and 6 (default) cover all production use without unbounded
// pool growth.
//
// Both maps are filled from WriteHeader, which runs on every serving goroutine
// at once, and a plain map read racing a plain map write is a fatal error the
// process cannot recover from. sync.Map is the right shape here because the
// key space is the handful of compression levels and the entry for each is
// written once and read forever after.
var gzipWriterPools sync.Map // int level -> *sync.Pool

func gzipWriterPoolForLevel(level int) *sync.Pool {
	if p, ok := gzipWriterPools.Load(level); ok {
		return p.(*sync.Pool)
	}
	// LoadOrStore rather than Store: two goroutines can arrive on the same new
	// level together, and they must leave with the same pool or one set of
	// pooled writers is dropped on the floor.
	p, _ := gzipWriterPools.LoadOrStore(level, &sync.Pool{
		New: func() any {
			w, _ := gzip.NewWriterLevel(io.Discard, level)
			return w
		},
	})
	return p.(*sync.Pool)
}

var brotliWriterPools sync.Map // int level -> *sync.Pool

func brotliWriterPoolForLevel(level int) *sync.Pool {
	if p, ok := brotliWriterPools.Load(level); ok {
		return p.(*sync.Pool)
	}
	p, _ := brotliWriterPools.LoadOrStore(level, &sync.Pool{
		New: func() any {
			return brotli.NewWriterLevel(io.Discard, level)
		},
	})
	return p.(*sync.Pool)
}

// LevelFunc returns a (gzipLevel, brotliLevel) pair for the response.
// Called at WriteHeader time so Content-Type is available.
type LevelFunc func(contentType string, status int) (gzipLevel, brotliLevel int)

// DefaultLevelFunc returns gzip=6, brotli=4 regardless of content type.
func defaultLevelFunc(contentType string, status int) (int, int) {
	return 6, 4
}

// ContentTypeLevelFunc tunes compression by content type:
//   - application/json -> gzip=4, brotli=2  (fast, near-optimal ratio)
//   - text/*            -> gzip=6, brotli=4  (default balance)
//   - everything else   -> gzip=6, brotli=4
//
// Environment overrides (optional):
//
//	COMPRESS_GZIP_JSON_LEVEL - override gzip level for JSON (default: 4)
//	COMPRESS_BROTLI_JSON_LEVEL - override brotli level for JSON (default: 2)
//	COMPRESS_GZIP_TEXT_LEVEL - override gzip level for text/* (default: 6)
//	COMPRESS_BROTLI_TEXT_LEVEL - override brotli level for text/* (default: 4)
//
// Set any to 0 to keep the default. Levels outside valid ranges are clamped.
func ContentTypeLevelFunc(contentType string, status int) (int, int) {
	compressOnce.Do(initCompressLevels)
	jsonGz, jsonBr := compressJSONGzLevel, compressJSONBrLevel
	textGz, textBr := compressTextGzLevel, compressTextBrLevel

	ct := strings.ToLower(contentType)
	if idx := strings.IndexByte(ct, ';'); idx != -1 {
		ct = strings.TrimSpace(ct[:idx])
	}
	if ct == "application/json" || ct == "application/ld+json" ||
		(len(ct) > 5 && strings.HasSuffix(ct, "+json")) {
		return jsonGz, jsonBr
	}
	return textGz, textBr
}

// CompressWithLevelFunc returns middleware that picks compression levels
// per response using the given LevelFunc. The level function is called at
// WriteHeader time when Content-Type has been set by the handler.
//
// A nil LevelFunc compresses at gzip 6 and brotli 4 whatever the content
// type. Use ContentTypeLevelFunc for per-content-type tuning.
func CompressWithLevelFunc(lf LevelFunc) func(http.Handler) http.Handler {
	if lf == nil {
		lf = defaultLevelFunc
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ae := r.Header.Get("Accept-Encoding")
			if ae == "" || ae == "identity" {
				next.ServeHTTP(w, r)
				return
			}

			encoding, ok := negotiateEncoding(ae)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				next.ServeHTTP(w, r)
				return
			}
			if r.Header.Get("Range") != "" {
				next.ServeHTTP(w, r)
				return
			}

			cw := &levelCompressWriter{
				ResponseWriter: w,
				encoding:       encoding,
				levelFunc:      lf,
			}
			w.Header().Set("Vary", "Accept-Encoding")

			next.ServeHTTP(cw, r)

			if cw.writer != nil {
				switch cw.encoding {
				case "gzip":
					gw := cw.writer.(*gzip.Writer)
					_ = gw.Close() // err suppressed: best-effort writer cleanup
					gzipWriterPoolForLevel(cw.gzLevel).Put(gw)
				case "br":
					bw := cw.writer.(*brotli.Writer)
					_ = bw.Close() // err suppressed: best-effort writer cleanup
					brotliWriterPoolForLevel(cw.brLevel).Put(bw)
				}
			}
		})
	}
}

type levelCompressWriter struct {
	http.ResponseWriter
	encoding  string
	levelFunc LevelFunc
	gzLevel   int
	brLevel   int
	writer    io.Writer
	hdrSent   bool
}

// WriteHeader selects compression levels via the LevelFunc and sets
// Content-Encoding before flushing headers for 2xx responses.
func (w *levelCompressWriter) WriteHeader(code int) {
	if w.hdrSent {
		return
	}
	w.hdrSent = true

	if ce := w.Header().Get("Content-Encoding"); ce != "" {
		w.ResponseWriter.WriteHeader(code)
		return
	}

	if code < 200 || code >= 300 {
		w.ResponseWriter.WriteHeader(code)
		return
	}

	if !shouldCompress(w.Header().Get("Content-Type")) {
		w.ResponseWriter.WriteHeader(code)
		return
	}

	w.gzLevel, w.brLevel = w.levelFunc(w.Header().Get("Content-Type"), code)
	w.gzLevel = clamp(w.gzLevel, 1, 9)
	w.brLevel = clamp(w.brLevel, 0, 11)

	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Encoding", w.encoding)

	switch w.encoding {
	case "gzip":
		gw := gzipWriterPoolForLevel(w.gzLevel).Get().(*gzip.Writer)
		gw.Reset(w.ResponseWriter)
		w.writer = gw
	case "br":
		bw := brotliWriterPoolForLevel(w.brLevel).Get().(*brotli.Writer)
		bw.Reset(w.ResponseWriter)
		w.writer = bw
	}

	w.ResponseWriter.WriteHeader(code)
}

// Write compresses the bytes with the level-negotiated encoder or passes
// through when no encoding is active.
func (w *levelCompressWriter) Write(b []byte) (int, error) {
	if !w.hdrSent {
		w.WriteHeader(http.StatusOK)
	}
	if w.writer != nil {
		return w.writer.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush flushes the compressor then the underlying ResponseWriter.
func (w *levelCompressWriter) Flush() {
	if w.writer != nil {
		if f, ok := w.writer.(interface{ Flush() error }); ok {
			_ = f.Flush() // err suppressed: best-effort flush
		}
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (w *levelCompressWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Push delegates to the underlying Pusher if supported.
func (w *levelCompressWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Hijack delegates to the underlying Hijacker if supported.
func (w *levelCompressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("compress(level): underlying ResponseWriter does not implement http.Hijacker: %w", http.ErrNotSupported)
}

var (
	_ http.Flusher  = (*levelCompressWriter)(nil)
	_ http.Pusher   = (*levelCompressWriter)(nil)
	_ http.Hijacker = (*levelCompressWriter)(nil)
)

// compressJSONGzLevel and related vars are per-content-type level overrides
// from env, read in init().
var (
	compressJSONGzLevel int // COMPRESS_GZIP_JSON_LEVEL, default 4
	compressOnce        sync.Once
	compressJSONBrLevel int // COMPRESS_BROTLI_JSON_LEVEL, default 2
	compressTextGzLevel int // COMPRESS_GZIP_TEXT_LEVEL, default 6
	compressTextBrLevel int // COMPRESS_BROTLI_TEXT_LEVEL, default 4
)

func initCompressLevels() {
	compressJSONGzLevel = envCompressLevel("COMPRESS_GZIP_JSON_LEVEL", 4)
	compressJSONBrLevel = envCompressLevel("COMPRESS_BROTLI_JSON_LEVEL", 2)
	compressTextGzLevel = envCompressLevel("COMPRESS_GZIP_TEXT_LEVEL", 6)
	compressTextBrLevel = envCompressLevel("COMPRESS_BROTLI_TEXT_LEVEL", 4)
}

// envCompressLevel reads an int env var and clamps to [1, 11]. A value of 0
// or parse error returns defaultVal. Callers treat 0 as "use default".
func envCompressLevel(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	if n < 1 {
		n = 1
	}
	if n > 11 {
		n = 11
	}
	return n
}
