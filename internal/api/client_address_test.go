package api

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

func proxiedAdminRouter(t *testing.T, tune func(*config.Config)) http.Handler {
	t.Helper()
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	cfg := testConfig()
	tune(cfg)
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg,
		WithLifetime(testLifetime(t)), WithTrustedProxies(apimw.TrustedProxies{trusted}))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	return router
}

func viaProxy(method, path, client string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(`{"email":"a@example.com","password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.2:5000"
	req.Header.Set("X-Forwarded-For", client)
	return req
}

// The per-address limiter runs after the proxy headers are read, so behind a
// proxy each client has its own bucket and no client can spend the others'.
func TestAdminRouter_RateLimitCountsEachClientBehindAProxy(t *testing.T) {
	router := proxiedAdminRouter(t, func(c *config.Config) {
		c.RateLimitRPS = 1
		c.RateLimitBurst = 1
	})

	serve := func(client string) int {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, viaProxy(http.MethodGet, "/api/admin/setup", client))
		return rec.Code
	}
	serve("198.51.100.1")
	if got := serve("198.51.100.1"); got != http.StatusTooManyRequests {
		t.Fatalf("second request from one client = %d, want 429", got)
	}
	if got := serve("198.51.100.2"); got == http.StatusTooManyRequests {
		t.Fatalf("another client behind the same proxy was refused with the first one's budget")
	}
}

// True-Client-IP, X-Real-IP and the leftmost X-Forwarded-For entry are written
// by the client, so the login limit must not key on them.
func TestAdminRouter_LoginLimitIgnoresClientChosenAddresses(t *testing.T) {
	router := proxiedAdminRouter(t, func(c *config.Config) {})

	for attempt := 0; attempt < 40; attempt++ {
		spoofed := "192.0.2." + strconv.Itoa(attempt+1)
		req := viaProxy(http.MethodPost, "/api/admin/auth/login", spoofed+", 198.51.100.7")
		req.Header.Set("True-Client-IP", spoofed)
		req.Header.Set("X-Real-IP", spoofed)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatal("forty logins from one client never reached the login limit")
}

func TestAdminRouter_RefusesAWrongConsoleSignature(t *testing.T) {
	router := proxiedAdminRouter(t, func(c *config.Config) {
		c.AdminConsoleKey = strings.Repeat("k", 32)
	})
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := viaProxy(http.MethodGet, "/api/admin/setup", "198.51.100.1")
	req.Header.Set(apimw.ConsoleClientHeader, "203.0.113.9")
	req.Header.Set(apimw.ConsoleTimeHeader, stamp)
	req.Header.Set(apimw.ConsoleSignatureHeader,
		apimw.ConsoleSignature([]byte(strings.Repeat("x", 32)), stamp, http.MethodGet, "/api/admin/setup", "", "203.0.113.9", ""))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A console behind a Go reverse proxy has its raw characters re-escaped on the
// way in. A signature over the decoded path must survive the trip.
func TestClientAddress_SignatureSurvivesAReverseProxy(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	var seen string
	engine := httptest.NewServer(apimw.ClientAddress(nil, key)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
		w.WriteHeader(http.StatusNoContent)
	})))
	defer engine.Close()
	target, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()

	for _, tc := range []struct{ raw, decoded, query string }{
		{"/api/admin/content/a|b^c", "/api/admin/content/a|b^c", "q=1|2"},
		{"/api/admin/content/a%2Fb", "/api/admin/content/a/b", ""},
		{"/api/admin/content/%C3%A9t%C3%A9", "/api/admin/content/été", "tag=%C3%A9"},
		{"/api/admin/content/[id]", "/api/admin/content/[id]", "f[a]=1"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			seen = ""
			stamp := strconv.FormatInt(time.Now().Unix(), 10)
			sig := apimw.ConsoleSignature(key, stamp, http.MethodGet, tc.decoded, tc.query, "203.0.113.9", "")
			uri := tc.raw
			if tc.query != "" {
				uri += "?" + tc.query
			}
			status := rawGet(t, proxy.Listener.Addr().String(), uri, map[string]string{
				apimw.ConsoleClientHeader:    "203.0.113.9",
				apimw.ConsoleTimeHeader:      stamp,
				apimw.ConsoleSignatureHeader: sig,
			})
			if status != http.StatusNoContent {
				t.Fatalf("status = %d, want 204: the signature did not survive the proxy", status)
			}
			if seen != "203.0.113.9:0" {
				t.Fatalf("engine saw %q, want the signed client", seen)
			}
		})
	}
}

// rawGet writes the request line as given, the way Node's fetch sends a path
// it did not escape, which net/http's client would otherwise re-encode.
func rawGet(t *testing.T, addr, uri string, headers map[string]string) int {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var b strings.Builder
	b.WriteString("GET " + uri + " HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n")
	for k, v := range headers {
		b.WriteString(k + ": " + v + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
