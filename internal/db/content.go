package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// ErrContentConflict is returned by Update when the record was modified by
// another request since the caller last read it (optimistic concurrency).
var ErrContentConflict = errors.New("content was modified by another request")

// ContentStore provides CRUD operations for content entries across all
// configured content schemas. It uses the pool for all queries so that
// tenant isolation (SET search_path / USE database) applied at the
// connection level is respected automatically.
type ContentStore struct {
	pool      DB
	schemas   core.SchemaSource
	itemCache cache.Cache[string, *domain.Content]   // nullable; nil = disabled
	listCache cache.Cache[string, []*domain.Content] // nullable; nil = disabled

	onInvalidate func(ctx context.Context, schemaName string) // nullable

	// ensured records the generated tables this process has already verified
	// to carry the tenant_id column, so the conditional ALTER runs once per
	// table instead of once per request.
	ensured sync.Map

	// dbName is the engine's own database, resolved once for the
	// MySQL/MSSQL table qualification. Resolving per query would log the
	// miss for every test double that does not report a database name.
	dbName     string
	dbNameOnce sync.Once

	ListCacheHits   atomic.Uint64
	ListCacheMisses atomic.Uint64
}

// NewContentStore constructs a ContentStore backed by the given connection pool and schema registry.
func NewContentStore(pool DB, schemas core.SchemaSource) *ContentStore {
	return &ContentStore{pool: pool, schemas: schemas}
}

// NewContentStoreWithCache constructs a ContentStore that caches GetByID and List results.
func NewContentStoreWithCache(
	pool DB,
	schemas core.SchemaSource,
	itemCache cache.Cache[string, *domain.Content],
	listCache cache.Cache[string, []*domain.Content],
) *ContentStore {
	return &ContentStore{pool: pool, schemas: schemas, itemCache: itemCache, listCache: listCache}
}

// table returns the physical table name for a schema, validating the schema
// exists. The returned name is engine-qualified so it resolves on a
// tenant-scoped MySQL/MSSQL connection. Postgres resolves through search_path.
// Tables created by engines without tenant isolation get their tenant_id
// column added on first use (once per process).
func (s *ContentStore) table(ctx context.Context, schemaName string) (*domain.Schema, string, error) {
	sc, err := s.schemas.GetByName(ctx, schemaName)
	if err != nil {
		// A supplied engine reports an unknown content type with the public
		// sentinel. Joining the kernel's own keeps every caller's
		// errors.Is(err, domain.ErrNotFound) working, so a name nobody
		// defined is a 404 rather than an outage.
		if errors.Is(err, core.ErrSchemaNotFound) {
			return nil, "", fmt.Errorf("%w: %w", domain.ErrNotFound, err)
		}
		return nil, "", err
	}
	table := domain.TableName(schemaName)
	if err := s.ensureTable(ctx, table); err != nil {
		return nil, "", fmt.Errorf("content: ensure tenant column: %w", err)
	}
	return sc, s.qualify(table), nil
}

// qualify returns the runtime SQL reference for a generated table, naming the
// engine's own database explicitly on MySQL/MSSQL so the shared table still
// resolves on a tenant-scoped connection.
func (s *ContentStore) qualify(table string) string {
	s.dbNameOnce.Do(func() { s.dbName = DatabaseNameOf(s.pool) })
	return qualifyGeneratedTable(s.pool.Engine(), s.dbName, table)
}

// ensureTable adds the tenant_id column to a generated table when this
// process has not already verified it. Tables created by engines without
// tenant isolation are healed on first use rather than failing every query.
func (s *ContentStore) ensureTable(ctx context.Context, table string) error {
	if _, ok := s.ensured.Load(table); ok {
		return nil
	}
	s.dbNameOnce.Do(func() { s.dbName = DatabaseNameOf(s.pool) })
	ref := qualifyGeneratedTable(s.pool.Engine(), s.dbName, table)
	if _, err := s.pool.Exec(ctx, AddTenantColumnIfMissingSQL(s.pool.Engine(), ref, table, s.dbName)); err != nil {
		if isDuplicateColumnError(err, s.pool.Engine()) {
			s.ensured.Store(table, struct{}{})
			return nil
		}
		return fmt.Errorf("ensure tenant column on %s: %w", table, err)
	}
	s.ensured.Store(table, struct{}{})
	return nil
}

// cacheItemKey returns a stable cache key for a single item. Tenant-scoped:
// two tenants reading the same record id must never share a cache entry.
func cacheItemKey(tenantID, schemaName string, id uuid.UUID) string {
	return tenantID + ":" + schemaName + ":" + id.String()
}

// cacheListKey returns a stable cache key for a list query.
func cacheListKey(tenantID, schemaName string, limit, offset int, filters map[string]any) string {
	h := fmt.Sprintf("%s:%s:l=%d:o=%d", tenantID, schemaName, limit, offset)
	if len(filters) > 0 {
		b, _ := json.Marshal(filters)
		h += ":f=" + string(b)
	}
	return h
}

// OnInvalidate registers fn to run after a write invalidates this store's
// caches, with the schema written. The runtime uses it to tell the other
// replicas, whose in-memory caches this write cannot reach. Call it before the
// store serves traffic.
func (s *ContentStore) OnInvalidate(fn func(ctx context.Context, schemaName string)) {
	s.onInvalidate = fn
}

// invalidateSchema flushes all item and list caches for a schema on any write.
func (s *ContentStore) invalidateSchema(ctx context.Context, schemaName string, id uuid.UUID) {
	if s.onInvalidate != nil && (s.itemCache != nil || s.listCache != nil) {
		s.onInvalidate(ctx, schemaName)
	}
	if s.itemCache != nil {
		_ = s.itemCache.Delete(ctx, cacheItemKey(tenant.ID(ctx), schemaName, id)) // err suppressed: cache is best-effort
	}
	if s.listCache != nil {
		// Flush all list keys for this schema prefix: simplest correct strategy.
		// Full flush is safe here because list keys are namespaced per ContentStore instance.
		_ = s.listCache.Flush(ctx) // err suppressed: cache is best-effort
	}
}

