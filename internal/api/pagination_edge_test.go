package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePagination_EdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		query        string
		wantOffset   int
		wantLimit    int
		wantErr      bool
		wantErrField string
	}{
		{"offset zero explicitly", "offset=0", 0, reqparse.DefaultPageLimit, false, ""},
		{"limit zero accepted", "limit=0", 0, 0, false, ""},
		{"limit exceeds max clamped", "limit=99999", 0, reqparse.MaxPageLimit, false, ""},
		{"negative offset rejected", "offset=-1", 0, 0, true, "offset"},
		{"negative limit rejected", "limit=-10", 0, 0, true, "limit"},
		{"offset zero limit zero", "offset=0&limit=0", 0, 0, false, ""},
		{"offset at large value", "offset=100000", 100000, reqparse.DefaultPageLimit, false, ""},
		{"limit at max bound", "limit=500", 0, reqparse.MaxPageLimit, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			offset, limit, err := reqparse.ParsePagination(r)
			if tt.wantErr {
				require.Error(t, err)
				var pagErr *reqparse.InvalidPaginationError
				assert.ErrorAs(t, err, &pagErr)
				assert.Equal(t, tt.wantErrField, pagErr.Field)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOffset, offset, "offset")
			assert.Equal(t, tt.wantLimit, limit, "limit")
		})
	}
}

type paginatedEnvelope struct {
	Data       []json.RawMessage `json:"data"`
	TotalCount int               `json:"total_count"`
	Offset     int               `json:"offset"`
}

func TestPaginated_EmptyResultSet(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	httpx.Paginated(w, nil, 0, 50, 0)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp paginatedEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Data, "data must be empty array, not null")
	assert.Equal(t, 0, resp.TotalCount)
}

func TestPaginated_OffsetBeyondTotal(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	httpx.Paginated(w, []any{}, 50, 20, 100)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp paginatedEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Data)
	assert.Equal(t, 50, resp.TotalCount)
	assert.Equal(t, 100, resp.Offset)
}

func TestPaginated_TotalCountConsistency(t *testing.T) {
	t.Parallel()

	const total = 47
	const pageSize = 10

	for offset := 0; offset < total; offset += pageSize {
		w := httptest.NewRecorder()
		httpx.Paginated(w, []string{"a"}, total, pageSize, offset)
		var resp paginatedEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, total, resp.TotalCount, "total_count must be stable across pages")
		assert.Equal(t, offset, resp.Offset)
	}
}

func TestDecodeList_RoundTrip(t *testing.T) {
	t.Parallel()

	type item struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	input := []item{{ID: "1", Name: "alpha"}, {ID: "2", Name: "beta"}}

	w := httptest.NewRecorder()
	httpx.Paginated(w, input, len(input), 10, 0)

	decoded, err := httpx.DecodeList[item](strings.NewReader(w.Body.String()))
	require.NoError(t, err)
	assert.Equal(t, input, decoded)
	assert.Contains(t, w.Body.String(), `"total_count":2`)
}
