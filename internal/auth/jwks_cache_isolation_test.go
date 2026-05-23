package auth_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

const validJWKSBody = `{"keys":[{"kty":"RSA","kid":"k1","use":"sig",` +
	`"n":"xGOr-H7A-PWG3v1hLyNHnRJ2NSql6EYaJ8Q1VtV6-h0Rr7bDdWJ6vd6C7GnKAtT_9zR8` +
	`FvHm1Zx9UZgkPqjTQnv5Wf4h3xTBBFXcHIsi_S9dGHrmcXQeVxk0hVBiXNlZbDbGSHnW2m0u` +
	`SMVTuXKlZ_TFA7EYWMlbBSMJXnZRt0k","e":"AQAB"}]}`

func serveOn(t *testing.T, addr, body string) *httptest.Server {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot rebind %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	return srv
}

// A JWKS URL is cached for an hour keyed by URL alone, and httptest takes an
// ephemeral port the OS may hand to a later server. A test that reaches a
// recycled address must still see that server's response rather than the
// previous occupant's cached keys.
func TestParseExternal_ARecycledAddressIsNotServedFromACachedKeySet(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(validJWKSBody))
	}))
	addr := first.Listener.Addr().String()
	base := first.URL

	// Populate the cache for this address, then vacate it.
	_, _ = auth.ParseExternal(context.Background(), "any.token", base, base)
	first.Close()

	second := serveOn(t, addr, "not json")
	defer second.Close()

	_, err := auth.ParseExternal(context.Background(), "any.token", jwksURLFor(t, second), base)
	if err == nil {
		t.Fatal("expected an error from the broken JWKS endpoint")
	}
	if !strings.Contains(err.Error(), "jwks:") {
		t.Fatalf("error = %q, want a jwks error; the cached key set from the previous "+
			"occupant of %s shadowed the endpoint", err.Error(), addr)
	}
}