// Caching reports whether reads are served through both caches. A store
// built without them reads the database on every request.
func (s *ContentStore) Caching() bool { return s.itemCache != nil && s.listCache != nil }

// FlushCaches drops every cached item and list entry. Callers use it when the
// backing schema changes: the cached rows carry the pre-migration column set,
// which a schema edit alone would leave in place until the TTL lapsed. No-op
// when caching is disabled.
func (s *ContentStore) FlushCaches(ctx context.Context) {
	if s.itemCache != nil {
		_ = s.itemCache.Flush(ctx) // err suppressed: cache is best-effort
	}
	if s.listCache != nil {
		_ = s.listCache.Flush(ctx) // err suppressed: cache is best-effort
	}
}

// Insert creates a new row in the collection table, tagged with the request's
// tenant. Only keys matching defined (non-system) field names are written.
// Unknown keys are ignored.
func (s *ContentStore) Insert(ctx context.Context, schemaName string, data map[string]any) (*domain.Content, error) {
	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, fmt.Errorf("content insert: resolve table: %w", err)
	}

	d := dialect.Must(s.pool.Engine())
	tenantID := tenant.ID(ctx)
	cols, args := buildInsert(sc, data, d)
	cols = append([]string{d.QuoteIdentifier(TenantColumnName)}, cols...)
	args = append([]any{tenantID}, args...)

	// PostgreSQL supports RETURNING. MySQL and MSSQL do not.
	if s.pool.Engine() == "postgres" {
		ph := placeholders(len(args))
		row, qErr := s.pool.QueryRow(ctx,
			fmt.Sprintf(`INSERT INTO %s AS r (%s) VALUES (%s) RETURNING %s`,
				table, strings.Join(cols, ", "), ph, writtenRowColumns(s.pool.Engine(), sc)),
			args...,
		)
		if qErr != nil {
			return nil, fmt.Errorf("content insert: %w", qErr)
		}
		result, err := scanWrittenRow(s.pool.Engine(), row, schemaName, sc)
		if err != nil {
			return nil, err
		}
		s.invalidateSchema(ctx, schemaName, result.ID)
		return result, nil
	}

	// MySQL / MSSQL: generate UUID in Go, INSERT with explicit id, then SELECT.
	newID := uuid.New()
	cols = append([]string{"id"}, cols...)
	args = append([]any{newID}, args...)
	ph := placeholders(len(args))
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(cols, ", "), ph),
		args...,
	); err != nil {
		return nil, fmt.Errorf("content insert: %w", err)
	}
	row, qErr := s.pool.QueryRow(ctx, writtenRowSelect(s.pool.Engine(), sc, table), newID, tenantID)
	if qErr != nil {
		return nil, fmt.Errorf("content insert select: %w", qErr)
	}
	result, err := scanWrittenRow(s.pool.Engine(), row, schemaName, sc)
	if err != nil {
		return nil, fmt.Errorf("content insert select: %w", err)
	}
	s.invalidateSchema(ctx, schemaName, result.ID)
	return result, nil
}

// InsertWithID inserts a content row under an explicit id. A caller that keeps
// its own source of truth mirrors rows into the per-schema table this way, so
// the mirrored row shares its id with the source record and later updates and
// deletes land on the same row.
func (s *ContentStore) InsertWithID(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) (*domain.Content, error) {
	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, fmt.Errorf("content insert: resolve table: %w", err)
	}

	d := dialect.Must(s.pool.Engine())
	tenantID := tenant.ID(ctx)
	cols, args := buildInsert(sc, data, d)
	cols = append([]string{"id", d.QuoteIdentifier(TenantColumnName)}, cols...)
	args = append([]any{id, tenantID}, args...)
	ph := placeholders(len(args))

	if s.pool.Engine() == "postgres" {
		row, qErr := s.pool.QueryRow(ctx,
			fmt.Sprintf(`INSERT INTO %s AS r (%s) VALUES (%s) RETURNING %s`,
				table, strings.Join(cols, ", "), ph, writtenRowColumns(s.pool.Engine(), sc)),
			args...,
		)
		if qErr != nil {
			return nil, fmt.Errorf("content insert: %w", qErr)
		}
		result, err := scanWrittenRow(s.pool.Engine(), row, schemaName, sc)
		if err != nil {
			return nil, err
		}
		s.invalidateSchema(ctx, schemaName, result.ID)
		return result, nil
	}

	// MySQL / MSSQL: no RETURNING. Read back the row just written.
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(cols, ", "), ph),
		args...,
	); err != nil {
		return nil, fmt.Errorf("content insert: %w", err)
	}
	row, qErr := s.pool.QueryRow(ctx, writtenRowSelect(s.pool.Engine(), sc, table), id, tenantID)
	if qErr != nil {
		return nil, fmt.Errorf("content insert select: %w", qErr)
	}
	result, err := scanWrittenRow(s.pool.Engine(), row, schemaName, sc)
	if err != nil {
		return nil, fmt.Errorf("content insert select: %w", err)
	}
	s.invalidateSchema(ctx, schemaName, result.ID)
	return result, nil
}

// UpsertContent writes a content row under an explicit id, updating an
// existing row or inserting a missing one. It is the mirror primitive behind
// core.ContentWriter: a caller with its own source of truth keeps the
// per-schema table, which the public routes read, aligned with it.
func (s *ContentStore) UpsertContent(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any) error {
	if _, err := s.Update(ctx, schemaName, id, data, time.Time{}); err == nil {
		return nil
	} else if !errors.Is(err, ErrContentConflict) && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	_, err := s.InsertWithID(ctx, schemaName, id, data)
	return err
}

