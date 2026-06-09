package reqparse

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUUID(t *testing.T) {
	validID := uuid.New()

	t.Run("valid uuid", func(t *testing.T) {
		req := chiRequest("GET", "/items/"+validID.String(), map[string]string{"id": validID.String()})
		got, err := ParseUUID(req, "id")
		require.NoError(t, err)
		assert.Equal(t, validID, got)
	})

	t.Run("missing param", func(t *testing.T) {
		req := chiRequest("GET", "/items", nil)
		_, err := ParseUUID(req, "id")
		require.Error(t, err)
		var uuidErr *InvalidUUIDError
		assert.ErrorAs(t, err, &uuidErr)
		assert.Equal(t, "id", uuidErr.Param)
		assert.Empty(t, uuidErr.Raw)
	})

	t.Run("invalid uuid string", func(t *testing.T) {
		req := chiRequest("GET", "/items/not-a-uuid", map[string]string{"id": "not-a-uuid"})
		_, err := ParseUUID(req, "id")
		require.Error(t, err)
		var uuidErr *InvalidUUIDError
		assert.ErrorAs(t, err, &uuidErr)
		assert.Equal(t, "id", uuidErr.Param)
		assert.Equal(t, "not-a-uuid", uuidErr.Raw)
	})

	t.Run("empty uuid string", func(t *testing.T) {
		req := chiRequest("GET", "/items/", map[string]string{"id": ""})
		_, err := ParseUUID(req, "id")
		require.Error(t, err)
		var uuidErr *InvalidUUIDError
		assert.ErrorAs(t, err, &uuidErr)
	})

	t.Run("different param name", func(t *testing.T) {
		req := chiRequest("GET", "/entries/"+validID.String(), map[string]string{"entryID": validID.String()})
		got, err := ParseUUID(req, "entryID")
		require.NoError(t, err)
		assert.Equal(t, validID, got)
	})
}

func TestQueryInt(t *testing.T) {
	t.Run("valid integer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?page=5", nil)
		got, err := QueryInt(req, "page", 0)
		require.NoError(t, err)
		assert.Equal(t, 5, got)
	})

	t.Run("missing param returns default", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		got, err := QueryInt(req, "limit", 20)
		require.NoError(t, err)
		assert.Equal(t, 20, got)
	})

	t.Run("not an integer returns error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?size=abc", nil)
		_, err := QueryInt(req, "size", 0)
		require.Error(t, err)
		var intErr *InvalidQueryIntError
		assert.ErrorAs(t, err, &intErr)
		assert.Equal(t, "size", intErr.Key)
		assert.Equal(t, "abc", intErr.Raw)
	})

	t.Run("negative integer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?offset=-5", nil)
		got, err := QueryInt(req, "offset", 0)
		require.NoError(t, err)
		assert.Equal(t, -5, got)
	})
}

func TestClientIP(t *testing.T) {
	t.Run("ip with port", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		assert.Equal(t, "192.168.1.1", ClientIP(req))
	})

	t.Run("ip without port", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1"
		assert.Equal(t, "10.0.0.1", ClientIP(req))
	})

	t.Run("ipv6", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "[::1]:55443"
		assert.Equal(t, "::1", ClientIP(req))
	})
}

