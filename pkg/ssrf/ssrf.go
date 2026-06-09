// Package ssrf provides reusable SSRF protection for outbound HTTP calls.
// It implements the OWASP SSRF Prevention Cheat Sheet: resolve DNS, check IPs
// against private/internal/loopback/multicast ranges, reject blocked
// destinations at both validate-time and dial-time.
//
// Callers that construct HTTP clients should use NewSafeHTTPClient() or
// NewSafeTransport() to get transport-level blocking + redirect validation.
// Callers that use their own HTTP clients should call ValidateURL() before
// dialing.
//
// Allowlists: call AddAllowlist() with permitted CIDRs before ValidateURL or
// client construction. This allows specific internal targets (e.g. a
// legitimate internal webhook relay) while still blocking everything else.
// Entries added with AddAllowlistScoped() apply only to checks that pass the
// same scope, so plugins with allowlist configuration never weaken each
// other's guards.
package ssrf

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Blocklist: IP ranges that are always blocked unless allowlisted.

// blockedCIDRs is the canonical list of IP ranges the SSRF guard blocks.
// These cover: RFC 1918 private, loopback, link-local (AWS/cloud IMDS),
// IPv6 loopback/unspecified/ULA/link-local, multicast (v4 and v6), reserved,
// and "this network". Every IPv4 range has an IPv6 counterpart: an address
// family that blocks one spelling of the local host but not the other is a
// bypass, not a hardened list.
var blockedCIDRs = []string{
	"10.0.0.0/8",     // RFC 1918 private
	"172.16.0.0/12",  // RFC 1918 private
	"192.168.0.0/16", // RFC 1918 private
	"127.0.0.0/8",    // Loopback
	"169.254.0.0/16", // Link-local (AWS IMDS, Azure IMDS, GCP metadata)
	"0.0.0.0/8",      // "This network" (defense in depth)
	"224.0.0.0/4",    // Multicast (defense in depth)
	"240.0.0.0/4",    // Reserved / Class E (defense in depth)
	"::1/128",        // IPv6 loopback
	"::/128",         // IPv6 unspecified: reaches the local host like 0.0.0.0
	"fc00::/7",       // IPv6 unique local
	"fe80::/10",      // IPv6 link-local
	"ff00::/8",       // IPv6 multicast (defense in depth)
}

// blockedPrefixes is the parsed form of blockedCIDRs, populated at init.
var blockedPrefixes []netip.Prefix

func init() {
	blockedPrefixes = make([]netip.Prefix, len(blockedCIDRs))
	for i, cidr := range blockedCIDRs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			panic(fmt.Sprintf("ssrf: invalid blocked CIDR %q: %v", cidr, err))
		}
		blockedPrefixes[i] = p
	}
}

// Allowlist: permissive overrides for specific internal targets.

var (
	allowlistMu sync.RWMutex
	// allowlists maps a scope name to its CIDR entries. The empty scope is
	// the global allowlist, consulted by every check.
	allowlists = map[string][]netip.Prefix{}
)

// AddAllowlist adds CIDRs that are allowed even if they fall within a blocked
// range. Use for legitimate internal targets like a webhook relay inside the
// same VPC. Thread-safe. Typical usage is once at startup.
//
// Entries are global: every ValidateURL/ValidateAddress call and every safe
// client in the process sees them. A plugin whose allowance must not weaken
// other plugins' guards should use AddAllowlistScoped instead.
func AddAllowlist(cidrs ...string) error {
	return AddAllowlistScoped("", cidrs...)
}

// AddAllowlistScoped adds CIDRs that are allowed only for checks performed
// under the named scope. Scoped entries never affect ValidateURL,
// ValidateAddress, or the safe clients unless the caller explicitly passes
// the same scope, so one plugin's allowance does not open the same targets
// for every other plugin in the process.
func AddAllowlistScoped(scope string, cidrs ...string) error {
	allowlistMu.Lock()
	defer allowlistMu.Unlock()

	for _, cidr := range cidrs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("ssrf: invalid allowlist CIDR %q: %w", cidr, err)
		}
		allowlists[scope] = append(allowlists[scope], p)
	}
	return nil
}

