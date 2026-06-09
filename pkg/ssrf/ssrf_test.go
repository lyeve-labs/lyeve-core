package ssrf

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// DNS mock helpers

func mockResolver(entries map[string][]net.IPAddr) func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if ips, ok := entries[host]; ok {
			return ips, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
}

func ipAddr(s string) net.IPAddr { return net.IPAddr{IP: net.ParseIP(s)} }

func withResolver(fn func(ctx context.Context, host string) ([]net.IPAddr, error)) func() {
	orig := ResolveFunc
	ResolveFunc = fn
	return func() { ResolveFunc = orig }
}

// ValidateURL: allowed hosts

func TestValidateURL_PublicIPv4(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://8.8.8.8/hook")
	require.NoError(t, err)
}

func TestValidateURL_PublicDomain(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"example.com": {ipAddr("93.184.216.34")},
	}))()
	err := ValidateURL(context.Background(), "https://example.com/hook")
	require.NoError(t, err)
}

// ValidateURL: blocked hosts

func TestValidateURL_RejectsLoopback(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://127.0.0.1/hook")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
}

func TestValidateURL_RejectsIMDS(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://169.254.169.254/latest/meta-data/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "169.254")
}

func TestValidateURL_RejectsLocalhost(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"localhost": {ipAddr("127.0.0.1")},
	}))()
	err := ValidateURL(context.Background(), "http://localhost/hook")
	require.Error(t, err)
	require.Contains(t, err.Error(), "localhost")
}

func TestValidateURL_RejectsPrivate10(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://10.0.0.5/hook")
	require.Error(t, err)
}

func TestValidateURL_RejectsPrivate172_16(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://172.16.0.1/hook")
	require.Error(t, err)
}

func TestValidateURL_RejectsPrivate192_168(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://192.168.1.1/hook")
	require.Error(t, err)
}

func TestValidateURL_RejectsIPv6Loopback(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://[::1]/hook")
	require.Error(t, err)
}

// The unspecified address reaches the local host on Linux exactly as 0.0.0.0
// does, and both spellings have to be refused: the compressed form and the
// fully expanded one parse to the same address but read very differently in a
// blocklist review.
func TestValidateURL_RejectsIPv6Unspecified(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	for _, raw := range []string{
		"http://[::]/hook",
		"http://[0:0:0:0:0:0:0:0]/hook",
	} {
		require.Error(t, ValidateURL(context.Background(), raw), raw)
	}
}

func TestValidateURL_RejectsIPv6Multicast(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	for _, raw := range []string{
		"http://[ff02::1]/hook",
		"http://[ff05::1:3]/hook",
	} {
		require.Error(t, ValidateURL(context.Background(), raw), raw)
	}
}

func TestValidateURL_RejectsIPv6ULA(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://[fc00::1]/hook")
	require.Error(t, err)
}

func TestValidateURL_RejectsIPv6LinkLocal(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://[fe80::1]/hook")
	require.Error(t, err)
}

func TestValidateURL_RejectsIPv4MappedIPv6Loopback(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://[::ffff:127.0.0.1]/hook")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
}

func TestValidateURL_RejectsIPv4MappedIPv6IMDS(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://[::ffff:169.254.169.254]/")
	require.Error(t, err)
}

func TestValidateURL_RejectsDomainResolvingToPrivate(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"evil.internal": {ipAddr("127.0.0.1")},
	}))()
	err := ValidateURL(context.Background(), "http://evil.internal/hook")
	require.Error(t, err)
	require.Contains(t, err.Error(), "evil.internal")
	require.Contains(t, err.Error(), "blocked")
}

// Safe HTTP client: redirect blocking

func TestSafeHTTPClient_BlocksRedirectToLoopback(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"safe.example.com": {ipAddr("93.184.216.34")},
	}))()

	client := NewSafeHTTPClient(5*time.Second, 3)
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	via := []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://safe.example.com/", nil),
	}
	err := client.CheckRedirect(req, via)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ssrf")
}

func TestSafeHTTPClient_BlocksRedirectToIMDS(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"safe.example.com": {ipAddr("93.184.216.34")},
	}))()

	client := NewSafeHTTPClient(5*time.Second, 3)
	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/", nil)
	via := []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://safe.example.com/", nil),
	}
	err := client.CheckRedirect(req, via)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ssrf")
}

func TestSafeHTTPClient_BlocksRedirectToLocalhost(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"safe.example.com": {ipAddr("93.184.216.34")},
		"localhost":        {ipAddr("127.0.0.1")},
	}))()

	client := NewSafeHTTPClient(5*time.Second, 3)
	req, _ := http.NewRequest(http.MethodGet, "http://localhost/", nil)
	via := []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://safe.example.com/", nil),
	}
	err := client.CheckRedirect(req, via)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ssrf")
}

