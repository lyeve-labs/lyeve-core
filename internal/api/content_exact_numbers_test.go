package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A JSON number past 2^53 is not a float64. Decoded as one, 9007199254740993
// becomes 9007199254740992 before any rule or hook reads it, and that is what
// would be stored. An integer field keeps it exact from the body to the
// column and back, and a before hook reads the int64.
func TestContentAPI_StoresAnIntegerPast2To53Exactly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"ledgers"}, func(t *testing.T, fx schemaContentFixture) {
		var seen any
		fx.content.hooks.Register("ledgers", hooks.BeforeCreate, func(_ context.Context, e hooks.Event) error {
			seen = e.Data["ref_no"]
			return nil
		})

		body := `{"data":{"ref_no":9007199254740993,"amount":19.9,"meta":{"n":9007199254740993}}}`
		r := httptest.NewRequest(http.MethodPost, "/api/v1/content/ledgers", strings.NewReader(body))
		r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": "ledgers"})
		rr := httptest.NewRecorder()
		fx.content.Create(rr, r)
		id := createdID(t, rr)

		assert.Equal(t, int64(9007199254740993), seen, "the before hook reads an int64")
		assert.Contains(t, rr.Body.String(), `"ref_no":9007199254740993`)
		assert.Contains(t, rr.Body.String(), `"amount":"19.90"`)

		got := fx.get(t, "ledgers", id)
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		assert.Contains(t, got.Body.String(), `"ref_no":9007199254740993`)
		assert.Contains(t, got.Body.String(), `"amount":"19.90"`)
		// A json field is not an integer field, and reads its number as
		// before: a float64, rounded.
		assert.Contains(t, got.Body.String(), `"n":9007199254740992`)
	})
}

// A decimal value with more fractional digits than its scale is refused, not
// rounded: each database rounds it by a rule of its own. A producer writing
// through UpsertContent meets the same refusal as a request, whether the row
// is new or already there, and nothing is written.
func TestContentStore_UpsertRefusesADecimalPastItsScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"ledgers"}, func(t *testing.T, fx schemaContentFixture) {
		store := fx.content.store
		id := uuid.New()
		for _, v := range []any{"1.005", 1.005, json.Number("1.005")} {
			err := store.UpsertContent(fx.ctx, "ledgers", id, map[string]any{"amount": v})
			require.Error(t, err, "%#v", v)
			assert.Equal(t, http.StatusUnprocessableEntity, httpStatus(err), "%#v: %v", v, err)
		}
		_, err := store.GetByID(fx.ctx, "ledgers", id)
		require.True(t, errors.Is(err, domain.ErrNotFound), "a refused insert stored a row: %v", err)

		require.NoError(t, store.UpsertContent(fx.ctx, "ledgers", id, map[string]any{"amount": "2.50"}))
		err = store.UpsertContent(fx.ctx, "ledgers", id, map[string]any{"amount": "1.005"})
		assert.Equal(t, http.StatusUnprocessableEntity, httpStatus(err), "%v", err)
		row, err := store.GetByID(fx.ctx, "ledgers", id)
		require.NoError(t, err)
		assert.Equal(t, "2.50", row.Data["amount"], "a refused update changed the row")
	})
}

// A list filter compares an integer or decimal field with the value parsed by
// the field's type. Bound as text, MySQL compares a BIGINT with a string as a
// double, so a filter on 9007199254740993 would also match 9007199254740992.
// A decimal filter matches by value whatever its scale, 19.9 and 19.90 share
// one cached page, and a value the field cannot hold is the caller's 400.
func TestContentAPI_FiltersIntegerAndDecimalFieldsExactly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"ledgers"}, func(t *testing.T, fx schemaContentFixture) {
		store := fx.content.store
		for _, row := range []map[string]any{
			{"ref_no": int64(9007199254740992), "amount": "5.00"},
			{"ref_no": int64(9007199254740993), "amount": "19.90"},
		} {
			_, err := store.Insert(fx.ctx, "ledgers", row)
			require.NoError(t, err)
		}
		list := func(query string) *httptest.ResponseRecorder {
			return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/ledgers?"+query,
				map[string]string{"schema": "ledgers"}, nil, fx.content.List)
		}
		rows := func(rr *httptest.ResponseRecorder) []map[string]any {
			t.Helper()
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var out []struct {
				Data map[string]any `json:"data"`
			}
			dec := json.NewDecoder(strings.NewReader(rr.Body.String()))
			dec.UseNumber()
			require.NoError(t, dec.Decode(&out), rr.Body.String())
			data := make([]map[string]any, len(out))
			for i, o := range out {
				data[i] = o.Data
			}
			return data
		}

		got := rows(list("filters[ref_no]=9007199254740993"))
		require.Len(t, got, 1)
		assert.Equal(t, json.Number("9007199254740993"), got[0]["ref_no"])

		got = rows(list("filters[amount]=19.9"))
		require.Len(t, got, 1)
		assert.Equal(t, "19.90", got[0]["amount"])
		hits := store.ListCacheHits.Load()
		require.Len(t, rows(list("filters[amount]=19.90")), 1)
		assert.Equal(t, hits+1, store.ListCacheHits.Load(), "19.9 and 19.90 are one cached page")

		for _, q := range []string{"filters[ref_no]=abc", "filters[ref_no]=1.5", "filters[amount]=abc", "filters[amount]=1.005"} {
			assert.Equal(t, http.StatusBadRequest, list(q).Code, q)
		}
	})
}

// flakySchemas fails the one lookup numbered failOn and answers the rest from
// the source it wraps.
type flakySchemas struct {
	core.SchemaSource
	calls  atomic.Int64
	failOn int64
}

func (s *flakySchemas) GetByName(ctx context.Context, name string) (*core.Schema, error) {
	if s.calls.Add(1) == s.failOn {
		return nil, core.ErrServiceUnavailable
	}
	return s.SchemaSource.GetByName(ctx, name)
}

// A write reads its schema once, to choose how its body's numbers decode and
// to validate it. Read twice, a lookup that failed for the decode and
// succeeded for the rules stored a body decoded as floats, rounded. The
// first lookup a create makes is the transport gate's, which serves a schema
// it cannot load, so the write's own lookup is the second.
func TestContentAPI_AFailedSchemaLookupStoresNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"ledgers"}, func(t *testing.T, fx schemaContentFixture) {
		h := &ContentHandler{store: fx.content.store, rules: fx.content.rules, hooks: fx.content.hooks,
			schemas: &flakySchemas{SchemaSource: fx.content.schemas, failOn: 2}}
		rr := fx.send(t, fx.ctx, http.MethodPost, "/api/v1/content/ledgers", map[string]string{"schema": "ledgers"},
			map[string]any{"data": map[string]any{"ref_no": json.Number("9007199254740993")}}, h.Create)
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())

		rows, err := fx.content.store.List(fx.ctx, "ledgers", 10, 0, nil)
		require.NoError(t, err)
		assert.Empty(t, rows, "a write whose schema could not be read stored a row")
	})
}
