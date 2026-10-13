package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/schemaimport"
)

// The import reads a bundle of up to four mebibytes, more than the JSON
// documents the rest of the admin API takes. The admin router's JSON body
// guard ran ahead of it, so a bundle over one mebibyte was refused in the
// guard's words and at the guard's figure, and the limit the import documents
// was never reached.
//
// Every request goes through the whole admin router as a signed-in super
// admin, in each media type the router admits on the import's path, once with
// its size declared and once streamed with no length, which is how a piped
// upload arrives.
func TestSchemaImport_ReadsBundlesUpToItsOwnLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	jsonUnder, jsonTypes := importBundle(t, 2<<20, false)
	yamlUnder, yamlTypes := importBundle(t, 2<<20, true)
	jsonOver, _ := importBundle(t, 5<<20, false)
	yamlOver, _ := importBundle(t, 5<<20, true)
	atLimit, atLimitTypes := importBundle(t, maxBundleBytes, true)
	pastLimit, _ := importBundle(t, maxBundleBytes+1, true)

	media := []struct {
		mediaType   string
		under, over []byte
		types       int
	}{
		{"application/json", jsonUnder, jsonOver, jsonTypes},
		{"application/yaml", yamlUnder, yamlOver, yamlTypes},
		{"application/x-yaml", yamlUnder, yamlOver, yamlTypes},
		{"text/yaml", yamlUnder, yamlOver, yamlTypes},
	}

	forEachImportDialect(t, func(t *testing.T, rig *importRig) {
		router := rig.router(t, testConfig())

		for _, m := range media {
			for _, streamed := range []bool{false, true} {
				name := strings.ReplaceAll(m.mediaType, "/", " ")
				if streamed {
					name += " streamed"
				}
				t.Run(name, func(t *testing.T) {
					rec := rig.post(t, router, m.mediaType, m.under, streamed)
					assertPlanned(t, rec, m.types)

					rec = rig.post(t, router, m.mediaType, m.over, streamed)
					assertImportTooLarge(t, rec)
				})
			}
		}

		t.Run("the limit is the import's own", func(t *testing.T) {
			assertPlanned(t, rig.post(t, router, "application/yaml", atLimit, false), atLimitTypes)
			assertImportTooLarge(t, rig.post(t, router, "application/yaml", pastLimit, false))
		})

		// A guard refuses a declared size before reading, so a caller who
		// declares more than it sends gets an answer instead of a hung
		// connection. The import keeps that now that the refusal is its own.
		t.Run("a body declared over the limit is refused unread", func(t *testing.T) {
			src := &countingReader{r: bytes.NewReader(yamlOver)}
			req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas/import", src)
			req.ContentLength = int64(len(yamlOver))
			req.Header.Set("Content-Type", "application/yaml")
			req.Header.Set("Authorization", "Bearer "+rig.session)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			assertImportTooLarge(t, rec)
			assert.Zero(t, src.n, "bytes read from a body already refused")
			assert.Equal(t, "close", rec.Header().Get("Connection"))
		})

		// The import's limit is its own. A route beside it in the same group
		// still refuses a body over the JSON limit, in the guard's words.
		t.Run("a route beside it keeps the JSON limit", func(t *testing.T) {
			rec := rig.postTo(t, router, "/api/admin/users", "application/json", jsonUnder, false)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
			msg, _ := refusalOf(t, rec)
			assert.Equal(t, "Request body exceeds the 1048576 byte limit.", msg)
		})

		// The server's hard limit sits ahead of routing, and an operator may
		// set it below the import's. The import still takes what it documents,
		// the way a plugin route that declares its body does. Past that, the
		// hard limit answers before routing, in its own words but at the
		// import's figure.
		t.Run("a hard limit below the import's still admits it", func(t *testing.T) {
			cfg := testConfig()
			cfg.MaxBodyBytes = 1 << 20
			low := rig.router(t, cfg)
			assertPlanned(t, rig.post(t, low, "application/yaml", yamlUnder, false), yamlTypes)
			assertImportTooLarge(t, rig.post(t, low, "application/yaml", yamlOver, true))

			rec := rig.post(t, low, "application/yaml", yamlOver, false)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
			msg, _ := refusalOf(t, rec)
			assert.Equal(t, fmt.Sprintf("Request body exceeds the %d byte limit.", maxBundleBytes), msg)
		})
	})
}

// importRig is a super admin's session on one real database, for building
// admin routers that carry a schema engine.
type importRig struct {
	pool    db.DB
	session string
}

