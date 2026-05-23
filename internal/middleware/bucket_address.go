package middleware

import "net"

// ipv6BucketPrefix is how much of an IPv6 address names one client for rate
// limiting. A single host is routinely handed a /64 and can pick any address
// in it, so a limiter keyed on the full address would give it 2^64 fresh
// buckets: a new one per request, and with the bucket cap, a way to evict
// everyone else's. A /64 is the smallest block a host is expected to hold.
const ipv6BucketPrefix = 64

// bucketAddress is the part of a client address a rate limit counts: an IPv4
// address whole, and an IPv6 address by its /64. Anything that does not parse
// is returned unchanged.
func bucketAddress(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return (&net.IPNet{IP: parsed.Mask(net.CIDRMask(ipv6BucketPrefix, 128)), Mask: net.CIDRMask(ipv6BucketPrefix, 128)}).String()
}
