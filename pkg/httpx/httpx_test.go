package httpx

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"github.com/stretchr/testify/assert"
)

func TestJSON(t *testing.T) {
	w := httptest.NewRecorder()

	t.Run("with body", func(t *testing.T) {
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		assert.Contains(t, w.Body.String(), `"status":"ok"`)
	})

	t.Run("nil body", func(t *testing.T) {
		w2 := httptest.NewRecorder()
		JSON(w2, http.StatusNoContent, nil)
		assert.Equal(t, http.StatusNoContent, w2.Code)
		assert.Empty(t, w2.Body.String())
	})
}

func TestError(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, http.StatusNotFound, "not found")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), `"error":"not found"`)
	assert.Contains(t, w.Body.String(), `"code":"not_found"`)
}

func TestError_Codes(t *testing.T) {
	cases := []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "bad_request"},
		{http.StatusUnauthorized, "unauthorized"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusNotFound, "not_found"},
		{http.StatusConflict, "conflict"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusInternalServerError, "internal_error"},
		{http.StatusServiceUnavailable, "service_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			w := httptest.NewRecorder()
			Error(w, tc.status, "msg")
			assert.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
		})
	}
}

func TestStatusFor(t *testing.T) {
	assert.Equal(t, http.StatusOK, StatusFor(nil))
	assert.Equal(t, http.StatusNotFound, StatusFor(core.ErrNotFound))
	assert.Equal(t, http.StatusConflict, StatusFor(core.ErrConflict))
	assert.Equal(t, http.StatusInternalServerError, StatusFor(errors.New("db down")))

	// reqparse typed errors map to 400.
	assert.Equal(t, http.StatusBadRequest, StatusFor(&reqparse.InvalidUUIDError{Param: "id", Raw: "xyz"}))
	assert.Equal(t, http.StatusBadRequest, StatusFor(&reqparse.InvalidQueryIntError{Key: "limit", Raw: "abc"}))
	assert.Equal(t, http.StatusBadRequest, StatusFor(&reqparse.InvalidPaginationError{Field: "offset", Raw: "-1"}))
}

func TestStoreStatusFor(t *testing.T) {
	// nil error -> 200
	assert.Equal(t, http.StatusOK, StoreStatusFor(nil))

	// Domain sentinels pass through from StatusFor.
	assert.Equal(t, http.StatusNotFound, StoreStatusFor(core.ErrNotFound))
	assert.Equal(t, http.StatusConflict, StoreStatusFor(core.ErrConflict))

	// reqparse errors also pass through.
	assert.Equal(t, http.StatusBadRequest, StoreStatusFor(&reqparse.InvalidUUIDError{Param: "id", Raw: "xyz"}))

	// Context errors map to 503.
	assert.Equal(t, http.StatusServiceUnavailable, StoreStatusFor(context.DeadlineExceeded))
	assert.Equal(t, http.StatusServiceUnavailable, StoreStatusFor(context.Canceled))

	// sql.ErrConnDone maps to 503.
	assert.Equal(t, http.StatusServiceUnavailable, StoreStatusFor(sql.ErrConnDone))
	assert.Equal(t, http.StatusServiceUnavailable, StoreStatusFor(sql.ErrTxDone))

	// net.ErrClosed maps to 503.
	assert.Equal(t, http.StatusServiceUnavailable, StoreStatusFor(net.ErrClosed))

	// Generic unrecognized errors default to 503 (not 500).
	assert.Equal(t, http.StatusServiceUnavailable, StoreStatusFor(errors.New("some db error")))
}

func TestStoreStatusFor_WrappedDomainErrors(t *testing.T) {
	// Wrapped ErrNotFound still returns 404.
	err := fmt.Errorf("get job: %w", core.ErrNotFound)
	assert.Equal(t, http.StatusNotFound, StoreStatusFor(err))

	// Wrapped ErrConflict still returns 409.
	err = fmt.Errorf("create schedule: %w", core.ErrConflict)
	assert.Equal(t, http.StatusConflict, StoreStatusFor(err))
}

func TestDecodeJSON(t *testing.T) {
	t.Run("valid json", func(t *testing.T) {
		body := strings.NewReader(`{"name":"test","count":42}`)
		r := httptest.NewRequest(http.MethodPost, "/", body)
		var v struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		n, err := DecodeJSON(r, &v)
		assert.NoError(t, err)
		assert.Equal(t, int64(26), n)
		assert.Equal(t, "test", v.Name)
		assert.Equal(t, 42, v.Count)
	})

	t.Run("unknown field rejected", func(t *testing.T) {
		body := strings.NewReader(`{"name":"test","admin":true}`)
		r := httptest.NewRequest(http.MethodPost, "/", body)
		var v struct {
			Name string `json:"name"`
		}
		_, err := DecodeJSON(r, &v)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "decode JSON")
	})

	t.Run("empty body rejected", func(t *testing.T) {
		body := strings.NewReader(``)
		r := httptest.NewRequest(http.MethodPost, "/", body)
		var v struct {
			Name string `json:"name"`
		}
		_, err := DecodeJSON(r, &v)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "decode JSON")
	})

	t.Run("invalid json rejected", func(t *testing.T) {
		body := strings.NewReader(`{invalid}`)
		r := httptest.NewRequest(http.MethodPost, "/", body)
		var v struct {
			Name string `json:"name"`
		}
		_, err := DecodeJSON(r, &v)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "decode JSON")
	})

	t.Run("multiple json values rejected", func(t *testing.T) {
		body := strings.NewReader(`{"name":"a"}{"name":"b"}`)
		r := httptest.NewRequest(http.MethodPost, "/", body)
		var v struct {
			Name string `json:"name"`
		}
		_, err := DecodeJSON(r, &v)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "single JSON value")
	})
}

// A store returning its zero slice hands Paginated a typed nil, and a typed nil
// in an interface is not == nil. Answering "data": null breaks every consumer
// that iterates the list, which is the state a fresh install starts in.
func TestPaginated_NilSliceBecomesEmptyArray(t *testing.T) {
	type row struct {
		ID string `json:"id"`
	}
	for _, tc := range []struct {
		name string
		data any
	}{
		{"untyped nil", nil},
		{"typed nil slice", []row(nil)},
		{"typed nil slice of pointers", []*row(nil)},
		{"nil map", map[string]row(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Paginated(rec, tc.data, 0, 50, 0)

			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got["data"] == nil {
				t.Fatalf("data is null, want an empty array; body=%s", rec.Body.String())
			}
			if arr, ok := got["data"].([]any); !ok || len(arr) != 0 {
				t.Fatalf("data = %#v, want an empty array", got["data"])
			}
		})
	}
}

func TestPaginated_NonEmptySliceIsUnchanged(t *testing.T) {
	type row struct {
		ID string `json:"id"`
	}
	rec := httptest.NewRecorder()
	Paginated(rec, []row{{ID: "a"}, {ID: "b"}}, 2, 50, 0)

	var got struct {
		Data       []row `json:"data"`
		TotalCount int   `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Data) != 2 || got.Data[0].ID != "a" {
		t.Fatalf("data = %#v, want the two rows", got.Data)
	}
	if got.TotalCount != 2 {
		t.Fatalf("total_count = %d, want 2", got.TotalCount)
	}
}
