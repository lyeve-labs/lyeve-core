package middleware

import (
	"fmt"
	"strings"
)

// skipCompressPrefixes lists MIME-type prefixes for already-compressed or binary content.
var skipCompressPrefixes = []string{
	// Server-Sent Events must never be compressed: a streaming compressor
	// buffers frames until its window fills, so an event stream would deliver
	// nothing until it closed.
	"text/event-stream",
	"image/",
	"video/",
	"audio/",
	"application/zip",
	"application/gzip",
	"application/x-gzip",
	"application/x-bzip2",
	"application/x-xz",
	"application/x-rar-compressed",
	"application/x-7z-compressed",
	"application/x-tar",
	"application/x-compress",
	"application/x-compressed",
	"application/x-zip-compressed",
	"application/octet-stream",
	"application/epub+zip",
	"application/vnd.rar",
	"application/vnd.google-earth.kmz",
	"application/x-msdownload",
	"application/x-shockwave-flash",
	"application/java-archive",
	"model/",
}

// shouldCompress returns false for already-compressed or binary content types.
func shouldCompress(contentType string) bool {
	if ct := strings.ToLower(contentType); ct != "" {
		if idx := strings.IndexByte(ct, ';'); idx != -1 {
			ct = strings.TrimSpace(ct[:idx])
		}
		for _, prefix := range skipCompressPrefixes {
			if strings.HasPrefix(ct, prefix) {
				return false
			}
		}
	}
	return true
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// negotiateEncoding picks br or gzip from Accept-Encoding. Brotli preferred
// at equal quality. Fast path avoids allocation for common single-value headers.
func negotiateEncoding(ae string) (string, bool) {
	switch ae {
	case "gzip", "gzip, deflate":
		return "gzip", true
	case "br":
		return "br", true
	case "br, gzip", "br, gzip, deflate", "gzip, br", "gzip, deflate, br":
		return "br", true
	}

	// Parse q-values for non-trivial Accept-Encoding headers.
	best := struct {
		name    string
		quality float64
	}{quality: -1}

	for _, token := range strings.Split(ae, ",") {
		token = strings.TrimSpace(token)
		parts := strings.SplitN(token, ";", 2)
		name := strings.TrimSpace(parts[0])
		q := 1.0
		if len(parts) == 2 {
			q = parseQuality(parts[1])
		}
		if q == 0 || (name != "br" && name != "gzip") {
			continue
		}
		if q > best.quality || (q == best.quality && name == "br" && best.name == "gzip") {
			best.name = name
			best.quality = q
		}
	}

	return best.name, best.name != ""
}

func parseQuality(param string) float64 {
	param = strings.TrimSpace(param)
	if !strings.HasPrefix(param, "q=") {
		return 1.0
	}
	var q float64
	if _, err := fmt.Sscanf(param[2:], "%f", &q); err != nil {
		return 1.0
	}
	if q < 0 {
		return 0
	}
	if q > 1 {
		return 1
	}
	return q
}
