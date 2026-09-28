// Package schemaengine is a schema engine built the way a third party would
// have to build one: it imports pkg/core and nothing else of this module.
//
// It exists to prove the kernel has a real extension point. If this
// package ever needs an import from internal/, the extension point is not one,
// because no package outside this module could write the same code.
package schemaengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Engine keeps schema definitions in memory and generates the table for each
// one through the host's migration database. It implements core.SchemaEngine
// and core.SchemaSource.
type Engine struct {
	host core.Host

	mu      sync.RWMutex
	schemas map[string]*core.Schema
}

// New returns an Engine that writes its tables through the given host.
func New(host core.Host) *Engine {
	return &Engine{host: host, schemas: map[string]*core.Schema{}}
}

func (e *Engine) Apply(ctx context.Context, name string, definition json.RawMessage) error {
	var sc core.Schema
	if err := json.Unmarshal(definition, &sc); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	sc.Name = name

	if e.host != nil {
		if err := e.createTable(ctx, &sc); err != nil {
			return err
		}
	}

	e.mu.Lock()
	e.schemas[name] = &sc
	e.mu.Unlock()
	return nil
}

// createTable emits the one CREATE TABLE this engine knows how to write. It
// is deliberately the simplest thing that serves content: an id, the declared
// columns, and the tenant column every generated table carries.
func (e *Engine) createTable(ctx context.Context, sc *core.Schema) error {
	// A generated table is one table shared by every tenant, keyed by a
	// tenant_id column. Creating it through the request querier puts it
	// wherever that connection is bound, so a tenant-scoped caller would build
	// a second copy inside its own schema and the pivot's foreign key would
	// then point at rows in a different one. core.EngineDBConn exists for this:
	// a connection bound to the engine's own database whatever the caller.
	if p, ok := e.host.(core.EngineDBConnProvider); ok {
		conn, err := p.EngineDBConn(ctx)
		if err == nil {
			defer conn.Close()
			return e.createTableOn(ctx, engineConnExec{conn}, sc)
		}
	}
	q := e.host.Querier(ctx)
	if q == nil {
		return nil
	}
	return e.createTableOn(ctx, querierExec{q}, sc)
}

// execer is the one thing table creation needs, so the engine connection and
// the request querier can both satisfy it. Each reports a different result
// type, and none of this reads one.
type execer interface {
	exec(ctx context.Context, query string) error
}

type engineConnExec struct{ conn *sql.Conn }

func (e engineConnExec) exec(ctx context.Context, query string) error {
	_, err := e.conn.ExecContext(ctx, query)
	return err
}

type querierExec struct{ q core.Querier }

func (e querierExec) exec(ctx context.Context, query string) error {
	_, err := e.q.Exec(ctx, query)
	return err
}