func TestSafeHTTPClient_AllowsRedirectToPublic(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"safe.example.com":  {ipAddr("93.184.216.34")},
		"safe2.example.com": {ipAddr("1.1.1.1")},
	}))()

	client := NewSafeHTTPClient(5*time.Second, 3)
	req, _ := http.NewRequest(http.MethodGet, "https://safe2.example.com/", nil)
	via := []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://safe.example.com/", nil),
	}
	err := client.CheckRedirect(req, via)
	require.NoError(t, err)
}

func TestSafeHTTPClient_LimitsRedirects(t *testing.T) {
	defer withResolver(mockResolver(map[string][]net.IPAddr{
		"a.example.com": {ipAddr("1.1.1.1")},
		"b.example.com": {ipAddr("2.2.2.2")},
		"c.example.com": {ipAddr("3.3.3.3")},
		"d.example.com": {ipAddr("4.4.4.4")},
		"e.example.com": {ipAddr("5.5.5.5")},
	}))()

	client := NewSafeHTTPClient(5*time.Second, 3)

	// 4 redirects should be blocked (4 > maxRedirects=3)
	req, _ := http.NewRequest(http.MethodGet, "https://e.example.com/", nil)
	via := []*http.Request{
		httptest.NewRequest(http.MethodGet, "https://a.example.com/", nil),
		httptest.NewRequest(http.MethodGet, "https://b.example.com/", nil),
		httptest.NewRequest(http.MethodGet, "https://c.example.com/", nil),
		httptest.NewRequest(http.MethodGet, "https://d.example.com/", nil),
	}
	err := client.CheckRedirect(req, via)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stopped after")
}

// Transport: dial-time blocking

func TestSafeHTTPClient_CannotGetLoopback(t *testing.T) {
	client := NewSafeHTTPClient(2*time.Second, 3)
	_, err := client.Get("http://127.0.0.1:9/") // port 9 = discard
	require.Error(t, err)
}

func TestSafeHTTPClient_CannotGetIMDS(t *testing.T) {
	client := NewSafeHTTPClient(2*time.Second, 3)
	_, err := client.Get("http://169.254.169.254/")
	require.Error(t, err)
}

// Allowlist

func TestAllowlist_PermitsSpecificPrivateIP(t *testing.T) {
	defer ClearAllowlist()
	defer withResolver(mockResolver(nil))()

	// Allow 10.0.0.5: a specific internal webhook relay.
	err := AddAllowlist("10.0.0.5/32")
	require.NoError(t, err)

	err = ValidateURL(context.Background(), "http://10.0.0.5/hook")
	require.NoError(t, err, "allowlisted IP should pass")

	// 10.0.0.6 should still be blocked.
	err = ValidateURL(context.Background(), "http://10.0.0.6/hook")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
}

func TestAllowlist_PermitsInternalCIDR(t *testing.T) {
	defer ClearAllowlist()
	defer withResolver(mockResolver(nil))()

	// Allow the entire 10.0.0.0/24 subnet.
	err := AddAllowlist("10.0.0.0/24")
	require.NoError(t, err)

	err = ValidateURL(context.Background(), "http://10.0.0.42/hook")
	require.NoError(t, err)

	// 10.0.1.1 should still be blocked (outside /24).
	err = ValidateURL(context.Background(), "http://10.0.1.1/hook")
	require.Error(t, err)
}

func TestAllowlist_InvalidCIDR(t *testing.T) {
	defer ClearAllowlist()
	err := AddAllowlist("not-a-cidr")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid allowlist CIDR")
}

func TestAllowlist_ScopedDoesNotLeakToOtherScopes(t *testing.T) {
	defer ClearAllowlist()
	defer withResolver(mockResolver(nil))()

	// One plugin allows loopback under its own scope.
	require.NoError(t, AddAllowlistScoped("webhook", "127.0.0.0/8", "::1/128"))

	// Its own checks see the allowance.
	require.NoError(t, ValidateURLScoped(context.Background(), "webhook", "http://127.0.0.1/hook"))
	require.NoError(t, ValidateURLScoped(context.Background(), "webhook", "http://[::1]/hook"))

	// Every other plugin's guard stays strict.
	err := ValidateURL(context.Background(), "http://127.0.0.1/hook")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
	err = ValidateURLScoped(context.Background(), "synthetic-monitoring", "http://localhost/hook")
	require.Error(t, err)

	// A scoped dial-time check also stays strict outside its scope.
	client := NewSafeHTTPClientScoped("webhook", 2*time.Second, 3)
	_, err = client.Get("http://127.0.0.1:1/")
	require.Error(t, err) // connection refused proves the dial was allowed
	strictClient := NewSafeHTTPClient(2*time.Second, 3)
	_, err = strictClient.Get("http://127.0.0.1:1/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")

	// Global entries still apply to every scope.
	require.NoError(t, AddAllowlist("10.0.0.5/32"))
	require.NoError(t, ValidateURLScoped(context.Background(), "any-scope", "http://10.0.0.5/hook"))
}

