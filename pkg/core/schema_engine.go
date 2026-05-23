package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SchemaEngine provides DDL lifecycle operations for content schemas.
// Plugins use this to manage the dynamic schema system.
type SchemaEngine interface {
	// Apply stores the schema definition and applies the corresponding DDL.
	// The definition is a JSON-encoded schema object. If a schema with the
	// given name already exists, it computes a diff and applies ALTER DDL.
	Apply(ctx context.Context, name string, definition json.RawMessage) error

	// Delete removes a schema definition and drops its content table.
	Delete(ctx context.Context, name string) error

	// PreviewDDL returns the DDL statements that would be executed for the
	// given schema definition without actually applying them.
	PreviewDDL(ctx context.Context, name string, definition json.RawMessage) ([]DDLStatement, error)

	// ApplyPending runs every statement the engine recorded and has not yet
	// applied.
	// Returns the number of statements applied.
	ApplyPending(ctx context.Context) (int, error)

	// List returns all registered schema definitions as JSON.
	List(ctx context.Context) ([]json.RawMessage, error)

	// Get returns a single schema definition by name.
	Get(ctx context.Context, name string) (json.RawMessage, error)

	// ValidateContent validates content data against a schema's validation rules.
	// Returns nil when all rules pass, or a slice of validation errors keyed by
	// i18n error codes for the caller to localize.
	ValidateContent(ctx context.Context, schemaName string, data map[string]any) ([]SchemaValidationError, error)
}

// SchemaValidationError is a single schema validation failure returned by
// SchemaEngine.ValidateContent.
type SchemaValidationError struct {
	Field   string         `json:"field"`
	Code    string         `json:"code"`
	Rule    string         `json:"rule"`
	Value   any            `json:"value,omitempty"`
	Message string         `json:"message"` // pre-localized message from the bridge
	Params  map[string]any `json:"-"`
}

// DDLStatement represents a single generated DDL step.
type DDLStatement struct {
	Description string `json:"description"`
	SQL         string `json:"sql"`
	DownSQL     string `json:"down_sql,omitempty"`
}

// SchemaSource reads content schema definitions. It is the engine's own way in
// to the schema registry, so the kernel's content path depends on this
// interface rather than on whatever stores the definitions.
//
// SchemaEngine is the other direction: a plugin calling the engine to apply
// DDL. This one is the engine calling out to whatever holds the definitions.
type SchemaSource interface {
	// GetByName returns one schema definition for the request's tenant.
	GetByName(ctx context.Context, name string) (*Schema, error)

	// List returns every schema definition for the request's tenant.
	List(ctx context.Context) ([]*Schema, error)
}

// SchemaEngineRegistrar is implemented by the engine host. The plugin that
// owns schema definitions calls it when it starts with its engine, and again
// with nil when it stops, so the registration follows the plugin's lifecycle
// rather than the process.
//
// This is the seam that makes the schema engine suppliable. Any package that
// can import this one can implement SchemaEngine and register it, so an
// install is not tied to one implementation.
type SchemaEngineRegistrar interface {
	RegisterSchemaEngine(e SchemaEngine)
}

// ErrNoSchemaEngine is what every schema read answers when no schema engine is
// installed. It is deliberately not a not-found: a caller has to be able to
// tell "this install cannot describe content types" from "that content type
// does not exist", because the two call for opposite actions.
var ErrNoSchemaEngine = errors.New("no schema engine is installed")

// AbsentSchemaSource is the registry an install has when no schema engine is
// installed. Every read fails with ErrNoSchemaEngine.
//
// It exists so the absence is loud. The obvious alternative, a source that
// reports no schemas, makes the content API answer an empty list for every
// name, and an empty list is what a caller sees after its data is deleted.
// Reporting emptiness for a missing engine would publish that reading on
// every route at once.
type AbsentSchemaSource struct{}

func (AbsentSchemaSource) GetByName(context.Context, string) (*Schema, error) {
	return nil, ErrNoSchemaEngine
}

func (AbsentSchemaSource) List(context.Context) ([]*Schema, error) {
	return nil, ErrNoSchemaEngine
}

