package api

import (
	"net/http"
	"strconv"
	"sync"
)

// contentStreamsPerCredential is how many change streams one credential may
// hold open on one instance. Every write to a schema costs each open stream a
// credential and rule check before its event, so the cap bounds what one
// token or key can make a single write cost, with room for a person who
// keeps the same schema open in several tabs.
const contentStreamsPerCredential = 8

// contentStreamsPerTenant is how many change streams one tenant's callers
// together may hold open on one instance, so a tenant with many accounts or
// keys cannot fill the pool every tenant shares.
const contentStreamsPerTenant = 128

// contentStreamRetryAfter is the Retry-After a refused stream carries, in
// seconds: one heartbeat, about as long as a closed tab takes to free a slot.
const contentStreamRetryAfter = "10"

// streamSlots counts the change streams open on this instance, per tenant and
// per credential. The zero value is ready to use, and a nil one admits every
// stream.
type streamSlots struct {
	mu         sync.Mutex
	tenants    map[string]int
	credential map[string]int
}

// take claims a slot for tenant and credential, and reports false without
// claiming either when one of them is at its cap.
func (s *streamSlots) take(tenant, credential string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tenants == nil {
		s.tenants = map[string]int{}
		s.credential = map[string]int{}
	}
	if s.tenants[tenant] >= contentStreamsPerTenant || s.credential[credential] >= contentStreamsPerCredential {
		return false
	}
	s.tenants[tenant]++
	s.credential[credential]++
	return true
}

// release gives back a slot that take claimed.
func (s *streamSlots) release(tenant, credential string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tenants[tenant]--; s.tenants[tenant] <= 0 {
		delete(s.tenants, tenant)
	}
	if s.credential[credential]--; s.credential[credential] <= 0 {
		delete(s.credential, credential)
	}
}

// streamCredentialKey names the credential a stream counts against: a
// session's subject and token version, an API key's id, or the caller's
// subject for any other token.
func streamCredentialKey(r *http.Request) string {
	if c := claimsFromCtx(r); c != nil {
		return "session:" + c.UserID + ":" + strconv.Itoa(c.TokenVersion)
	}
	c := requestClaims(r)
	switch {
	case c == nil:
		return "none"
	case c.IsAPIKey:
		return "key:" + c.APIKeyID
	default:
		return "user:" + c.UserID
	}
}
