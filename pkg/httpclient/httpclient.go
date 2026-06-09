// Package httpclient provides a shared HTTP client with built-in retry/backoff
// and automatic SSRF protection for the engine and its plugins. Every outbound
// call via DoRequest or DoRequestWithID passes through the SSRF guard unless
// explicitly skipped.
package httpclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/ssrf"
)

// Public API

// DoRequest sends an HTTP request and returns the response. By default it:
//   - Validates the URL against SSRF blocked ranges before dialing
//   - Retries on 5xx responses and network errors up to 3 times
//   - Uses exponential backoff starting at 1s, capped at 30s
//   - Applies a 30s timeout
//
// On context cancellation, no further retries are attempted and the error
// is returned immediately. Failed retries return the last response (or nil
// for network errors).
//
// Options: WithRetries, WithRetryDelay, WithMaxRetryDelay, WithTimeout,
// WithSkipSSRF, WithClient.
func DoRequest(ctx context.Context, req *http.Request, opts ...Option) (*http.Response, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	// SSRF check before the first attempt.
	if !cfg.skipSSRF && req.URL != nil {
		if err := ssrf.ValidateURL(ctx, req.URL.String()); err != nil {
			return nil, fmt.Errorf("httpclient: %w", err)
		}
	}

	client := cfg.client
	if client == nil {
		if cfg.skipSSRF {
			client = &http.Client{Timeout: cfg.timeout}
		} else {
			client = ssrf.NewSafeHTTPClient(cfg.timeout, 3)
		}
	}

	return doWithRetry(ctx, req, client, cfg)
}

// DoRequestWithID is like DoRequest but sets the X-Request-ID header on the
// outgoing request. The requestID is used for correlation across retries.
func DoRequestWithID(ctx context.Context, requestID string, req *http.Request, opts ...Option) (*http.Response, error) {
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	return DoRequest(ctx, req, opts...)
}

// Options

// Config holds the resolved client configuration.
type Config struct {
	maxRetries    int
	retryDelay    time.Duration
	maxRetryDelay time.Duration
	timeout       time.Duration
	skipSSRF      bool
	client        *http.Client
}

// Option modifies the client configuration.
type Option func(*Config)

// WithRetries sets the maximum number of retries. The initial call is not
// counted as a retry. Setting N means up to N additional attempts.
// Default: 3.
func WithRetries(n int) Option {
	return func(c *Config) {
		if n < 0 {
			n = 0
		}
		c.maxRetries = n
	}
}

// WithRetryDelay sets the base delay for exponential backoff.
// Default: 1 second.
func WithRetryDelay(d time.Duration) Option {
	return func(c *Config) {
		if d <= 0 {
			d = 10 * time.Millisecond
		}
		c.retryDelay = d
	}
}

// WithMaxRetryDelay caps the exponential backoff at the given duration.
// Default: 30 seconds.
func WithMaxRetryDelay(d time.Duration) Option {
	return func(c *Config) {
		if d <= 0 {
			d = 10 * time.Millisecond
		}
		c.maxRetryDelay = d
	}
}

// WithTimeout sets the per-request timeout for HTTP requests.
// Default: 30 seconds.
func WithTimeout(d time.Duration) Option {
	return func(c *Config) {
		c.timeout = d
	}
}

// WithSkipSSRF disables SSRF URL validation. Use only for testing
// or for calls to clearly safe internal services.
func WithSkipSSRF(skip bool) Option {
	return func(c *Config) {
		c.skipSSRF = skip
	}
}

// WithClient replaces the default SSRF-safe HTTP client with a custom one.
// When set, the caller is responsible for SSRF protection.
func WithClient(client *http.Client) Option {
	return func(c *Config) {
		c.client = client
	}
}

// Implementation

const (
	defaultMaxRetries    = 3
	defaultRetryDelay    = 1 * time.Second
	defaultMaxRetryDelay = 30 * time.Second
	defaultTimeout       = 30 * time.Second
)

