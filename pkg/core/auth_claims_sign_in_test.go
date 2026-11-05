package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A plugin asks whether the person behind a request signed in recently
// before it lets the session add a first second factor. Only a sign-in time
// inside the window counts, a clock a minute ahead included, and a session
// that carries none never does.
func TestAuthClaims_SignedInWithin(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	window := 15 * time.Minute
	at := func(d time.Duration) *AuthClaims { return &AuthClaims{AuthTime: now.Add(d).Unix()} }
	cases := []struct {
		name   string
		claims *AuthClaims
		want   bool
	}{
		{"nil claims", nil, false},
		{"no sign-in time", &AuthClaims{}, false},
		{"inside the window", at(-5 * time.Minute), true},
		{"at the edge", at(-window), true},
		{"outside the window", at(-window - time.Second), false},
		{"a minute ahead", at(time.Minute), true},
		{"two minutes ahead", at(2 * time.Minute), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.claims.signedInWithin(now, window))
		})
	}
}
