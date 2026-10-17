package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// documentedOperation is what a client reads from one operation of the
// document: its parameters, the credentials and roles it asks for, its
// request body and the media types of its responses.
type documentedOperation struct {
	Parameters []struct {
		Name   string         `json:"name"`
		In     string         `json:"in"`
		Schema map[string]any `json:"schema"`
	} `json:"parameters"`
	Security    []map[string][]string `json:"security"`
	Roles       []string              `json:"x-roles"`
	RequestBody *struct {
		Required bool                       `json:"required"`
		Content  map[string]json.RawMessage `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Description string                     `json:"description"`
		Content     map[string]json.RawMessage `json:"content"`
	} `json:"responses"`
}

type documentedPaths struct {
	Paths map[string]map[string]documentedOperation `json:"paths"`
}

// documentedPathsOf reads a built document the way a client reads the served
// one, from its JSON.
func documentedPathsOf(t *testing.T, doc openAPIDoc) documentedPaths {
	t.Helper()
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var out documentedPaths
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

const contentStreamPath = "/api/v1/content/{schema}/stream"

// streamEventShape is the event the stream entry declares: the keys of the
// JSON object each data line holds, and the names its event key takes.
type streamEventShape struct {
	keys   []string
	events []string
}

// declaredStreamEvent reads the event shape from the stream entry's
// text/event-stream schema.
func declaredStreamEvent(t *testing.T, doc documentedPaths) streamEventShape {
	t.Helper()
	op, ok := doc.Paths[contentStreamPath]["get"]
	require.True(t, ok, "the change stream is not documented")
	raw, ok := op.Responses["200"].Content["text/event-stream"]
	require.True(t, ok, "the change stream's 200 declares no text/event-stream content")
	type eventObject struct {
		Required   []string `json:"required"`
		Properties struct {
			Event struct {
				Enum []string `json:"enum"`
			} `json:"event"`
		} `json:"properties"`
	}
	var media struct {
		Schema struct {
			Required   []string `json:"required"`
			Properties struct {
				Event struct {
					Enum []string `json:"enum"`
				} `json:"event"`
				Data struct {
					Type             string `json:"type"`
					ContentMediaType string `json:"contentMediaType"`
					ContentSchema    struct {
						OneOf []eventObject `json:"oneOf"`
					} `json:"contentSchema"`
				} `json:"data"`
			} `json:"properties"`
		} `json:"schema"`
	}
	require.NoError(t, json.Unmarshal(raw, &media))
	require.Equal(t, []string{"data"}, media.Schema.Required, "every event carries a data line")
	require.Equal(t, []string{contentStreamErrorEvent}, media.Schema.Properties.Event.Enum,
		"only the event that ends the stream names itself")
	data := media.Schema.Properties.Data
	require.Equal(t, "string", data.Type, "a data line is text")
	require.Equal(t, "application/json", data.ContentMediaType, "the data line holds JSON")
	require.Len(t, data.ContentSchema.OneOf, 2, "a data line holds a change or the reason the stream ended")
	require.Equal(t, []string{"error"}, data.ContentSchema.OneOf[1].Required, "the ending event says why")
	change := data.ContentSchema.OneOf[0]
	return streamEventShape{keys: change.Required, events: change.Properties.Event.Enum}
}

// The change stream is Server-Sent Events. Its entry declares that media type
// and no other, names every event the handler subscribes to, and takes the
// collection as its only parameter, because the handler reads no query.
func TestOpenAPI_ContentStreamIsServerSentEvents(t *testing.T) {
	doc := documentedPathsOf(t, buildOpenAPIDoc([]string{"notes"}, nil, openAPIOptions{}))
	op := doc.Paths[contentStreamPath]["get"]
	require.Equal(t, []string{"text/event-stream"}, slices.Sorted(maps.Keys(op.Responses["200"].Content)))

	shape := declaredStreamEvent(t, doc)
	subscribed := make([]string, len(contentStreamEvents))
	for i, et := range contentStreamEvents {
		subscribed[i] = string(et)
	}
	assert.Equal(t, subscribed, shape.events)
	assert.ElementsMatch(t, []string{"event", "schema", "data"}, shape.keys)

	var params []string
	for _, p := range op.Parameters {
		params = append(params, p.In+" "+p.Name)
	}
	assert.Equal(t, []string{"path schema"}, params)
}

// What the stream entry declares is what the handler writes: the media type,
// the comment it opens with, one event for each kind of write, each holding
// the documented keys, and the heartbeat comment.
func TestOpenAPI_ContentStreamMatchesTheWire(t *testing.T) {
	t.Parallel()
	doc := documentedPathsOf(t, buildOpenAPIDoc([]string{"notes"}, nil, openAPIOptions{}))
	declared := doc.Paths[contentStreamPath]["get"].Responses["200"].Content
	shape := declaredStreamEvent(t, doc)

	h := &ContentHandler{
		hooks:              hooks.NewRegistry(),
		perms:              fakeProvider{fakeChecker{allow: true}},
		streamHeartbeat:    50 * time.Millisecond,
		streamDeadlineWarn: new(sync.Once),
	}
	srv := httptest.NewServer(asStreamReader("t1", contentStreamRoute(h)))
	t.Cleanup(srv.Close)

	resp, lines := openChangeStreamResponse(t, srv.URL+"/api/v1/content/notes/stream")
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Contains(t, declared, mediaType, "the handler serves a media type the entry does not declare")
	readStreamLine(t, lines, fmt.Sprintf(contentStreamConnectedComment, "notes"))

	ctx := tenant.WithID(context.Background(), "t1")
	const id = "00000000-0000-0000-0000-0000000000aa"
	note := map[string]any{"body": "on the wire"}
	for _, action := range []core.EventType{core.AfterCreate, core.AfterUpdate, core.AfterDelete} {
		require.NoError(t, core.PublishContentEvent(ctx, h.hooks, core.SourceEngine, action, "notes", id, note, note))
	}

	var sent []string
	timeout := time.After(5 * time.Second)
	for len(sent) < len(shape.events) {
		select {
		case line, ok := <-lines:
			require.True(t, ok, "the stream ended after %v", sent)
			payload, isData := strings.CutPrefix(line, "data: ")
			if !isData {
				continue
			}
			var ev map[string]any
			require.NoError(t, json.Unmarshal([]byte(payload), &ev), payload)
			assert.ElementsMatch(t, shape.keys, slices.Collect(maps.Keys(ev)), payload)
			name, _ := ev["event"].(string)
			sent = append(sent, name)
		case <-timeout:
			t.Fatalf("the stream sent %v and then nothing", sent)
		}
	}
	assert.Equal(t, shape.events, sent, "one event per write, named as the entry names them")
	readStreamLine(t, lines, contentStreamHeartbeatComment)
}

// A media type belongs in a response's content, where a client reads it. One
// written as the response's description is text no generator reads, which is
// how the change stream came to be documented as NDJSON while it served
// events.
func TestOpenAPI_NoResponseDescriptionIsAMediaType(t *testing.T) {
	mediaTypeShaped := regexp.MustCompile(`^[a-z]+/[a-z0-9.+-]+$`)
	doc := documentedPathsOf(t, buildOpenAPIDoc([]string{"notes"}, nil,
		openAPIOptions{AdminTokens: true, TenantFeatures: true, LicensePresentation: true}))
	for path, item := range doc.Paths {
		for method, op := range item {
			for status, resp := range op.Responses {
				if mediaTypeShaped.MatchString(strings.ToLower(strings.TrimSpace(resp.Description))) {
					t.Errorf("%s %s answers %s described as %q: a media type goes in the response's content",
						strings.ToUpper(method), path, status, resp.Description)
				}
			}
		}
	}
}

// schemaHost is the test host with a schema engine in it, as an install has
// once a plugin registers one. The schema export reads its engine from the
// host.
type schemaHost struct {
	*testhost.Host
	engine core.SchemaEngine
}

func (h *schemaHost) Schema() core.SchemaEngine { return h.engine }

// Every GET operation of the served document is requested from the router that
// serves it, as a super admin, and each answer's media type must be one the
// operation declares for that status. An operation that declares none is held
// to application/json, which every engine handler answers with, so a route
// that starts serving anything else fails here until its entry says so. A GET
// is also asked once for each media type it declares, through Accept, and once
// for each value of a query parameter the document enumerates, and every
// declared media type has to be served at least once, so no declaration
// passes without a request having read it.
//
// Only reads are requested. A write needs a body of its own and changes what
// later requests see, and every engine write answers JSON, so an operation
// other than GET may declare no other media type.
func TestOpenAPI_MediaTypesMatchWhatTheRoutesServe(t *testing.T) {
	forEachProbeDialect(t, probeDocumentedMediaTypes)
}

// forEachProbeDialect runs probe against a database of each dialect the run
// selects.
func forEachProbeDialect(t *testing.T, probe func(*testing.T, db.DB)) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a database")
	}
	for _, d := range []struct {
		name string
		open func(*testing.T) db.DB
	}{
		{"postgres", testdb.Postgres},
		{"mysql", testdb.MySQL},
		{"mssql", testdb.MSSQL},
	} {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			probe(t, d.open(t))
		})
	}
}

// documentProbe is an install whose documented routes a test requests: both
// routers over one database, a schema engine holding a collection with one
// record, and the document the admin router serves to a super admin.
type documentProbe struct {
	cfg     *config.Config
	users   *db.UserStore
	servers map[string]*httptest.Server
	served  map[string]map[string]bool
	values  map[string]string
	doc     documentedPaths
	// token is a super admin's session.
	token string
}

// newDocumentProbe builds the install. opts reach both routers.
func newDocumentProbe(t *testing.T, pool db.DB, opts ...RouterOption) *documentProbe {
	t.Helper()
	ctx := context.Background()
	eng := schemaengine.New(testhost.New(pool))
	require.NoError(t, eng.Apply(ctx, "notes", json.RawMessage(
		`{"name":"notes","display_name":"Notes","fields":[{"name":"title","field_type":"text"}]}`)))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+domain.TableName("notes")) })

	cfg := &config.Config{
		DatabaseDriver:   pool.Engine(),
		JWTSecret:        testSecret,
		JWTSecrets:       []string{testSecret},
		JWTExpirySecs:    3600,
		MaxBodyBytes:     10 << 20,
		MaxJSONBodyBytes: 1 << 20,
		CacheTTL:         60 * time.Second,
	}
	adminRouter, err := NewAdminRouter(pool, cfg, append([]RouterOption{WithLifetime(testLifetime(t)),
		WithSchemaSource(eng.SchemaSource()), WithScalingHost(&schemaHost{Host: testhost.New(pool), engine: eng})}, opts...)...)
	require.NoError(t, err)
	apiRouter, err := NewAPIRouter(pool, cfg, nil, append([]RouterOption{WithLifetime(testLifetime(t)),
		WithSchemaSource(eng.SchemaSource())}, opts...)...)
	require.NoError(t, err)
	p := &documentProbe{
		cfg:     cfg,
		users:   db.NewUserStore(pool),
		servers: map[string]*httptest.Server{"admin": httptest.NewServer(adminRouter), "api": httptest.NewServer(apiRouter)},
		served:  map[string]map[string]bool{"admin": collectRoutes(t, adminRouter), "api": collectRoutes(t, apiRouter)},
	}
	for _, s := range p.servers {
		t.Cleanup(s.Close)
	}
	p.token = p.session(t, "super_admin")

	status, _, body := sendProbe(t, http.MethodPost, p.servers["api"].URL+"/api/v1/content/notes", p.token, "", `{"data":{"title":"probe"}}`)
	require.Equal(t, http.StatusCreated, status, string(body))
	var record struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &record))

	status, _, body = sendProbe(t, http.MethodGet, p.servers["admin"].URL+"/api/admin/openapi.json", p.token, "", "")
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &p.doc))

	p.values = map[string]string{
		"schema": "notes", "name": "notes", "id": record.ID, "field": "title",
		"rev_id": uuid.NewString(), "user_code": "ABCD-EFGH", "tenant": "acme",
	}
	return p
}

// session signs a session for a new account holding roles. Each call is a new
// account, so a request that ends its caller's sessions ends no other.
func (p *documentProbe) session(t *testing.T, roles ...string) string {
	t.Helper()
	email := "probe-" + uuid.NewString() + "@example.com"
	user, err := p.users.Create(context.Background(), email, "$2a$10$placeholder-hash-for-test", roles, "")
	require.NoError(t, err)
	token, err := auth.Sign(p.cfg.JWTSecret, p.cfg.JWTExpirySecs, user.ID, user.Email, user.Roles, "", user.TokenVersion)
	require.NoError(t, err)
	return token
}

// serverFor names the router that serves the operation, or "" when neither
// does.
func (p *documentProbe) serverFor(method, path string) string {
	for _, name := range []string{"admin", "api"} {
		if p.served[name][strings.ToUpper(method)+" "+path] {
			return name
		}
	}
	return ""
}

// mediaPluginRoutes are a plugin's routes that name what they answer through
// RouteDoc, so the probe holds a declaration to the handler behind it as it
// holds an engine route: a stream, an export answering CSV or NDJSON as the
// Accept header asks, a stored image declared by its range, and JSON with a
// schema.
func mediaPluginRoutes() []plugin.PluginRoutes {
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": open\n\n")
	})
	export := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/x-ndjson" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"id":1}`+"\n")
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		_, _ = io.WriteString(w, "id\n1\n")
	})
	picture := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	})
	report := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{"total": 1})
	})
	types := func(mediaTypes ...core.RouteMediaType) *core.RouteDoc {
		return &core.RouteDoc{ResponseMediaTypes: mediaTypes}
	}
	return []plugin.PluginRoutes{{Name: "doc-media", Routes: []core.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/doc-media/events", Group: core.GroupAdmin, Handler: stream,
			Doc: types(core.RouteMediaType{MediaType: "text/event-stream"})},
		{Method: http.MethodGet, Pattern: "/api/admin/doc-media/export", Group: core.GroupAdmin, Handler: export,
			Doc: types(core.RouteMediaType{MediaType: "text/csv"}, core.RouteMediaType{MediaType: "application/x-ndjson"})},
		{Method: http.MethodGet, Pattern: "/api/v1/doc-media/picture", Group: core.GroupAuth, Handler: picture,
			Doc: types(core.RouteMediaType{MediaType: "image/*"})},
		{Method: http.MethodGet, Pattern: "/api/v1/doc-media/report", Group: core.GroupAuth, Handler: report,
			Doc: types(core.RouteMediaType{MediaType: "application/json", Schema: map[string]any{"type": "object"}})},
	}}}
}