func defaultConfig() Config {
	return Config{
		maxRetries:    defaultMaxRetries,
		retryDelay:    defaultRetryDelay,
		maxRetryDelay: defaultMaxRetryDelay,
		timeout:       defaultTimeout,
	}
}

func doWithRetry(ctx context.Context, req *http.Request, client *http.Client, cfg Config) (*http.Response, error) {
	var lastResp *http.Response

	getBody, err := rewindableBody(req, cfg.maxRetries)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt <= cfg.maxRetries; attempt++ {
		// Check context before each attempt.
		if ctx.Err() != nil {
			// Close any body from the last attempt to avoid leaks.
			if lastResp != nil {
				lastResp.Body.Close()
			}
			return nil, fmt.Errorf("httpclient: context done: %w", ctx.Err())
		}

		// Do consumes the body, and Clone shares the same reader, so every
		// attempt that carries a body gets a fresh reader from getBody.
		// Without it a retried POST sends an empty body.
		reqCopy := req
		if attempt > 0 {
			reqCopy = req.Clone(ctx)
		} else if getBody != nil {
			reqCopy = req.Clone(req.Context())
		}
		if getBody != nil {
			body, err := getBody()
			if err != nil {
				if lastResp != nil {
					lastResp.Body.Close()
				}
				return nil, fmt.Errorf("httpclient: rewind request body: %w", err)
			}
			reqCopy.Body = body
		}

		resp, err := client.Do(reqCopy)

		// Save the most recent response so exhausted-retry paths
		// return the prior response instead of nil.
		if resp != nil {
			lastResp = resp
		}

		if err == nil {
			// Success path: check if status code is retryable.
			if isRetryableStatusCode(resp.StatusCode) && attempt < cfg.maxRetries {
				// Close this response and retry.
				resp.Body.Close()
				backoff := computeBackoff(cfg.retryDelay, attempt+1, cfg.maxRetryDelay)
				if !sleepOrCancel(ctx, backoff) {
					return nil, fmt.Errorf("httpclient: context done during backoff: %w", ctx.Err())
				}
				continue
			}
			// Either not retryable, or last attempt: return as-is.
			return resp, nil
		}

		// Network error path.
		if attempt < cfg.maxRetries {
			if lastResp != nil {
				lastResp.Body.Close()
			}
			backoff := computeBackoff(cfg.retryDelay, attempt+1, cfg.maxRetryDelay)
			if !sleepOrCancel(ctx, backoff) {
				return nil, fmt.Errorf("httpclient: context done during backoff: %w", ctx.Err())
			}
			continue
		}
		return lastResp, fmt.Errorf("httpclient: all %d retries exhausted: %w", cfg.maxRetries+1, err)
	}

	return lastResp, nil
}

// rewindableBody returns a function that yields a fresh copy of the request
// body for each attempt, or nil when the request has no body or will be sent
// only once. A request built by http.NewRequest over a bytes or strings
// reader already carries GetBody. Any other body is read into memory once,
// before the first attempt, because a stream cannot be read a second time.
func rewindableBody(req *http.Request, maxRetries int) (func() (io.ReadCloser, error), error) {
	if req.Body == nil || req.Body == http.NoBody || maxRetries == 0 {
		return nil, nil
	}
	if req.GetBody != nil {
		return req.GetBody, nil
	}
	buf, err := io.ReadAll(req.Body)
	closeErr := req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("httpclient: buffer request body: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("httpclient: close request body: %w", closeErr)
	}
	req.ContentLength = int64(len(buf))
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}, nil
}

// isRetryableStatusCode returns true for status codes where a retry is likely
// to succeed: 5xx server errors and 429 Too Many Requests.
func isRetryableStatusCode(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests
}

// computeBackoff calculates exponential backoff: base * 2^attempt, capped at max.
func computeBackoff(base time.Duration, attempt int, maxDelay time.Duration) time.Duration {
	delay := float64(base) * math.Pow(2, float64(attempt-1))
	d := time.Duration(delay)
	if d > maxDelay {
		d = maxDelay
	}
	return d
}

// sleepOrCancel blocks for d or until ctx is canceled. Returns false when canceled.
func sleepOrCancel(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