// List returns up to limit rows from the calling tenant's collection table,
// ordered by created_at DESC. filters maps column names to exact-match values
// (safe: column names come from schema). User-defined field values are
// returned inside Content.Data.
func (s *ContentStore) List(ctx context.Context, schemaName string, limit, offset int, filters map[string]any) ([]*domain.Content, error) {
	// limit<=0 means "no rows" per the pagination contract. MSSQL rejects
	// FETCH NEXT 0 ROWS ONLY, so short-circuit before building the query.
	if limit <= 0 {
		return []*domain.Content{}, nil
	}

	// Cache read
	tenantID := tenant.ID(ctx)
	if s.listCache != nil {
		key := cacheListKey(tenantID, schemaName, limit, offset, filters)
		if cached, ok := s.listCache.Get(ctx, key); ok {
			s.ListCacheHits.Add(1)
			// Copied: callers decorate Data in place. See content_clone.go.
			return cloneContents(cached), nil
		}
		s.ListCacheMisses.Add(1)
	}

	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, err
	}

	// Build filter-key allowlist: system columns + resolved schema field columns.
	allowed := map[string]bool{
		"id":         true,
		"created_at": true,
		"updated_at": true,
		"_status":    true,
		"deleted_at": true,
	}
	for _, f := range sc.Fields {
		col := fieldColumn(f)
		allowed[col] = true
	}

	// Validate filter keys against the allowlist.
	for key := range filters {
		if !allowed[key] {
			return nil, fmt.Errorf("%w: unknown filter key %q", domain.ErrBadRequest, key)
		}
	}

	d := dialect.Must(s.pool.Engine())
	// $1=tenant, $2=limit, $3=offset; filter clauses start at $4.
	args := []any{tenantID, limit, offset}
	whereClauses := []string{fmt.Sprintf("%s = $1", d.QuoteIdentifier(TenantColumnName))}
	if sc.WithSoftDelete {
		whereClauses = append(whereClauses, "deleted_at IS NULL")
	}
	// Default to published content unless caller explicitly sets _status in filters.
	if sc.WithDraftPublish {
		if _, hasStatus := filters["_status"]; !hasStatus {
			whereClauses = append(whereClauses, "(_status = 'published' OR _status IS NULL)")
		}
	}
	i := 4
	for col, val := range filters {
		// col is allowlist-validated above. Quote so reserved-word field names
		// (e.g. "order") don't break the query.
		whereClauses = append(whereClauses, fmt.Sprintf("%s = $%d", d.QuoteIdentifier(col), i))
		args = append(args, val)

		i++
	}
	where := ""
	if len(whereClauses) > 0 {
		where = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Dialect-aware pagination: PG/MySQL use LIMIT/OFFSET, MSSQL uses OFFSET/FETCH.
	// $2=limit, $3=offset (see args above).
	pagination := dialect.LimitOffsetPlaceholders(d, 2, 3)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, created_at, updated_at,
		       %s
		FROM %s r
		%s
		ORDER BY created_at DESC
		%s`, dataColumnExpr(s.pool.Engine(), sc, "r"), table, where, pagination),
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("content list %s: %w", table, err)
	}
	defer rows.Close()

	items := make([]*domain.Content, 0)
	for rows.Next() {
		c, err := scanFullRow(s.pool.Engine(), rows, schemaName, sc)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Cache write: skip for oversized limits (abusive or direct-caller paths).
	// The handler clamps to ≤200. This guard covers unclamped callers.
	if s.listCache != nil && limit <= 200 {
		key := cacheListKey(tenantID, schemaName, limit, offset, filters)
		// The cache keeps its own copy so that what it holds stays as the
		// database returned it, whatever the caller does to the page it got.
		_ = s.listCache.Set(ctx, key, cloneContents(items), 0) // err suppressed: cache is best-effort
	}
	return items, nil
}

// GetByID returns a single row by its UUID primary key, scoped to the
// calling tenant.
//
// A deadlock victim is retried. SQL Server serves READ COMMITTED with shared
// locks unless READ_COMMITTED_SNAPSHOT is on, so an ordinary read of a row a
// concurrent write is touching can be rolled back with error 1205 and reach the
// caller as a 503 on a request that never made it to the row. The read did not
// happen, so running it again is not a duplicate. Postgres and MySQL read from
// a snapshot and rarely take this path, but the retry costs them nothing.
func (s *ContentStore) GetByID(ctx context.Context, schemaName string, id uuid.UUID) (*domain.Content, error) {
	var out *domain.Content
	err := sqlx.RetryOnTxConflict(ctx, "content get", func() error {
		var gerr error
		out, gerr = s.getByIDOnce(ctx, schemaName, id)
		return gerr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *ContentStore) getByIDOnce(ctx context.Context, schemaName string, id uuid.UUID) (*domain.Content, error) {
	// Cache read
	tenantID := tenant.ID(ctx)
	if s.itemCache != nil {
		key := cacheItemKey(tenantID, schemaName, id)
		if cached, ok := s.itemCache.Get(ctx, key); ok {
			return cloneContent(cached), nil
		}
	}

	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, fmt.Errorf("content get: resolve table: %w", err)
	}

	row, qErr := s.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id, created_at, updated_at,
		       %s
		FROM %s r
		WHERE id = $1 AND tenant_id = $2%s`, dataColumnExpr(s.pool.Engine(), sc, "r"), table, softDeleteWhere(sc)),
		id, tenantID,
	)
	if qErr != nil {
		return nil, fmt.Errorf("content get: %w", qErr)
	}
	c, err := scanFullRow(s.pool.Engine(), row, schemaName, sc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Cache write
	if s.itemCache != nil {
		_ = s.itemCache.Set(ctx, cacheItemKey(tenantID, schemaName, id), cloneContent(c), 0) // err suppressed: cache is best-effort
	}
	return c, nil
}

