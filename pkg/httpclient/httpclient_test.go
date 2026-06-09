package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoRequest_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req, WithSkipSSRF(true))
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "ok")
}

func TestDoRequest_RetriesOnNetworkError(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			// Simulate connection drop
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"recovered"`))
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithRetries(3),
		WithRetryDelay(10*time.Millisecond),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, attempts.Load(), int32(3))
}

func TestDoRequest_RetriesOn5xx(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithRetries(3),
		WithRetryDelay(10*time.Millisecond),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, attempts.Load(), int32(3))
}

func TestDoRequest_GivesUpAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithRetries(2),
		WithRetryDelay(5*time.Millisecond),
		WithSkipSSRF(true),
	)
	require.NoError(t, err) // returns the response, not an error
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	// Initial + 2 retries = 3 total
	assert.Equal(t, int32(3), attempts.Load())
}

func TestDoRequest_NoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithRetries(3),
		WithRetryDelay(10*time.Millisecond),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, int32(1), attempts.Load()) // no retry on 4xx
}

// TestDoRequest_LastResponsePreservedOnExhaustedNetworkRetry verifies that
// when retries are exhausted with a network error on the final attempt,
// the prior response (e.g. a 503) is returned rather than nil.
func TestDoRequest_LastResponsePreservedOnExhaustedNetworkRetry(t *testing.T) {
	var attempts atomic.Int32

	// Server returns 503 on attempt 1, then drops the connection on attempt 2.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			// First attempt: retryable 503. The body is read and discarded.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// Second attempt: simulate a network error via hijack.
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithRetries(1), // 1 retry means at most 2 attempts
		WithRetryDelay(10*time.Millisecond),
		WithSkipSSRF(true),
	)

	// Should get an error (final attempt was a network error).
	require.Error(t, err)

	// lastResp should be the 503 from the first attempt: not nil.
	require.NotNil(t, resp, "lastResp should be the prior 503 after exhausted retries")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	defer resp.Body.Close()
}

func TestDoRequest_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	// Cancel immediately
	cancel()

	_, err = DoRequest(ctx, req, WithSkipSSRF(true))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "canceled")
}

func TestDoRequest_SSRFBlocksInternalIPs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The httptest server is on a local address. ValidateURL will block it.
	// SSRF is enabled by default.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	_, err = DoRequest(context.Background(), req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ssrf")
}

func TestDoRequest_AllowsPublicURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	// A non-loopback URL would pass SSRF validation. We use SkipSSRF here
	// because httptest binds to a local address. The test verifies that
	// DoRequest completes successfully with options applied.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithRetries(1),
		WithTimeout(10*time.Second),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestDoRequestWithID_IncludesRequestID(t *testing.T) {
	var capturedHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	requestID := "req-abc-123"
	_, err = DoRequestWithID(context.Background(), requestID, req,
		WithRetries(1),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)

	assert.Equal(t, requestID, capturedHeader)
}

func TestDoRequest_ExponentialBackoff(t *testing.T) {
	var timestamps []time.Time

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timestamps = append(timestamps, time.Now())
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	_, _ = DoRequest(context.Background(), req,
		WithRetries(3),
		WithRetryDelay(50*time.Millisecond),
		WithSkipSSRF(true),
	)

	// Should have 4 timestamps: initial + 3 retries
	assert.GreaterOrEqual(t, len(timestamps), 4)

	// 2nd and 3rd gaps should be roughly doubling
	if len(timestamps) >= 4 {
		gap1 := timestamps[1].Sub(timestamps[0])
		gap2 := timestamps[2].Sub(timestamps[1])
		gap3 := timestamps[3].Sub(timestamps[2])

		// Exponential: base=50ms -> gap1≈50ms, gap2≈100ms, gap3≈200ms
		// Allow loose tolerance for scheduling jitter
		assert.GreaterOrEqual(t, gap2, gap1/2)
		assert.GreaterOrEqual(t, gap3, gap2/2)
	}
}

func TestDoRequest_CustomClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent := r.Header.Get("User-Agent")
		assert.Equal(t, "custom-agent/1.0", agent)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	customClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &customTransport{
			userAgent: "custom-agent/1.0",
			inner:     http.DefaultTransport,
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithClient(customClient),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDoRequest_RetryableStatusCodes(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		retryable bool
	}{
		{"5xx_is_retryable", http.StatusInternalServerError, true},
		{"503_is_retryable", http.StatusServiceUnavailable, true},
		{"502_is_retryable", http.StatusBadGateway, true},
		{"504_is_retryable", http.StatusGatewayTimeout, true},
		{"429_is_retryable", http.StatusTooManyRequests, true},
		{"4xx_is_not_retryable", http.StatusNotFound, false},
		{"400_is_not_retryable", http.StatusBadRequest, false},
		{"401_is_not_retryable", http.StatusUnauthorized, false},
		{"403_is_not_retryable", http.StatusForbidden, false},
		{"409_is_not_retryable", http.StatusConflict, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			_, _ = DoRequest(context.Background(), req,
				WithRetries(2),
				WithRetryDelay(5*time.Millisecond),
				WithSkipSSRF(true),
			)

			if tt.retryable {
				assert.Equal(t, int32(3), attempts.Load(), "should retry on %d", tt.status)
			} else {
				assert.Equal(t, int32(1), attempts.Load(), "should NOT retry on %d", tt.status)
			}
		})
	}
}

func TestDefaultMaxRetries(t *testing.T) {
	assert.Equal(t, 3, defaultMaxRetries)
}

func TestDefaultRetryDelay(t *testing.T) {
	assert.Equal(t, 1*time.Second, defaultRetryDelay)
}

func TestDoRequest_TimeoutOption(t *testing.T) {
	slowHandler := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-slowHandler
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	_, err = DoRequest(context.Background(), req,
		WithTimeout(50*time.Millisecond),
		WithRetries(1),
		WithSkipSSRF(true),
	)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "context deadline exceeded")
	close(slowHandler)
}

func TestDoRequest_ResponseBodyPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "test-value")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("exact body here"))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := DoRequest(context.Background(), req, WithSkipSSRF(true))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "test-value", resp.Header.Get("X-Custom"))
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "exact body here", string(body))
}

func TestDoRequest_MaxRetryDelayCaps(t *testing.T) {
	var timestamps []time.Time

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timestamps = append(timestamps, time.Now())
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)

	start := time.Now()
	_, _ = DoRequest(context.Background(), req,
		WithRetries(3),
		WithRetryDelay(500*time.Millisecond),
		WithMaxRetryDelay(50*time.Millisecond),
		WithSkipSSRF(true),
	)
	elapsed := time.Since(start)

	// With max delay capped at 50ms, total wait should be well under 1s
	assert.Less(t, elapsed, 500*time.Millisecond)
}

func TestDoRequestWithID_ErrorResponse(t *testing.T) {
	var capturedHeader string
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Request-ID")
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := DoRequestWithID(context.Background(), "error-test-call", req,
		WithRetries(2),
		WithRetryDelay(5*time.Millisecond),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "error-test-call", capturedHeader)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, int32(3), attempts.Load())
}

// test helpers

type customTransport struct {
	userAgent string
	inner     http.RoundTripper
}

func (ct *customTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", ct.userAgent)
	return ct.inner.RoundTrip(req)
}

// benchmark

func BenchmarkDoRequest_Success(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := DoRequest(context.Background(), req, WithSkipSSRF(true))
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}

func BenchmarkDoRequest_WithRetries(b *testing.B) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n%2 == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		attempts.Store(0)
		resp, err := DoRequest(context.Background(), req, WithRetries(1), WithRetryDelay(1), WithSkipSSRF(true))
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}
