package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
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