// softDeleteWhere returns an extra WHERE clause fragment when soft-delete is enabled.
func softDeleteWhere(sc *domain.Schema) string {
	if sc != nil && sc.WithSoftDelete {
		return " AND deleted_at IS NULL"
	}
	return ""
}

// Update replaces the provided fields in an existing row.
// The WHERE clause includes AND updated_at = expectedUpdatedAt to prevent
// lost updates in multi-instance deployments. If the row was modified
// by another request since the caller read it, Update returns ErrContentConflict.
//
// A deadlock victim is retried. SQL Server picks one of two contending writers
// and rolls its statement back whole with error 1205, and MySQL and Postgres do
// the same under their own codes. Without a retry that concurrency loss would
// reach the caller as a 503 on a write that never happened.
func (s *ContentStore) Update(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any, expectedUpdatedAt time.Time) (*domain.Content, error) {
	var out *domain.Content
	err := sqlx.RetryOnTxConflict(ctx, "content update", func() error {
		var uerr error
		out, uerr = s.updateOnce(ctx, schemaName, id, data, expectedUpdatedAt)
		return uerr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// updateOnce runs one attempt of Update. Safe to repeat: a rolled-back
// statement leaves no row changed, and the optimistic updated_at check still
// catches a genuine lost update.
func (s *ContentStore) updateOnce(ctx context.Context, schemaName string, id uuid.UUID, data map[string]any, expectedUpdatedAt time.Time) (*domain.Content, error) {
	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, err
	}

	sets, args := buildUpdate(sc, data, dialect.Must(s.pool.Engine()))
	if len(sets) == 0 {
		return s.GetByID(ctx, schemaName, id)
	}

	d := dialect.Must(s.pool.Engine())
	tenantID := tenant.ID(ctx)

	// Build WHERE clause: always filter by id and tenant. Add the optimistic
	// updated_at check when requested. A row that belongs to another tenant
	// matches nothing and fails the update the same way a stale row does.
	args = append(args, id, tenantID)
	where := fmt.Sprintf("WHERE id = $%d AND tenant_id = $%d", len(args)-1, len(args))
	if !expectedUpdatedAt.IsZero() {
		args = append(args, expectedUpdatedAt)
		where = fmt.Sprintf("WHERE id = $%d AND updated_at = $%d AND tenant_id = $%d", len(args)-2, len(args), len(args)-1)
	}

	if s.pool.Engine() == "postgres" {
		row, qErr := s.pool.QueryRow(ctx,
			fmt.Sprintf(`UPDATE %s AS r SET %s, updated_at = `+d.NowFunc()+` %s RETURNING %s`,
				table, strings.Join(sets, ", "), where, writtenRowColumns(s.pool.Engine(), sc)),
			args...,
		)
		if qErr != nil {
			return nil, fmt.Errorf("content update: %w", qErr)
		}
		c, err := scanWrittenRow(s.pool.Engine(), row, schemaName, sc)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrContentConflict
		}
		if err != nil {
			return nil, err
		}
		s.invalidateSchema(ctx, schemaName, id)
		return c, nil
	}

	// MySQL / MSSQL: UPDATE then SELECT.
	tag, execErr := s.pool.Exec(ctx,
		fmt.Sprintf(`UPDATE %s SET %s, updated_at = `+d.NowFunc()+` %s`,
			table, strings.Join(sets, ", "), where),
		args...,
	)
	if execErr != nil {
		return nil, fmt.Errorf("content update: %w", execErr)
	}
	n, _ := tag.RowsAffected()
	if n == 0 {
		return nil, ErrContentConflict
	}
	row, qErr := s.pool.QueryRow(ctx, writtenRowSelect(s.pool.Engine(), sc, table), id, tenantID)
	if qErr != nil {
		return nil, fmt.Errorf("content update select: %w", qErr)
	}
	c, err := scanWrittenRow(s.pool.Engine(), row, schemaName, sc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("content update select: %w", err)
	}
	s.invalidateSchema(ctx, schemaName, id)
	return c, nil
}

// Delete removes a row by UUID, scoped to the calling tenant. When the schema
// has WithSoftDelete enabled, the row is soft-deleted (deleted_at = NOW())
// instead of physically removed.
func (s *ContentStore) Delete(ctx context.Context, schemaName string, id uuid.UUID) error {
	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return fmt.Errorf("delete %s: %w", schemaName, err)
	}
	tenantID := tenant.ID(ctx)
	var n int64
	switch {
	case sc.WithSoftDelete:
		d := dialect.Must(s.pool.Engine())
		tag, execErr := s.pool.Exec(ctx,
			fmt.Sprintf(`UPDATE %s SET deleted_at = `+d.NowFunc()+` WHERE id = $1 AND deleted_at IS NULL AND tenant_id = $2`, table), id, tenantID)
		if execErr != nil {
			return fmt.Errorf("soft-delete from %s: %w", table, execErr)
		}
		n, _ = tag.RowsAffected() // err suppressed: pgx always reports 0 for unsupported, not an error
	case s.pool.Engine() == "mssql":
		var derr error
		n, derr = s.deleteApplyingActions(ctx, schemaName, id)
		if derr != nil {
			return derr
		}
	default:
		tag, execErr := s.pool.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE id = $1 AND tenant_id = $2`, table), id, tenantID)
		if execErr != nil {
			return fmt.Errorf("delete from %s: %w", table, execErr)
		}
		n, _ = tag.RowsAffected() // err suppressed: pgx always reports 0 for unsupported, not an error
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	s.invalidateSchema(ctx, schemaName, id)
	return nil
}

// deleteApplyingActions deletes a row and performs the referential actions
// its foreign keys would have performed on the other two dialects.
//
// SQL Server refuses a foreign key that gives one table a second cascading
// path to another, so on that dialect every generated key is declared with
// no action and the actions live here: pivot rows naming the row go, rows of
// a required belongs_to go with it, rows of an optional one lose the key. It
// runs in one transaction, so a failure part-way leaves nothing half done,
// and it walks the tenant's schema list rather than the catalog because the
// action to take is a property of the field, not of the constraint.
func (s *ContentStore) deleteApplyingActions(ctx context.Context, schemaName string, id uuid.UUID) (int64, error) {
	schemas, err := s.schemas.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete %s: list schemas: %w", schemaName, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete %s: begin tx: %w", schemaName, err)
	}
	defer tx.Rollback() //nolint:errcheck // deferred rollback. Commit result takes precedence

	w := &actionWalker{store: s, ctx: ctx, tx: tx, engine: s.pool.Engine(), tenantID: tenant.ID(ctx), schemas: schemas, seen: map[string]bool{}}
	n, err := w.delete(schemaName, id)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("delete %s: commit: %w", schemaName, err)
	}
	// Rows of other schemas changed too, and which ones is not known here by
	// id, so the item cache keeps them until the TTL and the list cache is
	// flushed whole by the caller's own invalidation.
	return n, nil
}

// actionWalker carries the transaction and the tenant's schemas through the
// recursive delete. seen guards a cycle of required belongs_to fields, which
// the database would refuse to create but the schema list can still describe.
type actionWalker struct {
	store    *ContentStore
	ctx      context.Context
	tx       *sql.Tx
	engine   string
	tenantID string
	schemas  []*domain.Schema
	seen     map[string]bool
}

func (w *actionWalker) exec(q string, args ...any) (int64, error) {
	q, args = rewritePlaceholders(q, w.engine, args)
	tag, err := w.tx.ExecContext(w.ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := tag.RowsAffected()
	return n, nil
}

func (w *actionWalker) delete(schemaName string, id uuid.UUID) (int64, error) {
	key := schemaName + "/" + id.String()
	if w.seen[key] {
		return 0, nil
	}
	w.seen[key] = true

	ownCol := strings.TrimPrefix(domain.TableName(schemaName), "_") + "_id"
	for _, sc := range w.schemas {
		for _, f := range sc.Fields {
			if f.FieldType != "relation" || f.RelationTo == "" {
				continue
			}
			switch f.RelationType {
			case domain.RelBelongsTo, "":
				if f.RelationTo != schemaName {
					continue
				}
				fk := domain.FKColumn(f.Name, f.RelationFKName)
				table := w.store.qualify(domain.TableName(sc.Name))
				if !f.Required {
					if _, err := w.exec(fmt.Sprintf(`UPDATE %s SET %s = NULL WHERE %s = $1 AND tenant_id = $2`, table, fk, fk), id, w.tenantID); err != nil {
						return 0, fmt.Errorf("delete %s: clear %s.%s: %w", schemaName, sc.Name, fk, err)
					}
					continue
				}
				children, err := w.ids(fmt.Sprintf(`SELECT id FROM %s WHERE %s = $1 AND tenant_id = $2`, table, fk), id, w.tenantID)
				if err != nil {
					return 0, fmt.Errorf("delete %s: find %s rows: %w", schemaName, sc.Name, err)
				}
				for _, child := range children {
					if _, err := w.delete(sc.Name, child); err != nil {
						return 0, err
					}
				}
			case domain.RelManyToMany:
				pivot := f.RelationThrough
				if pivot == "" {
					pivot = domain.PivotTableName(sc.Name, f.RelationTo)
				}
				pivot = w.store.qualify(pivot)
				for _, side := range []string{sc.Name, f.RelationTo} {
					if side != schemaName {
						continue
					}
					if _, err := w.exec(fmt.Sprintf(`DELETE FROM %s WHERE %s = $1 AND tenant_id = $2`, pivot, ownCol), id, w.tenantID); err != nil {
						return 0, fmt.Errorf("delete %s: clear pivot %s: %w", schemaName, pivot, err)
					}
				}
			}
		}
	}

	table := w.store.qualify(domain.TableName(schemaName))
	n, err := w.exec(fmt.Sprintf(`DELETE FROM %s WHERE id = $1 AND tenant_id = $2`, table), id, w.tenantID)
	if err != nil {
		return 0, fmt.Errorf("delete from %s: %w", table, err)
	}
	return n, nil
}

func (w *actionWalker) ids(q string, args ...any) ([]uuid.UUID, error) {
	q, args = rewritePlaceholders(q, w.engine, args)
	rows, err := w.tx.QueryContext(w.ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(scanUUID(w.engine, &id)); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListCursor returns up to limit rows from the collection table using
// cursor-based (keyset) pagination for efficient large-dataset traversal.
// cursor is the UUID of the last seen row. Pass "" for the first page.
// Rows are returned in ascending id order.
func (s *ContentStore) ListCursor(ctx context.Context, schemaName string, cursor string, limit int) ([]*domain.Content, error) {
	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 20
	}

	engine := s.pool.Engine()
	dataExpr := dataColumnExpr(engine, sc, "r")
	limitClause := "LIMIT $1"
	if engine == "mssql" {
		limitClause = "OFFSET 0 ROWS FETCH NEXT $1 ROWS ONLY"
	}
	tenantID := tenant.ID(ctx)

	var (
		query string
		args  []any
	)
	if cursor == "" {
		query = fmt.Sprintf(`
			SELECT id, created_at, updated_at,
			       %s
			FROM %s r
			WHERE tenant_id = $2
			ORDER BY id ASC
			%s`, dataExpr, table, limitClause)
		args = []any{limit, tenantID}
	} else {
		cursorID, parseErr := uuid.Parse(cursor)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid cursor: %w", parseErr)
		}
		query = fmt.Sprintf(`
			SELECT id, created_at, updated_at,
			       %s
			FROM %s r
			WHERE id > $2 AND tenant_id = $3
			ORDER BY id ASC
			%s`, dataExpr, table, limitClause)
		args = []any{limit, cursorID, tenantID}
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list cursor %s: %w", table, err)
	}
	defer rows.Close()

	items := make([]*domain.Content, 0)
	for rows.Next() {
		c, scanErr := scanFullRow(s.pool.Engine(), rows, schemaName, sc)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

// BulkInsert inserts multiple rows in a single transaction.
// Returns the inserted records. If any row fails the whole batch is rolled back.
func (s *ContentStore) BulkInsert(ctx context.Context, schemaName string, items []map[string]any) ([]*domain.Content, error) {
	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin bulk insert tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // deferred rollback. Commit result takes precedence

	isPG := s.pool.Engine() == "postgres"
	tenantID := tenant.ID(ctx)
	tenantCol := dialect.Must(s.pool.Engine()).QuoteIdentifier(TenantColumnName)
	results := make([]*domain.Content, 0, len(items))
	for _, data := range items {
		cols, args := buildInsert(sc, data, dialect.Must(s.pool.Engine()))
		cols = append([]string{tenantCol}, cols...)
		args = append([]any{tenantID}, args...)
		if isPG {
			ph := placeholders(len(args))
			row := tx.QueryRowContext(ctx,
				fmt.Sprintf(`INSERT INTO %s AS r (%s) VALUES (%s) RETURNING %s`,
					table, strings.Join(cols, ", "), ph, writtenRowColumns(s.pool.Engine(), sc)),
				args...,
			)
			c, scanErr := scanWrittenRow(s.pool.Engine(), row, schemaName, sc)
			if scanErr != nil {
				return nil, fmt.Errorf("bulk insert row scan: %w", scanErr)
			}
			results = append(results, c)
		} else {
			// MySQL / MSSQL: generate UUID in Go, INSERT with explicit id, then SELECT.
			newID := uuid.New()
			cols = append([]string{"id"}, cols...)
			args = append([]any{newID}, args...)
			ph := placeholders(len(args))

			insertSQL, insertArgs := rewritePlaceholders(
				fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(cols, ", "), ph),
				s.pool.Engine(), args,
			)
			if _, execErr := tx.ExecContext(ctx, insertSQL, insertArgs...); execErr != nil {
				return nil, fmt.Errorf("bulk insert row: %w", execErr)
			}
			selectSQL, selectArgs := rewritePlaceholders(
				writtenRowSelect(s.pool.Engine(), sc, table),
				s.pool.Engine(), []any{newID, tenantID},
			)
			result, scanErr := scanWrittenRow(s.pool.Engine(), tx.QueryRowContext(ctx, selectSQL, selectArgs...), schemaName, sc)
			if scanErr != nil {
				return nil, fmt.Errorf("bulk insert row select: %w", scanErr)
			}
			results = append(results, result)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit bulk insert: %w", err)
	}
	return results, nil
}

// SetStatus updates the _status column of a content row. The status is one
// of draft, published and archived, and the schema must have draft and
// publish enabled, or there is no column to write. Both are refused as
// domain.ErrValidation before any SQL runs, so a caller that passes a value
// through from a plugin or a request cannot store one the read filters never
// match. A row the tenant does not have is domain.ErrNotFound.
func (s *ContentStore) SetStatus(ctx context.Context, schemaName string, id uuid.UUID, status string) error {
	if !core.ValidContentStatus(status) {
		return fmt.Errorf("set status %s: %w: unknown status %q", schemaName, domain.ErrValidation, status)
	}
	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return fmt.Errorf("set status %s: %w", schemaName, err)
	}
	if !sc.WithDraftPublish {
		return fmt.Errorf("set status %s: %w: schema has no draft and publish", schemaName, domain.ErrValidation)
	}
	d := dialect.Must(s.pool.Engine())
	tag, err := s.pool.Exec(ctx,
		fmt.Sprintf(`UPDATE %s SET _status = $1, updated_at = `+d.NowFunc()+` WHERE id = $2 AND tenant_id = $3`, table),
		status, id, tenant.ID(ctx),
	)
	if err != nil {
		return fmt.Errorf("set status %s/%s: %w", schemaName, id, err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	s.invalidateSchema(ctx, schemaName, id)
	return nil
}

// jsonColumn scans a JSON-valued column that a driver may return as either
// []byte (PostgreSQL to_jsonb, MySQL JSON_OBJECT) or string (MSSQL FOR JSON /
// NVARCHAR), normalizing both to json.RawMessage.
type jsonColumn struct{ raw json.RawMessage }

func (j *jsonColumn) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		j.raw = nil
	case []byte:
		j.raw = append(json.RawMessage(nil), v...)
	case string:
		j.raw = json.RawMessage(v)
	default:
		return fmt.Errorf("jsonColumn: unsupported scan type %T", src)
	}
	return nil
}

// scanFullRow scans a row that returns (id, created_at, updated_at, data::jsonb).
func scanFullRow(engine string, row interface{ Scan(...any) error }, schemaName string, sc *domain.Schema) (*domain.Content, error) {
	c := &domain.Content{SchemaName: schemaName}
	var rawData jsonColumn
	if err := row.Scan(scanUUID(engine, &c.ID), &c.CreatedAt, &c.UpdatedAt, &rawData); err != nil {
		return nil, fmt.Errorf("scan content row: %w", err)
	}
	if len(rawData.raw) > 0 {
		if err := json.Unmarshal(rawData.raw, &c.Data); err != nil {
			return nil, fmt.Errorf("unmarshal content data: %w", err)
		}
	}
	if c.Data == nil {
		c.Data = make(map[string]any)
	}
	normalizeContentData(engine, sc, c.Data)
	return c, nil
}

// contentTimeLayout is how a datetime reads on every dialect. It is the form
// PostgreSQL's to_jsonb gives a TIMESTAMPTZ in a UTC session: an explicit
// +00:00 offset and at most microseconds, trailing zeros dropped.
const contentTimeLayout = "2006-01-02T15:04:05.999999-07:00"

// storedTimeLayouts are the forms a datetime column reaches the data object
// in. MySQL's JSON_OBJECT writes DATETIME(6) with a space and no offset, and
// SQL Server's FOR JSON writes DATETIME2 with a T and no offset. Both columns
// hold UTC, because every write binds the value in UTC.
var storedTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
}

// normalizeContentData gives every field the JSON type PostgreSQL reads it
// as, from the schema definition, so an entry reads the same on every
// dialect. MySQL builds the data object with JSON_OBJECT, which knows a
// TINYINT(1) only as a number and a DATETIME only as text. SQL Server's FOR
// JSON writes a json field's NVARCHAR as an escaped string and a
// UNIQUEIDENTIFIER in upper case.
func normalizeContentData(engine string, sc *domain.Schema, data map[string]any) {
	if sc == nil {
		return
	}
	for _, f := range sc.Fields {
		if f.System || isVirtualRelationField(f) {
			continue
		}
		col := fieldColumn(f)
		v, ok := data[col]
		if !ok || v == nil {
			continue
		}
		switch f.FieldType {
		case "boolean":
			if n, isNum := v.(float64); isNum {
				data[col] = n != 0
			}
		case "datetime":
			data[col] = normalizeContentTime(v)
		case "json":
			if str, isStr := v.(string); isStr && engine == "mssql" {
				var decoded any
				if json.Unmarshal([]byte(str), &decoded) == nil {
					data[col] = decoded
				}
			}
		case "uid", "relation":
			if str, isStr := v.(string); isStr && engine == "mssql" {
				data[col] = strings.ToLower(str)
			}
		}
	}
	if sc.WithSoftDelete {
		if v, ok := data["deleted_at"]; ok && v != nil {
			data["deleted_at"] = normalizeContentTime(v)
		}
	}
}

// writtenTimeLayouts are the forms a caller may send a datetime in. The
// first carries its offset. The rest carry none and are read as UTC.
var writtenTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02",
}

// parseContentTime reads a datetime a caller sent, in UTC.
func parseContentTime(s string) (time.Time, bool) {
	for _, layout := range writtenTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// normalizeContentTime rewrites a stored datetime into contentTimeLayout. A
// value in no known form is returned as it came, since dropping it would lose
// data the caller can still read.
func normalizeContentTime(v any) any {
	str, ok := v.(string)
	if !ok {
		return v
	}
	for _, layout := range storedTimeLayouts {
		if t, err := time.Parse(layout, str); err == nil {
			return t.UTC().Format(contentTimeLayout)
		}
	}
	return v
}

// dataColumnExpr returns the SQL expression that extracts user-defined columns
// as a JSON value, aliased as "data". PostgreSQL uses to_jsonb, and MySQL/MSSQL
// uses JSON_OBJECT built from the schema field list.
// Column-name references are dialect-quoted (backticks/brackets/double-quotes)
// via QuoteIdentifier so reserved-word field names (order, group, key, etc.)
// do not cause syntax errors.
func dataColumnExpr(engine string, sc *domain.Schema, alias string) string {
	if engine == "postgres" {
		// tenant_id is an isolation marker, never user data: exclude it from
		// the returned JSON object.
		return fmt.Sprintf("(to_jsonb(%s) - 'id' - 'created_at' - 'updated_at' - '%s') AS data", alias, TenantColumnName)
	}
	// MySQL / MSSQL: build a JSON object from known schema columns.
	d := dialect.Must(engine)
	qi := d.QuoteIdentifier
	cols := make([]string, 0, len(sc.Fields)+3)
	for _, f := range sc.Fields {
		if f.System {
			continue
		}
		// Virtual relations (has_one, has_many, many_to_many) hold no column
		// on this table. PostgreSQL's to_jsonb never emits them, and the
		// physical column does not exist to reference here.
		if f.FieldType == "relation" &&
			(f.RelationType == domain.RelHasOne || f.RelationType == domain.RelHasMany || f.RelationType == domain.RelManyToMany) {
			continue
		}
		cols = append(cols, fieldColumn(f))
	}
	// Include optional system-managed columns.
	if sc.WithSoftDelete {
		cols = append(cols, "deleted_at")
	}
	if sc.WithDraftPublish {
		cols = append(cols, "_status")
	}
	if sc.WithLocalization {
		cols = append(cols, "_locale")
	}

	if engine == "mssql" {
		// Azure SQL Edge and SQL Server < 2022 do not support JSON_OBJECT. Build the
		// JSON object with a correlated FOR JSON PATH subquery instead. The column
		// alias becomes the JSON key, and INCLUDE_NULL_VALUES matches JSON_OBJECT's
		// semantics of emitting a key even when the value is NULL.
		if len(cols) == 0 {
			return "'{}' AS data"
		}
		sel := make([]string, len(cols))
		for i, col := range cols {
			sel[i] = alias + "." + qi(col) + " AS " + qi(col)
		}
		return "(SELECT " + strings.Join(sel, ", ") +
			" FOR JSON PATH, WITHOUT_ARRAY_WRAPPER, INCLUDE_NULL_VALUES) AS data"
	}

	// MySQL: JSON_OBJECT('col', alias.`col`, ...).
	if len(cols) == 0 {
		return "JSON_OBJECT() AS data"
	}
	parts := make([]string, 0, len(cols)*2)
	for _, col := range cols {
		parts = append(parts, fmt.Sprintf("'%s'", col), alias+"."+qi(col))
	}
	return "JSON_OBJECT(" + strings.Join(parts, ", ") + ") AS data"
}

// writtenRowColumns is the projection a write reads back: the system columns
// and the data object the read path builds, with the row aliased r. A write
// answers with the row as GET returns it, so a relation sits under its column
// name, a default the caller omitted is present, and a value has the type the
// column gave it. Echoing the caller's map instead would show a subscriber to
// after_create a relation under its field name and the id as raw bytes, while
// the read API shows the column and a string.
func writtenRowColumns(engine string, sc *domain.Schema) string {
	return "id, created_at, updated_at, " + dataColumnExpr(engine, sc, "r")
}

// writtenRowSelect reads the row a write just produced on the dialects that
// have no RETURNING.
func writtenRowSelect(engine string, sc *domain.Schema, table string) string {
	return fmt.Sprintf(`SELECT %s FROM %s r WHERE id = $1 AND tenant_id = $2`, writtenRowColumns(engine, sc), table)
}

// scanWrittenRow scans the projection writtenRowColumns names and stamps the
// id into Data as the string the read API and every event carry.
func scanWrittenRow(engine string, row interface{ Scan(...any) error }, schemaName string, sc *domain.Schema) (*domain.Content, error) {
	c, err := scanFullRow(engine, row, schemaName, sc)
	if err != nil {
		return nil, err
	}
	c.Data["id"] = c.ID.String()
	return c, nil
}

// buildInsert returns column names and argument values for an INSERT, using
// only schema-defined non-system fields present in data.
// Column names come from the schema definition (server-controlled, never
// from user input), so no SQL injection surface exists here.
// For belongs_to relation fields the physical column is {name}_id. Data is
// keyed by either the logical name or the FK column name (both accepted).
// Column names are quoted via d.QuoteIdentifier for dialect safety.
func buildInsert(sc *domain.Schema, data map[string]any, d dialect.Dialect) (cols []string, args []any) {
	qi := d.QuoteIdentifier
	for _, f := range sc.Fields {
		if f.System || isVirtualRelationField(f) {
			// A has_one, has_many or many_to_many keeps nothing on this table.
			// A value sent under its name would become a column in the INSERT
			// that does not exist and fail the whole write.
			continue
		}
		col := fieldColumn(f)
		// Accept either the logical field name or the physical column name as key.
		v, ok := data[col]
		if !ok {
			v, ok = data[f.Name]
		}
		if !ok {
			// The DDL generator emits no DEFAULT clause, so a declared default
			// is applied here, or a field relying on one would store NULL. It
			// is applied here rather than in the DDL because MySQL cannot
			// carry a literal DEFAULT on the TEXT column that a text field
			// becomes, which is the common case.
			//
			// Both inserts reach this function, so a create through the public
			// route and an admin write projected into the same table agree.
			if f.Default == nil {
				continue
			}
			v = f.Default
		}
		cols = append(cols, qi(col))
		args = append(args, bindContentValue(f, v))
	}
	return
}

// buildUpdate returns SET clauses and argument values for an UPDATE.
// Column names are quoted via d.QuoteIdentifier for dialect safety.
func buildUpdate(sc *domain.Schema, data map[string]any, d dialect.Dialect) (sets []string, args []any) {
	qi := d.QuoteIdentifier
	i := 1
	for _, f := range sc.Fields {
		if f.System || isVirtualRelationField(f) {
			continue
		}
		col := fieldColumn(f)
		v, ok := data[col]
		if !ok {
			v, ok = data[f.Name]
		}
		if ok {
			sets = append(sets, fmt.Sprintf("%s = $%d", qi(col), i))
			args = append(args, bindContentValue(f, v))
			i++
		}
	}
	return
}

// bindContentValue prepares a user-supplied field value for binding.
//
// A value that came out of encoding/json as a composite (map[string]any for a
// nested object, []any for a list) cannot be bound by any driver as-is. pgx
// encodes both into jsonb by itself. The MySQL and MSSQL drivers take scalars
// only and reject a map outright, so content that saves on Postgres would fail
// on the other two with "unsupported type map[string]interface {}".
// Re-encoding gives all three the same bytes.
//
// Only those two shapes are converted. A typed slice such as []string is
// something the caller built deliberately, and pgx binds it to a Postgres
// array. Encoding it as JSON would break that. Text and raw bytes pass through
// as JSON already, or as an encoding the caller chose.
func bindContentValue(f domain.SchemaField, v any) any {
	// A datetime is bound as a time in UTC rather than as the caller's text.
	// MySQL refuses an RFC 3339 value with a Z or an offset in a DATETIME
	// column, and neither MySQL's DATETIME nor SQL Server's DATETIME2 keeps
	// an offset, so the read path takes what they hold to be UTC. Text with
	// no offset is read as UTC on every dialect. Bound as text, PostgreSQL
	// would have read it in the session's zone and the other two as the
	// wall clock, so one value would have meant three instants.
	if str, ok := v.(string); ok && f.FieldType == "datetime" {
		if t, ok := parseContentTime(str); ok {
			return t
		}
		return v
	}
	switch v.(type) {
	case map[string]any, []any:
	default:
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	return string(b)
}

// fieldColumn returns the physical DB column name for a schema field.
// For belongs_to relation fields this is {name}_id (the FK column).
// For all other field types it equals f.Name.
func fieldColumn(f domain.SchemaField) string {
	if f.FieldType == "relation" && (f.RelationType == domain.RelBelongsTo || f.RelationType == "") {
		return domain.FKColumn(f.Name, f.RelationFKName)
	}
	return f.Name
}

func placeholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(p, ", ")
}

// isVirtualRelationField reports whether a relation field stores nothing in
// the row. Only belongs_to keeps a foreign key column, so the other three
// kinds are read through the other side or a pivot table and have no column
// here to select or write.
func isVirtualRelationField(f domain.SchemaField) bool {
	if f.FieldType != "relation" {
		return false
	}
	switch f.RelationType {
	case domain.RelHasOne, domain.RelHasMany, domain.RelManyToMany:
		return true
	}
	return false
}
