package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminToken_ListPassesTheSearchAndStatusToTheStore(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("super_admin")

	before := time.Now()
	query := url.Values{"q": {"  Deploy  "}, "status": {"expired"}}
	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/admin-tokens?" + query.Encode(), bearer: session})
	after := time.Now()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := rg.tokens.lastMatch()
	assert.Equal(t, "Deploy", got.q, "q reaches the store trimmed")
	assert.Equal(t, "expired", got.status)
	assert.False(t, got.now.Before(before) || got.now.After(after), "now is read for the request, got %s", got.now)
}

func TestAdminToken_ListRefusesAnOverlongQueryAndAnUnknownStatus(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	cases := []struct {
		name  string
		query url.Values
		want  string
	}{
		{"q over 200 characters", url.Values{"q": {strings.Repeat("x", 201)}}, "q is longer than 200 characters"},
		{"an unknown status", url.Values{"status": {"pending"}}, "status must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/admin-tokens?" + tc.query.Encode(), bearer: session})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.want)
		})
	}
	assert.Zero(t, rg.tokens.lastMatch().calls, "a refused list never reads the store")
}
