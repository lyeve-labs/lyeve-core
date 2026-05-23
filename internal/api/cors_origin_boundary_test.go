package api

import "testing"

// A suffix match treats the allowlist entry as the tail of the host rather
// than as a domain boundary, so an attacker only has to register a name that
// ends with the allowed one. evilexample.com is not a subdomain of
// example.com, and reflecting it hands the attacker's page a credentialed
// cross-origin channel.
func TestFilterAllowedDynamicOrigins_RequiresADomainBoundary(t *testing.T) {
	allowed := []string{"example.com"}

	refused := []string{
		"https://evilexample.com",
		"https://notexample.com",
		"https://example.com.attacker.net",
		"https://xexample.com:443",
	}
	for _, origin := range refused {
		if got := filterAllowedDynamicOrigins([]string{origin}, allowed); len(got) != 0 {
			t.Errorf("%q must not match the allowlist entry %q, got %v", origin, allowed[0], got)
		}
	}

	accepted := []string{
		"https://example.com",
		"https://tenant.example.com",
		"https://deep.tenant.example.com:8443",
	}
	for _, origin := range accepted {
		if got := filterAllowedDynamicOrigins([]string{origin}, allowed); len(got) != 1 {
			t.Errorf("%q is within %q and must be allowed, got %v", origin, allowed[0], got)
		}
	}
}
