package httpclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DefaultMaxResponseBytes bounds a response this engine did not serve. Eight
// mebibytes is far above any API page, error document or token response an
// integration returns, and far below what a process can absorb per request
// without the operator noticing.
const DefaultMaxResponseBytes int64 = 8 << 20

// ErrResponseTooLarge reports that the upstream body reached the limit. It is
// returned instead of the truncated bytes on purpose: half a JSON document
// parses into a plausible value, and a caller that received both would have to
// remember to check which it got.
var ErrResponseTooLarge = errors.New("httpclient: response body exceeds limit")

// ReadBody reads an upstream response body with a ceiling.
//
// Request bodies are already bounded everywhere by http.MaxBytesReader. This
// bounds responses from a server this engine does not run. Several of those
// servers are named by tenant configuration, which makes the size of the reply
// an input a tenant chooses.
//
// A limit of zero or less means DefaultMaxResponseBytes.
//
// The limit is deliberately checked by reading one byte past it rather than by
// trusting Content-Length, which an upstream is free to understate or omit
// entirely on a chunked reply.
func ReadBody(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("httpclient: no response body to read")
	}
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("httpclient: read response body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: over %d bytes", ErrResponseTooLarge, limit)
	}
	return body, nil
}