func TestClientIPTrusted(t *testing.T) {
	_, trusted10, _ := net.ParseCIDR("10.0.0.0/8")
	_, trusted172, _ := net.ParseCIDR("172.16.0.0/12")

	t.Run("no xff, no trusted proxies", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		assert.Equal(t, "192.168.1.1", ClientIPTrusted(req, nil))
	})

	t.Run("xff with no trusted proxies ignores xff", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		assert.Equal(t, "192.168.1.1", ClientIPTrusted(req, nil))
	})

	t.Run("xff with empty trusted proxies ignores xff", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		assert.Equal(t, "192.168.1.1", ClientIPTrusted(req, []*net.IPNet{}))
	})

	t.Run("single trusted proxy, one hop", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "203.0.113.10, 10.0.0.1")
		assert.Equal(t, "203.0.113.10", ClientIPTrusted(req, []*net.IPNet{trusted10}))
	})

	t.Run("multi-hop through trusted proxies", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "203.0.113.10, 172.16.1.2, 10.0.0.1")
		assert.Equal(t, "203.0.113.10", ClientIPTrusted(req, []*net.IPNet{trusted10, trusted172}))
	})

	t.Run("spoofed xff from untrusted immediate peer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.168.1.100:54321"
		req.Header.Set("X-Forwarded-For", "10.0.0.99, 192.168.1.100")
		assert.Equal(t, "192.168.1.100", ClientIPTrusted(req, []*net.IPNet{trusted10}))
	})

	t.Run("all xff ips are trusted proxies", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.3:8080"
		req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2, 10.0.0.3")
		assert.Equal(t, "10.0.0.3", ClientIPTrusted(req, []*net.IPNet{trusted10}))
	})

	t.Run("no xff with trusted proxies returns remote addr", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		assert.Equal(t, "10.0.0.1", ClientIPTrusted(req, []*net.IPNet{trusted10}))
	})

	t.Run("malformed ip in xff returns remote addr", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "203.0.113.10, not-an-ip")
		assert.Equal(t, "10.0.0.1", ClientIPTrusted(req, []*net.IPNet{trusted10}))
	})

	t.Run("single xff entry is trusted proxy returns remote addr", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "10.0.0.1")
		assert.Equal(t, "10.0.0.1", ClientIPTrusted(req, []*net.IPNet{trusted10}))
	})

	t.Run("remote addr without port", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.10"
		assert.Equal(t, "203.0.113.10", ClientIPTrusted(req, nil))
	})

	t.Run("ips from different trusted cidr ranges", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "172.16.1.1:8080"
		req.Header.Set("X-Forwarded-For", "203.0.113.10, 172.16.1.1")
		assert.Equal(t, "203.0.113.10", ClientIPTrusted(req, []*net.IPNet{trusted172}))
	})
}

func TestParsePagination(t *testing.T) {
	t.Run("defaults when no params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		offset, limit, err := ParsePagination(req)
		require.NoError(t, err)
		assert.Equal(t, 0, offset)
		assert.Equal(t, DefaultPageLimit, limit)
	})

	t.Run("valid offset and limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?offset=10&limit=50", nil)
		offset, limit, err := ParsePagination(req)
		require.NoError(t, err)
		assert.Equal(t, 10, offset)
		assert.Equal(t, 50, limit)
	})

	t.Run("only offset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?offset=5", nil)
		offset, limit, err := ParsePagination(req)
		require.NoError(t, err)
		assert.Equal(t, 5, offset)
		assert.Equal(t, DefaultPageLimit, limit)
	})

	t.Run("only limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=100", nil)
		offset, limit, err := ParsePagination(req)
		require.NoError(t, err)
		assert.Equal(t, 0, offset)
		assert.Equal(t, 100, limit)
	})

	t.Run("negative offset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?offset=-1", nil)
		_, _, err := ParsePagination(req)
		require.Error(t, err)
		var pagErr *InvalidPaginationError
		assert.ErrorAs(t, err, &pagErr)
		assert.Equal(t, "offset", pagErr.Field)
	})

	t.Run("negative limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=-5", nil)
		_, _, err := ParsePagination(req)
		require.Error(t, err)
		var pagErr *InvalidPaginationError
		assert.ErrorAs(t, err, &pagErr)
		assert.Equal(t, "limit", pagErr.Field)
	})

	t.Run("zero limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=0", nil)
		_, limit, err := ParsePagination(req)
		require.NoError(t, err)
		assert.Equal(t, 0, limit, "limit=0 is valid and means no rows")
	})

	t.Run("non-integer offset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?offset=abc", nil)
		_, _, err := ParsePagination(req)
		require.Error(t, err)
		var pagErr *InvalidPaginationError
		assert.ErrorAs(t, err, &pagErr)
		assert.Equal(t, "offset", pagErr.Field)
	})

	t.Run("non-integer limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=xyz", nil)
		_, _, err := ParsePagination(req)
		require.Error(t, err)
		var pagErr *InvalidPaginationError
		assert.ErrorAs(t, err, &pagErr)
		assert.Equal(t, "limit", pagErr.Field)
	})

	t.Run("limit exceeds max", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=9999", nil)
		_, limit, err := ParsePagination(req)
		require.NoError(t, err)
		assert.Equal(t, MaxPageLimit, limit)
	})

	t.Run("limit at max", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?limit=1000", nil)
		_, limit, err := ParsePagination(req)
		require.NoError(t, err)
		assert.Equal(t, MaxPageLimit, limit)
	})
}

func TestMissingParamError(t *testing.T) {
	err := &MissingParamError{Param: "id"}
	assert.Equal(t, `reqparse: missing required parameter "id"`, err.Error())
}

// chiRequest creates an *http.Request with chi URL params set, matching what
// chi.URLParam() reads at runtime.
func chiRequest(method, target string, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}
	return req
}