func probeDocumentedMediaTypes(t *testing.T, pool db.DB) {
	install := newDocumentProbe(t, pool, WithPluginRoutes(mediaPluginRoutes()))
	doc, servers, token := install.doc, install.servers, install.token
	probed := 0
	for _, path := range slices.Sorted(maps.Keys(doc.Paths)) {
		for _, method := range slices.Sorted(maps.Keys(doc.Paths[path])) {
			op := doc.Paths[path][method]
			declared := documentedResponseTypes(t, op)
			if method != "get" {
				for status, types := range declared {
					for mediaType := range types {
						if mediaType != "application/json" {
							t.Errorf("%s %s declares %s for %s: only a GET is requested here, so no other method may declare a media type but application/json",
								strings.ToUpper(method), path, mediaType, status)
						}
					}
				}
				continue
			}
			server := install.serverFor(method, path)
			if server == "" {
				t.Errorf("the document lists GET %s and neither router serves it", path)
				continue
			}
			probed++
			target := servers[server].URL + fillPathParams(path, install.values)
			answered := map[string]bool{}
			for _, p := range probesFor(op) {
				status, contentType, _ := sendProbe(t, http.MethodGet, target+p.query, token, p.accept, "")
				if status == http.StatusNoContent || status == http.StatusNotModified {
					continue
				}
				got := ""
				if contentType != "" {
					mediaType, _, err := mime.ParseMediaType(contentType)
					require.NoError(t, err, "GET %s%s answered Content-Type %q", path, p, contentType)
					got = mediaType
				}
				key := fmt.Sprint(status)
				want := declared[key]
				if len(want) == 0 {
					want = map[string]bool{"application/json": true}
				}
				covered, ok := coveringMediaType(want, got)
				if !ok {
					t.Errorf("GET %s%s answered %d as %q, and the document declares %v for it",
						path, p, status, contentType, slices.Sorted(maps.Keys(want)))
				}
				answered[key+" "+covered] = true
			}
			for status, types := range declared {
				for mediaType := range types {
					if !answered[status+" "+mediaType] {
						t.Errorf("GET %s declares %s for %s, and no request was answered with it", path, mediaType, status)
					}
				}
			}
		}
	}
	require.Positive(t, probed, "no operation was requested")
	t.Logf("requested %d GET operations of the served document", probed)
}