// ClearAllowlist removes all allowlist entries in every scope. Thread-safe.
func ClearAllowlist() {
	allowlistMu.Lock()
	defer allowlistMu.Unlock()
	allowlists = map[string][]netip.Prefix{}
}

func isAllowlisted(scope string, addr netip.Addr) bool {
	allowlistMu.RLock()
	defer allowlistMu.RUnlock()
	// The global scope applies to every check. The named scope adds its own.
	for _, s := range []string{"", scope} {
		for _, pfx := range allowlists[s] {
			if pfx.Contains(addr) {
				return true
			}
		}
	}
	return false
}

// DNS resolution (overridable for tests)

// ResolveFunc is the DNS resolver. Defaults to net.DefaultResolver.
// Override in tests to simulate DNS responses.
var ResolveFunc = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// IP checking

// isBlocked checks whether any IP falls within blocked ranges (and is not
// allowlisted under the given scope). Returns (true, reason) on the first
// blocked match, or (false, "") if all IPs are publicly routable or
// allowlisted.
func isBlocked(scope string, ips []net.IPAddr) (bool, string) {
	for _, ipa := range ips {
		addr, ok := netip.AddrFromSlice(ipa.IP)
		if !ok {
			continue
		}
		// Unwrap IPv4-mapped-IPv6 addresses (e.g. ::ffff:127.0.0.1).
		if addr.Is4In6() {
			addr = addr.Unmap()
		}
		// Allowlist takes precedence over blocklist.
		if isAllowlisted(scope, addr) {
			continue
		}
		for _, pfx := range blockedPrefixes {
			if pfx.Contains(addr) {
				return true, fmt.Sprintf("IP %s is in blocked range %s", addr, pfx)
			}
		}
	}
	return false, ""
}

// URL validation

// ValidateAddress resolves the given host:port address and rejects it if any
// resolved IP falls within a blocked range. Suitable for validating non-HTTP
// destinations (StatsD, SMTP, etc.) against the same SSRF blocklist.
// When port is absent, ValidateAddress parses the addr as host-only.
func ValidateAddress(ctx context.Context, addr string) error {
	return ValidateAddressScoped(ctx, "", addr)
}

// ValidateAddressScoped is ValidateAddress with allowlist entries registered
// under scope applied in addition to the global allowlist.
func ValidateAddressScoped(ctx context.Context, scope, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return fmt.Errorf("ssrf: empty host")
	}

	// Fast path: literal IP.
	if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
		ipAddr := net.IPAddr{IP: net.IP(ip.AsSlice())}
		if blocked, reason := isBlocked(scope, []net.IPAddr{ipAddr}); blocked {
			return fmt.Errorf("ssrf: %s (literal IP %s)", reason, host)
		}
		return nil
	}

	// Hostname path: resolve and validate.
	ips, err := ResolveFunc(ctx, host)
	if err != nil {
		return fmt.Errorf("ssrf: DNS resolution failed for %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("ssrf: no IP addresses resolved for %q", host)
	}
	if blocked, reason := isBlocked(scope, ips); blocked {
		return fmt.Errorf("ssrf: %s (resolved from %q)", reason, host)
	}
	return nil
}

// ValidateURL parses rawURL, resolves the hostname to IP addresses, and
// rejects the URL if any resolved IP falls within a blocked range (private,
// loopback, link-local, multicast, etc.) AND is not in the allowlist.
//
// Returns nil if the URL points to a publicly routable address.
// Returns a descriptive error if the URL is an SSRF risk.
func ValidateURL(ctx context.Context, rawURL string) error {
	return ValidateURLScoped(ctx, "", rawURL)
}

