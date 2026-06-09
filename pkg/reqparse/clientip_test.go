package reqparse

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidUUIDError_Error(t *testing.T) {
	t.Run("missing param (empty raw)", func(t *testing.T) {
		e := &InvalidUUIDError{Param: "id"}
		assert.Equal(t, `reqparse: missing URL parameter "id"`, e.Error())
	})

	t.Run("invalid with wrapped error", func(t *testing.T) {
		e := &InvalidUUIDError{Param: "id", Raw: "xyz", Err: errors.New("boom")}
		assert.Equal(t, `reqparse: invalid UUID for parameter "id": xyz: boom`, e.Error())
	})

	t.Run("invalid without wrapped error", func(t *testing.T) {
		e := &InvalidUUIDError{Param: "id", Raw: "xyz"}
		assert.Equal(t, `reqparse: invalid UUID for parameter "id": xyz`, e.Error())
	})
}

func TestInvalidUUIDError_Unwrap(t *testing.T) {
	sentinel := errors.New("root cause")
	e := &InvalidUUIDError{Param: "id", Raw: "xyz", Err: sentinel}

	assert.ErrorIs(t, e, sentinel)

	var target *InvalidUUIDError
	require.ErrorAs(t, error(e), &target)
	assert.Equal(t, "id", target.Param)

	assert.Equal(t, sentinel, e.Unwrap())
	assert.NoError(t, (&InvalidUUIDError{Param: "id"}).Unwrap())
}

func TestInvalidQueryIntError(t *testing.T) {
	sentinel := errors.New("atoi failed")
	e := &InvalidQueryIntError{Key: "limit", Raw: "abc", Err: sentinel}

	assert.Equal(t, `reqparse: invalid integer for query parameter "limit": abc`, e.Error())
	assert.ErrorIs(t, e, sentinel)
	assert.Equal(t, sentinel, e.Unwrap())
}

func TestInvalidPaginationError_Error(t *testing.T) {
	e := &InvalidPaginationError{Field: "offset", Raw: "-1"}
	assert.Equal(t, `reqparse: invalid pagination parameter "offset": -1`, e.Error())
}

func TestClientIP_ForwardedFor(t *testing.T) {
	t.Run("single xff value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		assert.Equal(t, "203.0.113.7", ClientIP(req))
	})

	t.Run("multiple values returns rightmost", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", "203.0.113.7, 70.41.3.18, 150.172.238.178")
		assert.Equal(t, "150.172.238.178", ClientIP(req))
	})

	t.Run("trailing empty parts are skipped", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", "203.0.113.7, ")
		assert.Equal(t, "203.0.113.7", ClientIP(req))
	})

	t.Run("all-empty xff falls back to remote addr", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "198.51.100.9:5000"
		req.Header.Set("X-Forwarded-For", " , ")
		assert.Equal(t, "198.51.100.9", ClientIP(req))
	})

	t.Run("x-real-ip is not consulted", func(t *testing.T) {
		// ClientIP honors only X-Forwarded-For and RemoteAddr. X-Real-IP is ignored.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "198.51.100.9:5000"
		req.Header.Set("X-Real-IP", "203.0.113.7")
		assert.Equal(t, "198.51.100.9", ClientIP(req))
	})
}

func TestParseCIDRs(t *testing.T) {
	t.Run("valid list parses to networks", func(t *testing.T) {
		got, err := ParseCIDRs([]string{"10.0.0.0/8", "192.168.1.0/24", "203.0.113.5/32"})
		require.NoError(t, err)
		require.Len(t, got, 3)
		assert.Equal(t, "10.0.0.0/8", got[0].String())
		assert.Equal(t, "192.168.1.0/24", got[1].String())
		assert.Equal(t, "203.0.113.5/32", got[2].String())
	})

	t.Run("invalid entry returns error", func(t *testing.T) {
		got, err := ParseCIDRs([]string{"10.0.0.0/8", "not-a-cidr"})
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), `invalid CIDR "not-a-cidr"`)
	})

	t.Run("empty list returns nil", func(t *testing.T) {
		got, err := ParseCIDRs(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("whitespace and empty entries are skipped", func(t *testing.T) {
		got, err := ParseCIDRs([]string{" 10.0.0.0/8 ", "", "  "})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "10.0.0.0/8", got[0].String())
	})

	t.Run("all-blank entries yield empty slice", func(t *testing.T) {
		got, err := ParseCIDRs([]string{"", "   "})
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}