// ErrSchemaNotFound is what a SchemaSource answers for a name it does not
// hold. It is public because the engine is a plugin: without a sentinel the
// kernel can read, a supplied engine reporting an unknown content type is
// indistinguishable from one reporting an outage, and the content API answers
// 503 where it should answer 404.
var ErrSchemaNotFound = errors.New("schema not found")

// SchemaSourceProvider is implemented by the engine host and forwarded by
// ScopedHost, and optionally by a SchemaEngine itself.
//
// On the host it answers the registry the content path reads, which is the
// plugin's once one has registered an engine and AbsentSchemaSource until
// then. The content path reads it per request and never at Start, because a
// plugin registers after the reader was built.
//
// On a SchemaEngine it is how an engine offers its definitions directly. Get
// and List answer JSON, so without this the content path decodes a document on
// every read. An engine holding definitions in memory implements this and
// hands back the values it already has. The two methods cannot live on one
// interface because SchemaEngine.List and SchemaSource.List share a name and
// differ in what they return.
type SchemaSourceProvider interface {
	SchemaSource() SchemaSource
}

// LazySchemaSource defers resolution to each call, so a reader built before a
// plugin starts still reads whatever that plugin registered.
//
// The content store is built while the host is, which is several boot steps
// before the first plugin Start. Given a source directly it would hold the
// absent registry of that moment for the life of the process, and a
// registered engine would serve the authoring routes while the content routes
// answered that no engine exists. A resolve that answers nil is treated as no
// engine at all.
func LazySchemaSource(resolve func() SchemaSource) SchemaSource {
	return lazySchemaSource{resolve: resolve}
}

type lazySchemaSource struct {
	resolve func() SchemaSource
}

func (l lazySchemaSource) now() SchemaSource {
	if l.resolve == nil {
		return AbsentSchemaSource{}
	}
	if s := l.resolve(); s != nil {
		return s
	}
	return AbsentSchemaSource{}
}

func (l lazySchemaSource) GetByName(ctx context.Context, name string) (*Schema, error) {
	return l.now().GetByName(ctx, name)
}

func (l lazySchemaSource) List(ctx context.Context) ([]*Schema, error) {
	return l.now().List(ctx)
}

// NoSchemaEngine reports whether src resolves to no engine at all.
//
// Absence is expressed by the source itself rather than by a flag beside it,
// and LazySchemaSource defers that answer, so a caller cannot learn it with a
// type assertion. Asking here resolves first, which is also what makes the
// answer follow a plugin that registers or stops after the caller was built.
func NoSchemaEngine(src SchemaSource) bool {
	switch s := src.(type) {
	case nil:
		return true
	case lazySchemaSource:
		return NoSchemaEngine(s.now())
	case AbsentSchemaSource:
		return true
	default:
		return false
	}
}

// SchemaSourceOf reads definitions from a SchemaEngine by decoding what Get
// and List answer. It is the fallback for an engine that does not provide a
// SchemaSource of its own, so every engine can feed the content path whether
// or not its author thought about that path.
//
// A nil engine reads as no engine, so every call fails with ErrNoSchemaEngine
// rather than panicking on a registration that has not happened yet.
func SchemaSourceOf(e SchemaEngine) SchemaSource {
	if e == nil {
		return AbsentSchemaSource{}
	}
	if p, ok := e.(SchemaSourceProvider); ok {
		if s := p.SchemaSource(); s != nil {
			return s
		}
	}
	return decodingSchemaSource{engine: e}
}

type decodingSchemaSource struct {
	engine SchemaEngine
}

func (d decodingSchemaSource) GetByName(ctx context.Context, name string) (*Schema, error) {
	raw, err := d.engine.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	var sc Schema
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, fmt.Errorf("decode schema %q: %w", name, err)
	}
	return &sc, nil
}

func (d decodingSchemaSource) List(ctx context.Context) ([]*Schema, error) {
	raws, err := d.engine.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Schema, 0, len(raws))
	for i, raw := range raws {
		var sc Schema
		if err := json.Unmarshal(raw, &sc); err != nil {
			return nil, fmt.Errorf("decode schema %d: %w", i, err)
		}
		out = append(out, &sc)
	}
	return out, nil
}