// ValidateURLScoped is ValidateURL with allowlist entries registered under
// scope applied in addition to the global allowlist. Plugins with their own
// allowlist configuration should validate under their own scope so another
// plugin's allowance does not open the same targets for them.
func ValidateURLScoped(ctx context.Context, scope, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("ssrf: invalid URL: %w", err)
	}

	// Scheme allowlist: only http:// and https:// are permitted.
	// Non-HTTP schemes are rejected early with a clear message,
	// even though Go's net/http transport would also reject them
	// (defense in depth: fail fast, fail clearly).
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("ssrf: unsupported URL scheme %q (only http/https allowed)", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("ssrf: URL has no host")
	}

	// Fast path: if host is already a literal IP, validate it directly.
	if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
		ipAddr := net.IPAddr{IP: net.IP(ip.AsSlice())}
		if blocked, reason := isBlocked(scope, []net.IPAddr{ipAddr}); blocked {
			return fmt.Errorf("ssrf: %s (literal IP %s)", reason, host)
		}
		return nil
	}

	// Hostname path: resolve DNS and validate every returned IP.
	ips, err := ResolveFunc(ctx, host)
	if err != nil {
		return fmt.Errorf("ssrf: DNS resolution failed for %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("ssrf: no IP addresses resolved for %q", host)
	}

	if blocked, reason := isBlocked(scope, ips); blocked {
		return fmt.Errorf("ssrf: %s (resolved from %q)", reason, host)
	}

	return nil
}

// Safe HTTP client

// NewSafeTransport returns an *http.Transport with SSRF-safe defaults:
//
//   - Custom DialContext that validates resolved IP addresses at connection
//     time and pins the first public IP (DNS rebinding defense-in-depth).
//   - Sensible timeouts.
func NewSafeTransport() *http.Transport {
	return NewSafeTransportScoped("")
}

// NewSafeTransportScoped is NewSafeTransport with allowlist entries
// registered under scope applied in addition to the global allowlist.
func NewSafeTransportScoped(scope string) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("ssrf dial: invalid address %q: %w", addr, err)
			}

			// Fast path: literal IP.
			if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
				if blocked, reason := isBlocked(scope, []net.IPAddr{{IP: net.IP(ip.AsSlice())}}); blocked {
					return nil, fmt.Errorf("ssrf dial: %s", reason)
				}
				return dialer.DialContext(ctx, network, addr)
			}

			// Hostname: resolve, validate, pin first public IP.
			ips, err := ResolveFunc(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("ssrf dial: DNS failed for %q: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("ssrf dial: no IPs for %q", host)
			}
			if blocked, reason := isBlocked(scope, ips); blocked {
				return nil, fmt.Errorf("ssrf dial: %s", reason)
			}

			// Pin the first resolved IP to prevent DNS rebinding.
			pinned := net.JoinHostPort(ips[0].IP.String(), port)
			return dialer.DialContext(ctx, network, pinned)
		},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewSafeHTTPClient returns an *http.Client with SSRF-safe defaults:
//
//   - Custom DialContext that validates resolved IP addresses at connection
//     time and pins the first public IP (DNS rebinding defense-in-depth).
//   - Custom CheckRedirect that validates every redirect hop against blocked
//     ranges and enforces a cap of maxRedirects hops.
//   - Sensible Transport timeouts.
func NewSafeHTTPClient(baseTimeout time.Duration, maxRedirects int) *http.Client {
	return NewSafeHTTPClientScoped("", baseTimeout, maxRedirects)
}

// NewSafeHTTPClientScoped is NewSafeHTTPClient with allowlist entries
// registered under scope applied in addition to the global allowlist.
func NewSafeHTTPClientScoped(scope string, baseTimeout time.Duration, maxRedirects int) *http.Client {
	return &http.Client{
		Transport: NewSafeTransportScoped(scope),
		Timeout:   baseTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("ssrf: stopped after %d redirects (max %d)", len(via), maxRedirects)
			}
			// Validate the redirect target against blocked ranges.
			if req.URL != nil {
				if err := ValidateURLScoped(req.Context(), scope, req.URL.String()); err != nil {
					return fmt.Errorf("ssrf redirect: %w", err)
				}
			}
			return nil
		},
	}
}
