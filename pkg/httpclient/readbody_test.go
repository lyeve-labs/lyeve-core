package httpclient_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/httpclient"
)

// endlessBody stands in for an upstream that never stops sending. A fixed
// string cannot show the difference between a bounded read and an unbounded
// one, because both finish.
type endlessBody struct{ n int64 }

func (b *endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	b.n += int64(len(p))
	return len(p), nil
}

func (b *endlessBody) Close() error { return nil }

func TestReadBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    io.ReadCloser
		limit   int64
		want    string
		wantErr error
	}{
		{
			name:  "returns a body inside the limit",
			body:  io.NopCloser(strings.NewReader("hello")),
			limit: 16,
			want:  "hello",
		},
		{
			name:  "returns a body exactly on the limit",
			body:  io.NopCloser(strings.NewReader("hello")),
			limit: 5,
			want:  "hello",
		},
		{
			name:    "refuses a body one byte over the limit",
			body:    io.NopCloser(strings.NewReader("hello!")),
			limit:   5,
			wantErr: httpclient.ErrResponseTooLarge,
		},
		{
			name:  "returns an empty body",
			body:  io.NopCloser(strings.NewReader("")),
			limit: 8,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := httpclient.ReadBody(&http.Response{Body: tt.body}, tt.limit)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("body = %q, want nil: a truncated body parses into a plausible value", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

// The point of the ceiling is that a sender who never stops cannot make the
// process grow without bound, so the read has to stop on its own.
func TestReadBody_StopsReadingAnEndlessSender(t *testing.T) {
	t.Parallel()

	body := &endlessBody{}
	_, err := httpclient.ReadBody(&http.Response{Body: body}, 1024)

	if !errors.Is(err, httpclient.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	// One page of slack over the limit covers the reader's buffer growth.
	if body.n > 1024+32*1024 {
		t.Errorf("read %d bytes from an endless sender for a 1024 byte limit", body.n)
	}
}

func TestReadBody_DefaultsTheLimitWhenUnset(t *testing.T) {
	t.Parallel()

	body := &endlessBody{}
	_, err := httpclient.ReadBody(&http.Response{Body: body}, 0)

	if !errors.Is(err, httpclient.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	if body.n > httpclient.DefaultMaxResponseBytes+32*1024 {
		t.Errorf("read %d bytes, want a stop near the default ceiling", body.n)
	}
}

func TestReadBody_ReportsAMissingBody(t *testing.T) {
	t.Parallel()

	if _, err := httpclient.ReadBody(nil, 16); err == nil {
		t.Error("a nil response should report an error rather than an empty body")
	}
	if _, err := httpclient.ReadBody(&http.Response{}, 16); err == nil {
		t.Error("a nil body should report an error rather than an empty body")
	}
}