func TestAllowlist_ClearRemovesAll(t *testing.T) {
	defer withResolver(mockResolver(nil))()

	AddAllowlist("10.0.0.5/32")
	require.NoError(t, ValidateURL(context.Background(), "http://10.0.0.5/hook"))

	ClearAllowlist()
	err := ValidateURL(context.Background(), "http://10.0.0.5/hook")
	require.Error(t, err) // blocked again
}

func TestAllowlist_IMDSStillBlockedByDefault(t *testing.T) {
	defer ClearAllowlist()
	defer withResolver(mockResolver(nil))()

	// Allow only a specific private range.
	err := AddAllowlist("10.0.0.0/24")
	require.NoError(t, err)

	// IMDS should STILL be blocked.
	err = ValidateURL(context.Background(), "http://169.254.169.254/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "169.254")
}

// Adversarial probes

func TestAdversary_IPv4MappedIPv6Private(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://[::ffff:10.0.0.1]/hook")
	require.Error(t, err, "::ffff:10.0.0.1 must be blocked as RFC1918 private")
	require.Contains(t, err.Error(), "blocked")
}

func TestAdversary_UserinfoWithPrivateIP(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://admin:password@127.0.0.1:6379/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
}

func TestAdversary_PublicIPsPass(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	publicURLs := []string{
		"https://1.1.1.1/",
		"https://8.8.8.8/",
		"https://93.184.216.34/",
		"http://[2606:2800:220:1:248:1893:25c8:1946]/",
	}
	for _, u := range publicURLs {
		t.Run(u, func(t *testing.T) {
			err := ValidateURL(context.Background(), u)
			require.NoError(t, err, "public IP %q should pass", u)
		})
	}
}

func TestAdversary_PrivatePorts(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	urls := []string{
		"http://127.0.0.1:6379/",  // Redis
		"http://127.0.0.1:5432/",  // PostgreSQL
		"http://127.0.0.1:27017/", // MongoDB
		"http://169.254.169.254:80/latest/meta-data/",
		"http://10.0.0.1:3000/api",
	}
	for _, u := range urls {
		t.Run(u, func(t *testing.T) {
			err := ValidateURL(context.Background(), u)
			require.Error(t, err, "%q must be blocked", u)
		})
	}
}

func TestAdversary_ZeroIP(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://0.0.0.0/")
	require.Error(t, err)
}

// Scheme allowlist: non-HTTP schemes rejected early

func TestValidateURL_RejectsGopherScheme(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "gopher://127.0.0.1:70/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
	require.Contains(t, err.Error(), "gopher")
}

func TestValidateURL_RejectsFTPScheme(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "ftp://127.0.0.1:21/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
	require.Contains(t, err.Error(), "ftp")
}

func TestValidateURL_RejectsFileScheme(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "file:///etc/passwd")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
	require.Contains(t, err.Error(), "file")
}

func TestValidateURL_RejectsS3Scheme(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "s3://mybucket/key")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
	require.Contains(t, err.Error(), "s3")
}

func TestValidateURL_RejectsUnknownScheme(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "xyznotascheme://8.8.8.8/path")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
	require.Contains(t, err.Error(), "xyznotascheme")
}

func TestValidateURL_AcceptsHTTPScheme(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "http://8.8.8.8/hook")
	require.NoError(t, err)
}

func TestValidateURL_AcceptsHTTPSScheme(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "https://8.8.8.8/hook")
	require.NoError(t, err)
}

func TestValidateURL_RejectsUpperCaseGopher(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	// Schemes are case-insensitive per RFC 3986. Our check lowercases.
	err := ValidateURL(context.Background(), "GOPHER://127.0.0.1:70/")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
}

func TestValidateURL_RejectsSchemeToInternalIP(t *testing.T) {
	// Non-HTTP scheme targeting private IP: should fail fast on scheme,
	// not reach the IP check at all.
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "gopher://10.0.0.1/somepath")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
}

func TestValidateURL_RejectsSchemeToLoopback(t *testing.T) {
	defer withResolver(mockResolver(nil))()
	err := ValidateURL(context.Background(), "ftp://127.0.0.1/somepath")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported URL scheme")
}

func TestSafeHTTPClient_RejectsNonHTTPSchemeAtDialTime(t *testing.T) {
	// Transport-level DialContext already rejects non-HTTP schemes.
	client := NewSafeHTTPClient(2*time.Second, 3)
	_, err := client.Get("ftp://127.0.0.1:21/")
	require.Error(t, err)
	// The error message is Go's net/http "unsupported protocol scheme", not
	// our custom one, because the HTTP client rejects the scheme before dialing.
	require.Contains(t, err.Error(), "unsupported protocol scheme")
}