// coveringMediaType finds the declared media type an answered one falls under:
// the type itself, or the range type/* or */* that holds it.
func coveringMediaType(declared map[string]bool, got string) (string, bool) {
	if declared[got] {
		return got, true
	}
	if typ, _, ok := strings.Cut(got, "/"); ok && declared[typ+"/*"] {
		return typ + "/*", true
	}
	if declared["*/*"] {
		return "*/*", true
	}
	return "", false
}

// documentedResponseTypes lists, by status, the media types an operation
// declares, without their parameters.
func documentedResponseTypes(t *testing.T, op documentedOperation) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for status, resp := range op.Responses {
		for key := range resp.Content {
			mediaType, _, err := mime.ParseMediaType(key)
			require.NoError(t, err, "declared media type %q", key)
			if out[status] == nil {
				out[status] = map[string]bool{}
			}
			out[status][mediaType] = true
		}
	}
	return out
}

// mediaProbe is one request made of a GET operation: a query string, an
// Accept header, both or neither.
type mediaProbe struct {
	query, accept string
}

func (p mediaProbe) String() string {
	if p.accept == "" {
		return p.query
	}
	return p.query + " (Accept: " + p.accept + ")"
}

// probesFor lists the requests a GET operation is asked: one plain, one for
// each media type it declares, named in Accept as the document writes it, and
// one for each value of a query parameter the document enumerates, because
// such a parameter is how a route picks a representation.
func probesFor(op documentedOperation) []mediaProbe {
	probes := []mediaProbe{{}}
	accepts := map[string]bool{}
	for _, resp := range op.Responses {
		for key := range resp.Content {
			accepts[key] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(accepts)) {
		probes = append(probes, mediaProbe{accept: key})
	}
	for _, p := range op.Parameters {
		if p.In != "query" {
			continue
		}
		enum, _ := p.Schema["enum"].([]any)
		for _, v := range enum {
			probes = append(probes, mediaProbe{query: "?" + url.Values{p.Name: {fmt.Sprint(v)}}.Encode()})
		}
	}
	return probes
}

// fillPathParams puts a value in each {name} segment of a documented path.
func fillPathParams(path string, values map[string]string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			v, ok := values[seg[1:len(seg)-1]]
			if !ok {
				v = "x"
			}
			segments[i] = url.PathEscape(v)
		}
	}
	return strings.Join(segments, "/")
}

// sendProbe makes one request, with token as its bearer credential unless it
// is empty and a body as JSON, and returns the status, the Content-Type and
// the body. The body of an event stream is never read: the request ends once
// the headers have arrived.
func sendProbe(t *testing.T, method, target, token, accept, body string) (int, string, []byte) {
	t.Helper()
	contentType := ""
	if body != "" {
		contentType = "application/json"
	}
	return sendProbeRequest(t, method, target, token, accept, contentType, body)
}

// sendTypedProbe is sendProbe for a body of another media type.
func sendTypedProbe(t *testing.T, method, target, token, contentType, body string) (int, string, []byte) {
	t.Helper()
	return sendProbeRequest(t, method, target, token, "", contentType, body)
}

func sendProbeRequest(t *testing.T, method, target, token, accept, contentType, body string) (int, string, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "%s %s", method, target)
	defer func() { _ = resp.Body.Close() }()
	answered := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(answered)
	if mediaType == "text/event-stream" {
		return resp.StatusCode, answered, nil
	}
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "%s %s", method, target)
	return resp.StatusCode, answered, out
}
