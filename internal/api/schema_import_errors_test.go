package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// importEngine is a schema engine whose Apply fails the way a database does,
// with the driver's own words in the error.
type importEngine struct {
	core.SchemaEngine
	applyErr error
}

func (e importEngine) Apply(context.Context, string, json.RawMessage) error { return e.applyErr }

func (e importEngine) PreviewDDL(context.Context, string, json.RawMessage) ([]core.DDLStatement, error) {
	return nil, nil
}

func (e importEngine) Get(context.Context, string) (json.RawMessage, error) {
	return nil, core.ErrNotFound
}

func (e importEngine) List(context.Context) ([]json.RawMessage, error) { return nil, nil }

type importHost struct {
	core.Host
	eng core.SchemaEngine
}

func (h importHost) Schema() core.SchemaEngine { return h.eng }

func postImport(t *testing.T, eng core.SchemaEngine, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/admin/schemas/import?apply=true", strings.NewReader(body))
	rr := httptest.NewRecorder()
	schemaImportHandler(importHost{eng: eng})(rr, r)
	return rr
}

// A failed apply answers with a fixed message. The engine's error wraps the
// driver's, and the driver names tables, constraints and columns.
func TestSchemaImport_ApplyFailureKeepsDriverTextOut(t *testing.T) {
	driver := errors.New(`apply "post": schema engine: apply DDL: ERROR: relation "_person" does not exist (SQLSTATE 42P01)`)
	rr := postImport(t, importEngine{applyErr: driver},
		`{"version":1,"schemas":[{"name":"post","fields":[{"name":"title","field_type":"text"}]}]}`)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.NotContains(t, rr.Body.String(), "SQLSTATE")
	assert.NotContains(t, rr.Body.String(), "_person")
	assert.NotContains(t, rr.Body.String(), "schema engine")
}

// A refusal of the definition itself is the caller's to fix, and says so with
// its status rather than its text.
func TestSchemaImport_ApplyRefusalIsABadRequest(t *testing.T) {
	rr := postImport(t, importEngine{applyErr: core.ErrValidation},
		`{"version":1,"schemas":[{"name":"post","fields":[{"name":"title","field_type":"text"}]}]}`)
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
}

// An import whose references cannot be satisfied never starts, and the
// answer names the schemas the operator has to fix. Those names came from the
// upload, not from a driver.
func TestSchemaImport_BlockedImportIsAConflict(t *testing.T) {
	rr := postImport(t, importEngine{},
		`{"version":1,"schemas":[{"name":"post","fields":[{"name":"author","field_type":"relation","relation_to":"person","relation_type":"belongs_to"}]}]}`)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "post")
}