// forEachImportDialect runs fn against a fresh database on every dialect the
// run tests.
func forEachImportDialect(t *testing.T, fn func(t *testing.T, rig *importRig)) {
	t.Helper()
	dialects := []struct {
		name string
		open func(*testing.T) db.DB
	}{
		{"postgres", testdb.Postgres},
		{"mysql", testdb.MySQL},
		{"mssql", testdb.MSSQL},
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.open(t)
			hash, err := auth.HashPassword("bcrypt", "correct-horse-battery-staple")
			require.NoError(t, err)
			u, err := db.NewUserStore(pool).Create(context.Background(),
				"importer-"+uuid.NewString()[:8]+"@example.com", hash, []string{"super_admin"}, "default")
			require.NoError(t, err)
			session, err := auth.Sign(testConfig().JWTSecret, 3600, u.ID, u.Email, u.Roles, "default", u.TokenVersion)
			require.NoError(t, err)
			fn(t, &importRig{pool: pool, session: session})
		})
	}
}

// router builds the admin router over the rig's database with cfg. Planning
// an import reads the schema engine only, so the engine needs no tables.
func (rg *importRig) router(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	host := testhost.New(rg.pool)
	h, err := NewAdminRouter(rg.pool, cfg,
		WithLifetime(testLifetime(t)),
		WithScalingHost(importHost{Host: host, eng: schemaengine.New(host)}))
	require.NoError(t, err)
	return h
}

// post sends body to the import as mediaType and asks for a plan.
func (rg *importRig) post(t *testing.T, h http.Handler, mediaType string, body []byte, streamed bool) *httptest.ResponseRecorder {
	t.Helper()
	return rg.postTo(t, h, "/api/admin/schemas/import", mediaType, body, streamed)
}

// postTo sends body to path. A streamed body is read from a reader that does
// not report its length, so the request declares none.
func (rg *importRig) postTo(t *testing.T, h http.Handler, path, mediaType string, body []byte, streamed bool) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader = bytes.NewReader(body)
	if streamed {
		r = io.MultiReader(r)
	}
	req := httptest.NewRequest(http.MethodPost, path, r)
	if streamed {
		require.Equal(t, int64(-1), req.ContentLength)
	}
	req.Header.Set("Content-Type", mediaType)
	req.Header.Set("Authorization", "Bearer "+rg.session)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// assertPlanned checks that the import read the whole bundle and planned
// every content type in it, writing nothing.
func assertPlanned(t *testing.T, rec *httptest.ResponseRecorder, types int) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out schemaImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.False(t, out.Applied)
	assert.Len(t, out.Plan.Schemas, types)
	assert.Equal(t, types, out.Plan.Changes(), "every content type is new to the target")
}

// assertImportTooLarge checks that the import refused the body itself, with
// the answer its documentation gives for a body over its limit.
func assertImportTooLarge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	msg, code := refusalOf(t, rec)
	assert.Equal(t, "content type definitions are too large", msg)
	assert.Equal(t, "payload_too_large", code)
}

// countingReader reports how much of a request body was read.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func refusalOf(t *testing.T, rec *httptest.ResponseRecorder) (msg, code string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error, body.Code
}

// importBundle renders a bundle of exactly size bytes, as YAML or as JSON,
// and reports how many content types it holds. The types fill all but the
// last few hundred bytes, and trailing newlines, which both formats read
// past, make up the rest.
func importBundle(t *testing.T, size int, asYAML bool) ([]byte, int) {
	t.Helper()
	render := func(n int) []byte {
		b := schemaimport.Bundle{Version: schemaimport.BundleVersion, Schemas: make([]core.Schema, n)}
		for i := range b.Schemas {
			b.Schemas[i] = importType(i)
		}
		var out []byte
		var err error
		if asYAML {
			out, err = b.MarshalYAML()
		} else {
			out, err = json.Marshal(b)
		}
		require.NoError(t, err)
		return out
	}
	// Every type renders to the same length, so two renders give the size
	// of one type and of the envelope around them.
	one, two := len(render(1)), len(render(2))
	each := two - one
	n := (size - (one - each)) / each
	out := render(n)
	for len(out) > size {
		n--
		out = render(n)
	}
	return append(out, bytes.Repeat([]byte("\n"), size-len(out))...), n
}

// importType is one content type of a generated bundle, with a field of most
// of the types an export carries.
func importType(i int) core.Schema {
	return core.Schema{
		Name:          fmt.Sprintf("catalog_%05d", i),
		DisplayName:   fmt.Sprintf("Catalog %05d", i),
		WithCreatedAt: true,
		WithUpdatedAt: true,
		Fields: []core.SchemaField{
			{Name: "title", FieldType: "text", Required: true},
			{Name: "summary", FieldType: "text"},
			{Name: "body", FieldType: "rich_text"},
			{Name: "contact", FieldType: "email"},
			{Name: "source", FieldType: "url"},
			{Name: "cover", FieldType: "media"},
			{Name: "price", FieldType: "number"},
			{Name: "in_stock", FieldType: "boolean"},
			{Name: "released_on", FieldType: "date"},
			{Name: "published_at", FieldType: "datetime"},
			{Name: "attributes", FieldType: "json"},
		},
	}
}
