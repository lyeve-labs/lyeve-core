package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestClientAddress(t *testing.T) {
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }

	type seen struct {
		remote, xff, consoleClient string
		reached                    bool
	}
	run := func(mw func(http.Handler) http.Handler, req *http.Request) (*httptest.ResponseRecorder, seen) {
		var got seen
		h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = seen{r.RemoteAddr, r.Header.Get("X-Forwarded-For"), r.Header.Get(ConsoleClientHeader), true}
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, got
	}
	signed := func(signKey []byte, at time.Time, client, auth string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login?next=%2Fadmin", nil)
		req.RemoteAddr = "10.0.0.7:41000"
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		stamp := strconv.FormatInt(at.Unix(), 10)
		req.Header.Set(ConsoleClientHeader, client)
		req.Header.Set(ConsoleTimeHeader, stamp)
		req.Header.Set(ConsoleSignatureHeader,
			ConsoleSignature(signKey, stamp, http.MethodPost, "/api/admin/auth/login", "next=%2Fadmin", client, auth))
		return req
	}

	t.Run("a valid console signature names the client", func(t *testing.T) {
		_, got := run(clientAddress(nil, [][]byte{key}, clock), signed(key, now, "203.0.113.9", "Bearer abc"))
		require.True(t, got.reached)
		assert.Equal(t, "203.0.113.9:0", got.remote)
		assert.Empty(t, got.consoleClient, "console headers must not reach handlers")
	})

	t.Run("a signature under another key is refused", func(t *testing.T) {
		other := []byte("ffffffffffffffffffffffffffffffff")
		rec, got := run(clientAddress(nil, [][]byte{key}, clock), signed(other, now, "203.0.113.9", ""))
		assert.False(t, got.reached)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("a stale signature is refused", func(t *testing.T) {
		rec, got := run(clientAddress(nil, [][]byte{key}, clock), signed(key, now.Add(-2*ConsoleSignatureSkew), "203.0.113.9", ""))
		assert.False(t, got.reached)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("a signature does not carry over to another session", func(t *testing.T) {
		req := signed(key, now, "203.0.113.9", "Bearer abc")
		req.Header.Set("Authorization", "Bearer other")
		rec, got := run(clientAddress(nil, [][]byte{key}, clock), req)
		assert.False(t, got.reached)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("a signature does not carry over to another address", func(t *testing.T) {
		req := signed(key, now, "203.0.113.9", "")
		req.Header.Set(ConsoleClientHeader, "198.51.100.1")
		rec, _ := run(clientAddress(nil, [][]byte{key}, clock), req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("during a rotation the previous key still verifies", func(t *testing.T) {
		previous := []byte("ffffffffffffffffffffffffffffffff")
		mw := clientAddress(nil, [][]byte{key, previous}, clock)
		_, got := run(mw, signed(previous, now, "203.0.113.9", ""))
		assert.Equal(t, "203.0.113.9:0", got.remote)
		_, got = run(mw, signed(key, now, "203.0.113.10", ""))
		assert.Equal(t, "203.0.113.10:0", got.remote)
	})

	t.Run("a mapped IPv4 address shares the plain address's bucket", func(t *testing.T) {
		_, got := run(clientAddress(nil, [][]byte{key}, clock), signed(key, now, "::ffff:203.0.113.9", ""))
		assert.Equal(t, "203.0.113.9:0", got.remote)
	})

	t.Run("with a key set an unsigned proxied caller still resolves through the chain", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.2:5000"
		req.Header.Set("X-Forwarded-For", "198.51.100.20")
		_, got := run(clientAddress([]*net.IPNet{trusted10}, [][]byte{key}, clock), req)
		assert.Equal(t, "198.51.100.20:0", got.remote)
	})

	t.Run("without a key the console headers are ignored and removed", func(t *testing.T) {
		rec, got := run(clientAddress(nil, nil, clock), signed(key, now, "203.0.113.9", ""))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "10.0.0.7:41000", got.remote)
		assert.Empty(t, got.consoleClient)
	})

	t.Run("a trusted proxy chain resolves to the rightmost untrusted entry", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.2:5000"
		req.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.20")
		_, got := run(clientAddress([]*net.IPNet{trusted10}, nil, clock), req)
		assert.Equal(t, "198.51.100.20:0", got.remote, "the leftmost entry is written by the client")
		assert.Empty(t, got.xff, "the chain must not be resolved twice")
	})

	t.Run("an untrusted peer keeps its own address", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.50:5000"
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		_, got := run(clientAddress([]*net.IPNet{trusted10}, nil, clock), req)
		assert.Equal(t, "192.0.2.50:5000", got.remote)
	})

	t.Run("no trust and no key leaves the peer address", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.50:5000"
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		_, got := run(clientAddress(nil, nil, clock), req)
		assert.Equal(t, "192.0.2.50:5000", got.remote)
		assert.Empty(t, got.xff)
	})
}

// The admin console computes the same value in TypeScript. This vector is
// asserted on both sides so the two implementations cannot drift apart.
func TestConsoleSignature_KnownVector(t *testing.T) {
	got := ConsoleSignature([]byte("0123456789abcdef0123456789abcdef"), "1800000000",
		http.MethodPost, "/api/admin/auth/login", "next=%2Fadmin", "203.0.113.9", "Bearer abc")
	assert.Equal(t, "e636068bed4b46f4db44b56c6efd2225e270af6419bd67b04c6366afbd27e761", got)
}

// The host signature has the same two implementations and the same vector.
func TestConsoleHostSignature_KnownVector(t *testing.T) {
	got := ConsoleHostSignature([]byte("0123456789abcdef0123456789abcdef"), "1800000000",
		"e636068bed4b46f4db44b56c6efd2225e270af6419bd67b04c6366afbd27e761", "acme.example.com")
	assert.Equal(t, "50560ffb0df4706696ea4f6aa08d1602c62f0eb79375c25de9e6a52594868c1e", got)
}

// The console calls the engine at an internal address, so the host the
// browser opened travels in its own signed header. Host-based tenant
// resolution reads it through core.RequestHost.
func TestClientAddress_ConsoleHost(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	stamp := strconv.FormatInt(now.Unix(), 10)

	type seen struct {
		host, header string
		reached      bool
	}
	run := func(keys [][]byte, req *http.Request) (*httptest.ResponseRecorder, seen) {
		var got seen
		h := clientAddress(nil, keys, clock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = seen{core.RequestHost(r), r.Header.Get(ConsoleHostHeader) + r.Header.Get(ConsoleHostSignatureHeader), true}
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, got
	}
	// request builds a public read the console forwards, signed under signKey,
	// naming host under hostSig. An empty signKey leaves it unsigned.
	request := func(signKey []byte, host string, hostSig func(reqSig string) string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/brand", nil)
		req.Host = "engine.internal:3001"
		req.RemoteAddr = "10.0.0.7:41000"
		reqSig := ""
		if signKey != nil {
			reqSig = ConsoleSignature(signKey, stamp, http.MethodGet, "/api/admin/auth/brand", "", "203.0.113.9", "")
			req.Header.Set(ConsoleClientHeader, "203.0.113.9")
			req.Header.Set(ConsoleTimeHeader, stamp)
			req.Header.Set(ConsoleSignatureHeader, reqSig)
		}
		if host != "" {
			req.Header.Set(ConsoleHostHeader, host)
		}
		if hostSig != nil {
			req.Header.Set(ConsoleHostSignatureHeader, hostSig(reqSig))
		}
		return req
	}
	signedFor := func(k []byte, host string) func(string) string {
		return func(reqSig string) string { return ConsoleHostSignature(k, stamp, reqSig, host) }
	}

	t.Run("a signed host is the host the request resolves by", func(t *testing.T) {
		rec, got := run([][]byte{key}, request(key, "Acme.Example.com", signedFor(key, "Acme.Example.com")))
		require.True(t, got.reached, rec.Body.String())
		assert.Equal(t, "acme.example.com", got.host)
		assert.Empty(t, got.header, "the host headers do not reach handlers")
	})

	t.Run("a signed request without a host keeps its own", func(t *testing.T) {
		_, got := run([][]byte{key}, request(key, "", nil))
		require.True(t, got.reached)
		assert.Equal(t, "engine.internal:3001", got.host)
	})

	t.Run("an unsigned request cannot name a host", func(t *testing.T) {
		_, got := run([][]byte{key}, request(nil, "acme.example.com", signedFor(key, "acme.example.com")))
		require.True(t, got.reached)
		assert.Equal(t, "engine.internal:3001", got.host)
		assert.Empty(t, got.header)
	})

	refused := map[string]*http.Request{
		"a host with no signature":        request(key, "acme.example.com", nil),
		"a signature with no host":        request(key, "", signedFor(key, "acme.example.com")),
		"a host signed for another host":  request(key, "globex.example.com", signedFor(key, "acme.example.com")),
		"a host signed under another key": request(key, "acme.example.com", signedFor([]byte("ffffffffffffffffffffffffffffffff"), "acme.example.com")),
		"a host bound to another request": request(key, "acme.example.com", func(string) string {
			return ConsoleHostSignature(key, stamp, "another-request", "acme.example.com")
		}),
		"an unparsable host under a forged signature": request(key, "a_b.example", signedFor([]byte("ffffffffffffffffffffffffffffffff"), "a_b.example")),
	}
	for name, req := range refused {
		t.Run(name+" is refused", func(t *testing.T) {
			rec, got := run([][]byte{key}, req)
			assert.False(t, got.reached)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}

	// The browser's URL parser emits names the domain table cannot hold. A
	// signed one is ignored rather than refused, or every request through a
	// console opened at such a name would fail, sign-in included.
	t.Run("a signed host the engine cannot map is ignored, not refused", func(t *testing.T) {
		rec, got := run([][]byte{key}, request(key, "acme.example.com/evil", signedFor(key, "acme.example.com/evil")))
		require.True(t, got.reached, rec.Body.String())
		assert.Equal(t, "engine.internal:3001", got.host)
	})

	for _, h := range []string{"[::1]", "a_b.example", "[::1]:5173"} {
		t.Run("a signed "+h+" is the host", func(t *testing.T) {
			rec, got := run([][]byte{key}, request(key, h, signedFor(key, h)))
			require.True(t, got.reached, rec.Body.String())
			assert.Equal(t, h, got.host)
		})
	}

	t.Run("an engine with no console key ignores the host", func(t *testing.T) {
		_, got := run(nil, request(key, "acme.example.com", signedFor(key, "acme.example.com")))
		require.True(t, got.reached)
		assert.Equal(t, "engine.internal:3001", got.host)
	})
}

func TestValidConsoleHost(t *testing.T) {
	for _, h := range []string{"acme.example.com", "localhost:5173", "10.0.0.5", "[::1]:3001", "[::1]", "a_b.example", "xn--bcher-kva.example"} {
		assert.True(t, validConsoleHost(h), h)
	}
	for _, h := range []string{"", "acme.example.com/path", "a b.example", "acme.example.com:99999", "[10.0.0.5]", "[::1", ".example.com", "evil.com@acme.example.com"} {
		assert.False(t, validConsoleHost(h), h)
	}
}
