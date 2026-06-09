package ssrf

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ValidateAddress, literal + resolved host:port validation.

func TestValidateAddress(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"public.relay.test":  {ipAddr("93.184.216.34")},
		"private.relay.test": {ipAddr("10.0.0.5")},
		"empty.relay.test":   {}, // present but resolves to zero IPs
	}))()

	tests := []struct {
		name    string
		addr    string
		wantErr string // "" means expect success. Otherwise an error substring
	}{
		{"literal public ipv4 with port", "8.8.8.8:53", ""},
		{"literal public ipv4 no port", "8.8.8.8", ""},
		{"literal private ipv4 with port", "10.0.0.1:5432", "blocked range"},
		{"literal loopback no port", "127.0.0.1", "blocked range"},
		{"literal ipv6 ula with port", "[fc00::1]:80", "blocked range"},
		{"empty addr", "", "empty host"},
		{"port only, empty host", ":53", "empty host"},
		{"hostname resolves public", "public.relay.test:443", ""},
		{"hostname resolves private", "private.relay.test:443", "blocked range"},
		{"hostname dns failure", "nxdomain.relay.test:80", "DNS resolution failed"},
		{"hostname resolves to no ips", "empty.relay.test:80", "no IP addresses resolved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAddress(context.Background(), tt.addr)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// ValidateURL: remaining error branches (parse, no-host, DNS, empty).

func TestValidateURL_InvalidURL(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://exa\x00mple.com/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid URL")
}

func TestValidateURL_NoHost(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http:///only/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no host")
}

func TestValidateURL_DNSFailure(t *testing.T) {
	defer withResolver(mockResolver(nil))() // any host -> DNSError
	err := ValidateURL(context.Background(), "http://unresolvable.test/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DNS resolution failed")
}

func TestValidateURL_NoIPsResolved(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"empty.test": {},
	}))()
	err := ValidateURL(context.Background(), "http://empty.test/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no IP addresses resolved")
}

// isBlocked: malformed IP slice must be skipped, not matched or panicked on.

func TestIsBlocked_MalformedIPSkipped(t *testing.T) {
	// A 3-byte slice is neither a valid IPv4 (4) nor IPv6 (16) address, so
	// netip.AddrFromSlice returns ok=false and the entry must be skipped.
	blocked, reason := isBlocked("", []net.IPAddr{{IP: net.IP{1, 2, 3}}})
	assert.False(t, blocked)
	assert.Empty(t, reason)
}

// NewSafeTransport DialContext: dial-time validation and pinning.

func TestNewSafeTransport_DialContext(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid address without port", func(t *testing.T) {
		tr := NewSafeTransport()
		_, err := tr.DialContext(ctx, "tcp", "not-an-addr")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid address")
	})

	t.Run("literal blocked ip", func(t *testing.T) {
		tr := NewSafeTransport()
		_, err := tr.DialContext(ctx, "tcp", "127.0.0.1:80")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blocked range")
	})

	t.Run("hostname resolves to blocked ip", func(t *testing.T) {
		defer withResolver(mockResolver(map[string][]net.IPAddr{
			"blocked.test": {ipAddr("10.0.0.9")},
		}))()
		tr := NewSafeTransport()
		_, err := tr.DialContext(ctx, "tcp", "blocked.test:80")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blocked range")
	})

	t.Run("hostname dns failure", func(t *testing.T) {
		defer withResolver(mockResolver(nil))()
		tr := NewSafeTransport()
		_, err := tr.DialContext(ctx, "tcp", "nxdomain.test:80")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DNS failed")
	})

	t.Run("hostname resolves to no ips", func(t *testing.T) {
		defer withResolver(mockResolver(map[string][]net.IPAddr{
			"empty.test": {},
		}))()
		tr := NewSafeTransport()
		_, err := tr.DialContext(ctx, "tcp", "empty.test:80")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no IPs")
	})

	t.Run("literal allowlisted ip dials successfully", func(t *testing.T) {
		ln := newLoopbackListener(t)
		defer ClearAllowlist()
		require.NoError(t, AddAllowlist("127.0.0.0/8"))

		tr := NewSafeTransport()
		conn, err := tr.DialContext(ctx, "tcp", ln.Addr().String())
		require.NoError(t, err)
		require.NotNil(t, conn)
		_ = conn.Close()
	})

	t.Run("hostname resolves to allowlisted ip, pins and dials", func(t *testing.T) {
		ln := newLoopbackListener(t)
		_, port, err := net.SplitHostPort(ln.Addr().String())
		require.NoError(t, err)

		defer ClearAllowlist()
		require.NoError(t, AddAllowlist("127.0.0.0/8"))
		defer withResolver(mockResolver(map[string][]net.IPAddr{
			"pinme.test": {ipAddr("127.0.0.1")},
		}))()

		tr := NewSafeTransport()
		conn, err := tr.DialContext(ctx, "tcp", "pinme.test:"+port)
		require.NoError(t, err)
		require.NotNil(t, conn)
		_ = conn.Close()
	})
}

// newLoopbackListener starts a TCP listener on 127.0.0.1 that accepts and
// immediately closes connections, so the DialContext success paths can be
// exercised deterministically without any external network.
func newLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln
}