func (e *Engine) createTableOn(ctx context.Context, q execer, sc *core.Schema) error {
	uuidType, tsType := "UUID", "TIMESTAMPTZ"
	uuidDefault, nowFunc := "gen_random_uuid()", "NOW()"
	switch e.host.Dialect() {
	case "mysql":
		// Microseconds, as the schema engine plugin creates them. A bare
		// DATETIME keeps whole seconds, so a write that lands in the same
		// second as the one before it reads back an unchanged updated_at.
		uuidType, tsType = "CHAR(36)", "DATETIME(6)"
		uuidDefault, nowFunc = "(UUID())", "CURRENT_TIMESTAMP(6)"
	case "mssql":
		uuidType, tsType = "UNIQUEIDENTIFIER", "DATETIMEOFFSET"
		uuidDefault, nowFunc = "NEWID()", "SYSDATETIMEOFFSET()"
	}

	// A generated table carries the engine's own columns beside the declared
	// fields. The content path reads them by name, so an engine that omits
	// them defines a schema the content API cannot serve.
	cols := []string{
		"id " + uuidType + " NOT NULL DEFAULT " + uuidDefault + " PRIMARY KEY",
		"created_at " + tsType + " DEFAULT " + nowFunc,
		"updated_at " + tsType + " DEFAULT " + nowFunc,
		"tenant_id VARCHAR(64)",
	}
	// A text column that carries a literal DEFAULT or takes part in a UNIQUE
	// constraint cannot be unbounded on MySQL or SQL Server, so those get a
	// bounded type. Postgres indexes TEXT directly and does not care.
	boundedText := "VARCHAR(255)"
	if e.host.Dialect() == "mssql" {
		boundedText = "NVARCHAR(255)"
	}

	// An unbounded text column is a different type on each engine, and on SQL
	// Server the wrong one is not a smaller column, it is one that cannot be
	// compared. TEXT there is the deprecated LOB type, and a filter on it
	// fails with "the data types text and nvarchar are incompatible in the
	// equal to operator", which reads as a defect in the list query.
	unboundedText := "TEXT"
	switch e.host.Dialect() {
	case "mysql":
		unboundedText = "LONGTEXT"
	case "mssql":
		unboundedText = "NVARCHAR(MAX)"
	}

	// Draft and publish are columns on the generated table, so an engine that
	// does not create them defines a schema whose entries the content API
	// cannot publish. The read path defaults to published when _status is
	// absent, which means omitting the column silently changes what a list
	// returns rather than failing.
	if sc.WithSoftDelete {
		// The read path selects deleted_at, so a table without it fails the
		// scan rather than reading every row as live.
		cols = append(cols, "deleted_at "+tsType)
	}
	if sc.WithDraftPublish {
		// published, not draft. A row written without a status is visible,
		// and the read path defaults to published, so defaulting the column
		// the other way makes every insert invisible to the very next list.
		cols = append(cols,
			"_status "+boundedText+" NOT NULL DEFAULT 'published'",
			"published_at "+tsType,
		)
	}

	var pivots []string
	var uniques []string
	var constraints []string
	for _, f := range sc.Fields {
		if f.System {
			continue
		}
		if f.FieldType == "relation" {
			// One side of the relation is a column on this table and the other
			// is a table of its own. Which one depends on the cardinality, and
			// the content path reads whichever the definition declares, so an
			// engine that creates neither leaves every relation read failing
			// against a column that does not exist.
			switch f.RelationType {
			case core.RelBelongsTo, core.RelHasOne:
				// The column alone is not the relation. What happens to a row
				// whose target is deleted is declared by the key, and the
				// content store relies on the database to apply it, so a
				// column without a key leaves a required relation pointing at
				// a row that is gone.
				//
				// Required cascades and optional nulls, which is what the
				// engine emits. SQL Server refuses a second cascading path
				// into one table, so there the key declares no action and the
				// content store applies it instead.
				onDelete := "SET NULL"
				notNull := ""
				if f.Required {
					onDelete = "CASCADE"
					notNull = " NOT NULL"
				}
				if e.host.Dialect() == "mssql" {
					onDelete = "NO ACTION"
				}
				// The key is declared as a table constraint, not on the
				// column. MySQL parses a column-level REFERENCES and discards
				// it, so a column-level key would not exist there. One
				// table-level clause is accepted by all three and leaves the
				// constraint where the catalog can be read for it.
				fkCol := e.quote(core.FKColumn(f.Name, f.RelationFKName))
				cols = append(cols, fmt.Sprintf("%s %s%s", fkCol, uuidType, notNull))
				constraints = append(constraints, fmt.Sprintf(
					"FOREIGN KEY (%s) REFERENCES %s(id) ON DELETE %s",
					fkCol, e.quote(core.TableName(f.RelationTo)), onDelete))
			case core.RelManyToMany:
				pivot := f.RelationThrough
				if pivot == "" {
					pivot = core.PivotTableName(sc.Name, f.RelationTo)
				}
				a := strings.TrimPrefix(core.TableName(sc.Name), "_") + "_id"
				b := strings.TrimPrefix(core.TableName(f.RelationTo), "_") + "_id"
				if a == b {
					b = "related_" + b
				}
				// The write path upserts a pivot row with ON CONFLICT on the
				// pair, so the pair has to carry a unique constraint. Without
				// it the insert fails with "no unique or exclusion constraint
				// matching the ON CONFLICT specification", which reads as a
				// query defect rather than a missing table definition.
				// Both sides cascade: a pivot row records a pair, and a pair
				// whose either half is deleted records nothing.
				pivotAction := "CASCADE"
				if e.host.Dialect() == "mssql" {
					pivotAction = "NO ACTION"
				}
				// Table-level keys here too, for the reason given on the
				// belongs_to above.
				pivots = append(pivots, e.createTableSQL(pivot, fmt.Sprintf(
					"%s %s NOT NULL, %s %s NOT NULL, tenant_id VARCHAR(64), PRIMARY KEY (%s, %s), "+
						"FOREIGN KEY (%s) REFERENCES %s(id) ON DELETE %s, "+
						"FOREIGN KEY (%s) REFERENCES %s(id) ON DELETE %s",
					e.quote(a), uuidType, e.quote(b), uuidType,
					e.quote(a), e.quote(b),
					e.quote(a), e.quote(core.TableName(sc.Name)), pivotAction,
					e.quote(b), e.quote(core.TableName(f.RelationTo)), pivotAction)))
			}
			continue
		}
		colType := e.columnType(f.FieldType, boundedText, unboundedText, tsType)
		if f.Unique && colType == unboundedText {
			colType = boundedText
		}
		cols = append(cols, e.quote(f.Name)+" "+colType)
		if f.Unique {
			// Scoped by tenant, the way the engine does it: the same value in
			// two tenants is two rows, not a conflict.
			uniques = append(uniques, fmt.Sprintf(
				"CREATE UNIQUE INDEX %s ON %s (tenant_id, %s)",
				e.quote("ux_"+core.TableName(sc.Name)+"_"+f.Name),
				e.quote(core.TableName(sc.Name)), e.quote(f.Name)))
		}
	}
	stmt := e.createTableSQL(core.TableName(sc.Name), strings.Join(append(cols, constraints...), ", "))
	if err := q.exec(ctx, stmt); err != nil {
		return fmt.Errorf("create table for %s: %w", sc.Name, err)
	}
	for _, stmt := range pivots {
		if err := q.exec(ctx, stmt); err != nil {
			return fmt.Errorf("create pivot for %s: %w", sc.Name, err)
		}
	}
	for _, stmt := range uniques {
		// A second Apply of the same definition re-runs this, and MySQL has no
		// CREATE INDEX IF NOT EXISTS, so an index that is already there is not
		// an error here. The engine reads the driver's code for this. A
		// fixture that may import only pkg/core reads the message, which is
		// weaker and enough: the only statement here is one CREATE INDEX.
		if err := q.exec(ctx, stmt); err != nil && !alreadyExists(err) {
			return fmt.Errorf("create unique index for %s: %w", sc.Name, err)
		}
	}
	return nil
}

