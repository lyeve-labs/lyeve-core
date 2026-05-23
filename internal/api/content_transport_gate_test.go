//go:build !mutest

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// seedSchema returns a handler reading one definition. The transport gate is
// decided from the definition alone, so the registry it comes from is the
// interface rather than a table.
func seedSchema(t *testing.T, name string, transports core.SchemaTransports) *ContentHandler {
	t.Helper()
	return &ContentHandler{hooks: hooks.NewRegistry(), schemas: newFakeSchemaSource(&core.Schema{
		Name:        name,
		DisplayName: name,
		Fields: []core.SchemaField{
			{Name: "title", FieldType: "text"},
		},
		Transports: transports,
	})}
}

// The field is optional and the default is permissive, so a schema that never
// mentions transports is served on every transport.
func TestContentHandler_CheckTransport_AbsentServesEverything(t *testing.T) {
	h := seedSchema(t, "posts", nil)

	for _, method := range []string{"list", "get", "create", "update", "delete"} {
		rr := httptest.NewRecorder()
		if !h.checkTransport(rr, httptest.NewRequest(http.MethodGet, "/", nil), "posts", method) {
			t.Fatalf("%s refused on a schema with no transports block: %d %s",
				method, rr.Code, rr.Body.String())
		}
	}
}

// A schema off over REST is absent rather than forbidden: 403 would confirm it
// exists to a caller who is not meant to learn that.
func TestContentHandler_CheckTransport_OffIsNotFound(t *testing.T) {
	h := seedSchema(t, "internal_notes", core.SchemaTransports{core.TransportREST: core.TransportOff})

	for _, method := range []string{"list", "get", "create", "delete"} {
		rr := httptest.NewRecorder()
		if h.checkTransport(rr, httptest.NewRequest(http.MethodGet, "/", nil), "internal_notes", method) {
			t.Fatalf("%s served a schema that is off over REST", method)
		}
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", method, rr.Code)
		}
	}
}

func TestContentHandler_CheckTransport_ReadOnlyRefusesWrites(t *testing.T) {
	h := seedSchema(t, "catalog", core.SchemaTransports{core.TransportREST: core.TransportReadOnly})
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	for _, method := range []string{"list", "get"} {
		rr := httptest.NewRecorder()
		if !h.checkTransport(rr, r, "catalog", method) {
			t.Fatalf("%s refused on a read-only schema: %d", method, rr.Code)
		}
	}
	for _, method := range []string{"create", "update", "delete"} {
		rr := httptest.NewRecorder()
		if h.checkTransport(rr, r, "catalog", method) {
			t.Fatalf("%s wrote through a read-only schema", method)
		}
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rr.Code)
		}
	}
}

func TestContentHandler_CheckTransport_WriteOnlyRefusesReads(t *testing.T) {
	h := seedSchema(t, "inbox", core.SchemaTransports{core.TransportREST: core.TransportWriteOnly})
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	rr := httptest.NewRecorder()
	if h.checkTransport(rr, r, "inbox", "list") {
		t.Fatal("list served a write-only schema")
	}
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}

	rr = httptest.NewRecorder()
	if !h.checkTransport(rr, r, "inbox", "create") {
		t.Fatalf("create refused on a write-only schema: %d", rr.Code)
	}
}

// Closing the other two transports must not close REST: publish over REST,
// keep GraphQL shut.
func TestContentHandler_CheckTransport_OtherTransportsDoNotGateREST(t *testing.T) {
	h := seedSchema(t, "articles", core.SchemaTransports{
		core.TransportGraphQL: core.TransportOff,
		core.TransportGRPC:    core.TransportOff,
	})

	for _, method := range []string{"list", "create"} {
		rr := httptest.NewRecorder()
		if !h.checkTransport(rr, httptest.NewRequest(http.MethodGet, "/", nil), "articles", method) {
			t.Fatalf("%s refused over REST because GraphQL and gRPC are off: %d", method, rr.Code)
		}
	}
}

// A schema the store cannot produce is served. The gate narrows a surface that
// is open by default, so a read failure must not close a schema whose
// definition never restricted it.
func TestContentHandler_CheckTransport_UnknownSchemaIsServed(t *testing.T) {
	h := seedSchema(t, "posts", nil)

	rr := httptest.NewRecorder()
	if !h.checkTransport(rr, httptest.NewRequest(http.MethodGet, "/", nil), "no_such_schema", "list") {
		t.Fatalf("a schema the store could not load was closed: %d %s", rr.Code, rr.Body.String())
	}
}