// alreadyExists reports whether err says the object is already there. Every
// dialect says so in its own words and this reads all three, because a
// third-party engine has pkg/core and the standard library and nothing else.
func alreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"already exists", "duplicate key name", "duplicate index"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// createTableSQL renders a create that an existing table does not fail.
//
// Two dialects spell that IF NOT EXISTS. SQL Server has no such clause and
// answers the statement with a syntax error, which arrives as a failure in
// whatever a test was setting up rather than as anything about the table, so
// there the create is guarded by a catalog lookup instead.
func (e *Engine) createTableSQL(table, body string) string {
	if e.host.Dialect() == "mssql" {
		return fmt.Sprintf("IF OBJECT_ID(N'%s', N'U') IS NULL CREATE TABLE %s (%s)",
			table, e.quote(table), body)
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", e.quote(table), body)
}

// columnType maps a declared field type to a column type the content path can
// write into. The store passes a Go value of the field's own type, so a column
// that cannot hold it is not a smaller fixture, it is a broken one: a boolean
// written into TEXT fails on insert with "cannot encode true into text format".
func (e *Engine) columnType(fieldType, boundedText, unboundedText, tsType string) string {
	switch fieldType {
	case "boolean":
		switch e.host.Dialect() {
		case "mysql":
			return "TINYINT(1)"
		case "mssql":
			return "BIT"
		default:
			return "BOOLEAN"
		}
	case "number":
		return "DOUBLE PRECISION"
	case "date", "datetime":
		return tsType
	case "json":
		switch e.host.Dialect() {
		case "mysql":
			return "JSON"
		case "mssql":
			return "NVARCHAR(MAX)"
		default:
			return "JSONB"
		}
	case "uid", "email", "url", "media":
		// Short, and each of them is a candidate for an index.
		return boundedText
	default:
		return unboundedText
	}
}

func (e *Engine) quote(name string) string {
	switch e.host.Dialect() {
	case "mysql":
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	case "mssql":
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	default:
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}

func (e *Engine) Delete(_ context.Context, name string) error {
	e.mu.Lock()
	delete(e.schemas, name)
	e.mu.Unlock()
	return nil
}

func (e *Engine) PreviewDDL(_ context.Context, name string, _ json.RawMessage) ([]core.DDLStatement, error) {
	return []core.DDLStatement{{
		Description: "create " + core.TableName(name),
		SQL:         "CREATE TABLE " + core.TableName(name),
	}}, nil
}

func (e *Engine) ApplyPending(context.Context) (int, error) { return 0, nil }

func (e *Engine) List(context.Context) ([]json.RawMessage, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]json.RawMessage, 0, len(e.schemas))
	for _, sc := range e.schemas {
		raw, err := json.Marshal(sc)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func (e *Engine) Get(_ context.Context, name string) (json.RawMessage, error) {
	e.mu.RLock()
	sc, ok := e.schemas[name]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", core.ErrSchemaNotFound, name)
	}
	return json.Marshal(sc)
}

func (e *Engine) ValidateContent(context.Context, string, map[string]any) ([]core.SchemaValidationError, error) {
	return nil, nil
}

// GetByName implements core.SchemaSource, which is what the content path reads.
func (e *Engine) GetByName(_ context.Context, name string) (*core.Schema, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	sc, ok := e.schemas[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", core.ErrSchemaNotFound, name)
	}
	return sc, nil
}

// SchemaSource returns this engine's core.SchemaSource surface, which is what
// core.SchemaSourceProvider asks for. It is a separate value because
// SchemaSource.List and SchemaEngine.List share a name and return different
// types, so one value cannot carry both.
//
// An engine that does not offer this is read through core.SchemaSourceOf,
// which decodes what Get and List answer. Offering it is what saves the
// content path a JSON decode per read, and this fixture offers it because it
// is the shape a supplied engine should copy.
func (e *Engine) SchemaSource() core.SchemaSource { return source{e} }

var _ core.SchemaSourceProvider = (*Engine)(nil)

type source struct{ e *Engine }

func (s source) GetByName(ctx context.Context, name string) (*core.Schema, error) {
	return s.e.GetByName(ctx, name)
}

func (s source) List(context.Context) ([]*core.Schema, error) {
	s.e.mu.RLock()
	defer s.e.mu.RUnlock()
	out := make([]*core.Schema, 0, len(s.e.schemas))
	for _, sc := range s.e.schemas {
		out = append(out, sc)
	}
	return out, nil
}
