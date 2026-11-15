package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// parsePopulateField extracts the field name and optional field mask from a
// populate path segment. Supports "field{id,name}" syntax for field selection.
// Returns the bare field name and the list of fields to include (nil = all fields).
//
//	"author"            -> "author", nil
//	"author{id,name}"   -> "author", ["id", "name"]
//	"*{id,name}"        -> "*", ["id", "name"]
func parsePopulateField(segment string) (field string, mask []string) {
	field = segment
	if idx := strings.Index(segment, "{"); idx >= 0 {
		if end := strings.Index(segment, "}"); end > idx {
			field = segment[:idx]
			maskStr := segment[idx+1 : end]
			for _, f := range strings.Split(maskStr, ",") {
				f = strings.TrimSpace(f)
				if f != "" {
					mask = append(mask, f)
				}
			}
		}
	}
	return field, mask
}

// applyFieldMask filters a flattened content map to only include the specified
// fields. System fields (id, created_at, updated_at) are always included.
// If mask is nil or empty, all fields are returned.
func applyFieldMask(data map[string]any, mask []string) map[string]any {
	if len(mask) == 0 {
		return data
	}
	allowed := make(map[string]bool, len(mask)+3)
	for _, f := range mask {
		allowed[f] = true
	}
	allowed["id"] = true
	allowed["created_at"] = true
	allowed["updated_at"] = true

	out := make(map[string]any, len(allowed))
	for k, v := range data {
		if allowed[k] {
			out[k] = v
		}
	}
	return out
}

// populateRootFields resolves the immediate (non-nested) field names to populate
// at the current level, given a PopulateConfig and the schema's relation fields.
// Returns the flat list of field names, remaining nested paths per field, and
// optional field masks parsed from "{...}" suffixes.
func populateRootFields(cfg domain.PopulateConfig, allRelationFields []string) (fields []string, nested map[string][]string, masks map[string][]string) {
	nested = make(map[string][]string)
	masks = make(map[string][]string)
	hasWildcard := false
	var wildcardMask []string

	for _, path := range cfg.Paths {
		// Split at first dot: "author{id,name}.avatar{url}" -> "author{id,name}", "avatar{url}"
		parts := strings.SplitN(path, ".", 2)
		root := strings.TrimSpace(parts[0])
		if root == "" {
			continue
		}
		// Check for wildcard (with or without mask): "*" or "*{id,name}"
		if strings.HasPrefix(root, "*") {
			hasWildcard = true
			_, wm := parsePopulateField(root)
			if len(wm) > 0 {
				wildcardMask = wm
			}
			continue
		}
		// Parse field name and optional mask: "author{id,name}" -> "author", ["id","name"]
		fieldName, fieldMask := parsePopulateField(root)
		fields = append(fields, fieldName)
		if len(fieldMask) > 0 {
			masks[fieldName] = fieldMask
		}
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			nested[fieldName] = append(nested[fieldName], strings.TrimSpace(parts[1]))
		}
	}

	// If maxDepth > 0 OR hasWildcard, include ALL relation fields at this level
	if cfg.MaxDepth > 0 || hasWildcard {
		seen := make(map[string]bool, len(fields))
		for _, f := range fields {
			seen[f] = true
		}
		for _, rf := range allRelationFields {
			if !seen[rf] {
				fields = append(fields, rf)
				// Apply wildcard mask to auto-discovered fields
				if len(wildcardMask) > 0 {
					masks[rf] = wildcardMask
				}
			}
		}
	}

	// Deduplicate while preserving order
	seen := make(map[string]bool, len(fields))
	var result []string
	for _, f := range fields {
		if !seen[f] {
			seen[f] = true
			result = append(result, f)
		}
	}
	return result, nested, masks
}

// nextPopulateConfig constructs the PopulateConfig for a nested level.
// If there are explicit nested paths for this field, use those.
// Otherwise, if nextDepth > 0, auto-populate all relations at the next level.
func nextPopulateConfig(fieldName string, nestedPaths map[string][]string, nextDepth int) domain.PopulateConfig {
	cfg := domain.PopulateConfig{MaxDepth: nextDepth}
	if paths, ok := nestedPaths[fieldName]; ok {
		cfg.Paths = paths
	}
	return cfg
}

// The bounds of one populate walk. ?depth= and the wildcard reach relations
// no request names, and a level serves a related entry once under every
// record that points at it, so a walk with no bound turns one page into
// every row of a relation, many times over.
const (
	// MaxPopulateDepth is the deepest level a walk reaches. It also ends a
	// walk around a circular relation.
	MaxPopulateDepth = 10
	// maxPopulateEntries is the most related entries one walk reads, and the
	// most it serves. An entry served under several records counts once for
	// each, since the answer carries a copy under each.
	maxPopulateEntries = 5000
	// maxPopulateBytes bounds the related entries one walk reads, and the
	// ones it serves, by the bytes of their data, since a rich text or JSON
	// field can hold a whole request body.
	maxPopulateBytes = 32 << 20
	// maxPopulateReads is the most queries one walk runs. A wildcard at every
	// level of a deep walk reads each relation again at each level.
	maxPopulateReads = 1000
)

// ErrPopulateTooLarge is a walk that would pass one of its bounds. The read
// is refused whole, because a relation cut short reads as complete.
var ErrPopulateTooLarge = fmt.Errorf("populate reaches past its bounds: %w", domain.ErrBadRequest)

// populateLoad is an amount of related entries and the bytes of their data.
type populateLoad struct {
	entries, bytes int
}

func (l populateLoad) plus(o populateLoad) populateLoad {
	return populateLoad{entries: l.entries + o.entries, bytes: l.bytes + o.bytes}
}

// within refuses a load past the walk's bounds.
func (l populateLoad) within() error {
	if l.entries > maxPopulateEntries || l.bytes > maxPopulateBytes {
		return ErrPopulateTooLarge
	}
	return nil
}

// populateWalk is one request's walk: the gate every relation is put to,
// whether a failed read is returned, and what the walk has read so far.
type populateWalk struct {
	gate   RelationGate
	strict bool
	read   populateLoad
	reads  int
	sizes  map[*domain.Content]int
}

// query counts one query against the walk's bound.
func (w *populateWalk) query() error {
	if w.reads++; w.reads > maxPopulateReads {
		return ErrPopulateTooLarge
	}
	return nil
}

// take counts a related row the walk read, size being the bytes of its data.
func (w *populateWalk) take(c *domain.Content, size int) error {
	w.sizes[c] = size
	w.read = w.read.plus(populateLoad{entries: 1, bytes: size})
	return w.read.within()
}

// limit is the most rows a to-many query reads: what the walk may still read
// and one more, which tells a relation past the bound from one that ends at it
// without reading the rest.
func (w *populateWalk) limit() int {
	return maxPopulateEntries - w.read.entries + 1
}

// served is what one related entry carries in the answer: itself, the bytes
// of its data, and what the levels below served under it.
func (w *populateWalk) served(c *domain.Content, below map[*domain.Content]populateLoad) populateLoad {
	return populateLoad{entries: 1, bytes: w.sizes[c]}.plus(below[c])
}

// distinctIDs is ids with each one once, in the order first seen, so an IN
// list never names a parent twice and no parent gets its children twice.
func distinctIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// InverseKeyError is a has_one or has_many the request names whose key on
// its target cannot be read by the rule core.InverseKey follows.
type InverseKeyError struct {
	Field, Target string
}

func (e *InverseKeyError) Error() string {
	return fmt.Sprintf("populate %s: no single key on %s points back", e.Field, e.Target)
}

// Unwrap makes the refusal a bad request: the request named a field the
// definition gives no way to read.
func (e *InverseKeyError) Unwrap() error { return domain.ErrBadRequest }

// namedFields is the relations cfg names at its level, as against the ones a
// wildcard or a depth reaches there.
func namedFields(cfg domain.PopulateConfig) map[string]bool {
	named := map[string]bool{}
	for _, path := range cfg.Paths {
		root, _, _ := strings.Cut(path, ".")
		if name, _ := parsePopulateField(strings.TrimSpace(root)); name != "" && !strings.HasPrefix(name, "*") {
			named[name] = true
		}
	}
	return named
}

// maxBatchSize limits the number of IDs in a single IN (...) clause to avoid
// excessively large queries. If a batch exceeds this, it is split into chunks.
// 500 is chosen as a safe upper bound that works across all three dialects
// without hitting parameter limits (PG: 65535, MySQL: 65535, MSSQL: 2100).
const maxBatchSize = 500

// GetByIDs returns multiple content rows by their UUID primary keys in a single
// query. Items found in the item cache are served from it, and only uncached
// IDs are fetched from the database and written back to the cache. Every row
// returned is the caller's own copy, as GetByID's are: population writes the
// related record into the row's Data, so a cached row handed out as it is
// would carry one request's relations into every later read of that row, and
// two requests writing it at once end the process. See content_clone.go.
// Returns a map keyed by UUID for O(1) lookup. IDs that are not found are
// silently omitted from the result map (same behavior as GetByID returning
// ErrNotFound for a missing ID: the caller assigns nil to that field).
func (s *ContentStore) GetByIDs(ctx context.Context, schemaName string, ids []uuid.UUID) (map[uuid.UUID]*domain.Content, error) {
	return s.getByIDs(ctx, schemaName, ids, nil)
}

// getByIDs is GetByIDs with each row handed to take as it is read, with the
// bytes of its data, so a caller can stop the read part way. A row from the
// item cache has no size from the database, so its data is measured as JSON.
func (s *ContentStore) getByIDs(ctx context.Context, schemaName string, ids []uuid.UUID, take func(c *domain.Content, size int) error) (map[uuid.UUID]*domain.Content, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]*domain.Content{}, nil
	}

	result := make(map[uuid.UUID]*domain.Content, len(ids))
	var uncached []uuid.UUID

	// Check item cache first.
	tenantID := tenant.ID(ctx)
	for _, id := range ids {
		if s.itemCache != nil && writeTx(ctx) == nil {
			key := cacheItemKey(tenantID, schemaName, id)
			if cached, ok := s.itemCache.Get(ctx, key); ok {
				c := cloneContent(cached)
				if take != nil {
					data, err := json.Marshal(c.Data)
					if err != nil {
						return nil, fmt.Errorf("content get by ids %s: measure row: %w", schemaName, err)
					}
					if err := take(c, len(data)); err != nil {
						return nil, err
					}
				}
				result[id] = c
				continue
			}
		}
		uncached = append(uncached, id)
	}

	if len(uncached) == 0 {
		return result, nil
	}

	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, fmt.Errorf("content get by ids: resolve table: %w", err)
	}

	dataExpr := dataColumnExpr(s.pool.Engine(), sc, "r")
	sw := softDeleteWhere(sc)

	// Process in chunks to stay within parameter limits.
	for start := 0; start < len(uncached); start += maxBatchSize {
		end := start + maxBatchSize
		if end > len(uncached) {
			end = len(uncached)
		}
		chunk := uncached[start:end]

		// Build IN ($1, $2, ..., $N) clause.
		phs := make([]string, len(chunk))
		args := make([]any, len(chunk)+1)
		for i, id := range chunk {
			phs[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		args[len(chunk)] = tenantID

		query := fmt.Sprintf(`
			SELECT id, created_at, updated_at,
			       %s
			FROM %s r
			WHERE id IN (%s) AND tenant_id = $%d%s`,
			dataExpr, table, strings.Join(phs, ", "), len(chunk)+1, sw)

		rows, qErr := s.pool.Query(ctx, query, args...)
		if qErr != nil {
			return nil, fmt.Errorf("content get by ids %s: %w", table, qErr)
		}

		for rows.Next() {
			c, size, scanErr := scanSizedRow(s.pool.Engine(), rows, schemaName, sc)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			if take != nil {
				if err := take(c, size); err != nil {
					rows.Close()
					return nil, err
				}
			}
			result[c.ID] = c
			if s.itemCache != nil && writeTx(ctx) == nil {
				_ = s.itemCache.Set(ctx, cacheItemKey(tenantID, schemaName, c.ID), cloneContent(c), 0) // err suppressed: cache is best-effort
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}

	return result, nil
}

// GetByFKs returns content rows from the target schema whose fkCol value
// matches one of the given parent IDs. Results are grouped by parent ID.
// Used for batch resolution of has_one and has_many relations. Each query
// reads at most what w may still read and one row more, so a relation past
// the bound is refused without being read whole. An unpublished row is read
// only when drafts is true. Rows come newest first, the id breaking a
// created_at tie as the list does, so a has_one takes the same row each time.
func (s *ContentStore) GetByFKs(ctx context.Context, w *populateWalk, schemaName string, fkCol string, parentIDs []uuid.UUID, drafts bool) (map[uuid.UUID][]*domain.Content, error) {
	parentIDs = distinctIDs(parentIDs)
	if len(parentIDs) == 0 {
		return map[uuid.UUID][]*domain.Content{}, nil
	}

	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, fmt.Errorf("content get by fks: resolve table: %w", err)
	}

	result := make(map[uuid.UUID][]*domain.Content, len(parentIDs))
	dataExpr := dataColumnExpr(s.pool.Engine(), sc, "r")
	d := dialect.Must(s.pool.Engine())
	fkColQuoted := d.QuoteIdentifier(fkCol)
	tenantID := tenant.ID(ctx)
	// A child in the trash is not served, as GetByIDs serves no trashed
	// parent.
	visible := draftWhere(sc, "r", drafts)
	if sc.WithSoftDelete {
		visible += " AND r.deleted_at IS NULL"
	}

	for start := 0; start < len(parentIDs); start += maxBatchSize {
		end := start + maxBatchSize
		if end > len(parentIDs) {
			end = len(parentIDs)
		}
		chunk := parentIDs[start:end]

		phs := make([]string, len(chunk))
		args := make([]any, len(chunk)+1)
		for i, id := range chunk {
			phs[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		args[len(chunk)] = tenantID

		if err := w.query(); err != nil {
			return nil, err
		}
		query := fmt.Sprintf(`
			SELECT id, created_at, updated_at,
			       %s
			FROM %s r
			WHERE %s IN (%s) AND tenant_id = $%d%s
			ORDER BY created_at DESC, id DESC
			%s`,
			dataExpr, table, fkColQuoted, strings.Join(phs, ", "), len(chunk)+1, visible,
			dialect.LimitOffset(d, w.limit(), 0))

		rows, qErr := s.pool.Query(ctx, query, args...)
		if qErr != nil {
			return nil, fmt.Errorf("content get by fks %s: %w", table, qErr)
		}

		for rows.Next() {
			c, size, scanErr := scanSizedRow(s.pool.Engine(), rows, schemaName, sc)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			if err := w.take(c, size); err != nil {
				rows.Close()
				return nil, err
			}
			// Extract the FK value to determine which parent this row belongs to.
			fkVal, ok := c.Data[fkCol]
			if !ok {
				continue
			}
			fkStr, _ := fkVal.(string)
			if fkStr == "" {
				continue
			}
			fkID, parseErr := uuid.Parse(fkStr)
			if parseErr != nil {
				continue
			}
			result[fkID] = append(result[fkID], c)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}

	return result, nil
}

// ListRelatedBatch returns items from the target schema that are related to
// multiple source IDs via a many_to_many pivot table. Results are grouped
// by source ID. Each query reads at most what w may still read and one row
// more, as GetByFKs does.
func (s *ContentStore) ListRelatedBatch(ctx context.Context, w *populateWalk, schemaName string, sourceIDs []uuid.UUID, f domain.SchemaField) (map[uuid.UUID][]*domain.Content, error) {
	sourceIDs = distinctIDs(sourceIDs)
	if len(sourceIDs) == 0 {
		return map[uuid.UUID][]*domain.Content{}, nil
	}

	pivotName, err := s.pivotTable(ctx, schemaName, f)
	if err != nil {
		return nil, fmt.Errorf("list related batch: %w", err)
	}
	if err := s.ensureTable(ctx, pivotName); err != nil {
		return nil, fmt.Errorf("list related batch: ensure pivot tenant column: %w", err)
	}
	pivotName = s.qualify(pivotName)
	targetTable := s.qualify(domain.TableName(f.RelationTo))
	aCol := strings.TrimPrefix(domain.TableName(schemaName), "_") + "_id"
	bCol := strings.TrimPrefix(domain.TableName(f.RelationTo), "_") + "_id"

	relSchema, relErr := s.schemas.GetByName(ctx, f.RelationTo)
	if relErr != nil {
		return nil, fmt.Errorf("list related batch: resolve schema %q: %w", f.RelationTo, relErr)
	}
	// A target in the trash is not served, as ListRelated serves none.
	visible := ""
	if relSchema.WithSoftDelete {
		visible = " AND t.deleted_at IS NULL"
	}

	result := make(map[uuid.UUID][]*domain.Content, len(sourceIDs))

	for start := 0; start < len(sourceIDs); start += maxBatchSize {
		end := start + maxBatchSize
		if end > len(sourceIDs) {
			end = len(sourceIDs)
		}
		chunk := sourceIDs[start:end]

		phs := make([]string, len(chunk))
		args := make([]any, len(chunk)+2)
		for i, id := range chunk {
			phs[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		// Both sides of the join must stay inside the caller's tenant: the
		// pivot row and the target row alike.
		tenantID := tenant.ID(ctx)
		args[len(chunk)] = tenantID
		args[len(chunk)+1] = tenantID

		if err := w.query(); err != nil {
			return nil, err
		}
		rows, qErr := s.pool.Query(ctx, fmt.Sprintf(`
			SELECT p.%s AS source_id, t.id, t.created_at, t.updated_at,
			       %s
			FROM %s t
			JOIN %s p ON p.%s = t.id
			WHERE p.%s IN (%s) AND p.tenant_id = $%d AND t.tenant_id = $%d%s
			ORDER BY t.created_at DESC
			%s`,
			aCol,
			dataColumnExpr(s.pool.Engine(), relSchema, "t"),
			targetTable, pivotName, bCol, aCol,
			strings.Join(phs, ", "), len(chunk)+1, len(chunk)+2, visible,
			dialect.LimitOffset(dialect.Must(s.pool.Engine()), w.limit(), 0),
		), args...)
		if qErr != nil {
			return nil, fmt.Errorf("list related batch %s->%s: %w", schemaName, f.RelationTo, qErr)
		}

		for rows.Next() {
			var sourceID uuid.UUID
			c := &domain.Content{SchemaName: f.RelationTo}
			var rawData jsonColumn
			if scanErr := rows.Scan(scanUUID(s.pool.Engine(), &sourceID), scanUUID(s.pool.Engine(), &c.ID), &c.CreatedAt, &c.UpdatedAt, &rawData); scanErr != nil {
				rows.Close()
				return nil, fmt.Errorf("scan related batch row: %w", scanErr)
			}
			if len(rawData.raw) > 0 {
				if uErr := json.Unmarshal(rawData.raw, &c.Data); uErr != nil {
					rows.Close()
					return nil, fmt.Errorf("unmarshal related batch data: %w", uErr)
				}
			}
			if c.Data == nil {
				c.Data = make(map[string]any)
			}
			normalizeContentData(s.pool.Engine(), relSchema, c.Data)
			if err := w.take(c, len(rawData.raw)); err != nil {
				rows.Close()
				return nil, err
			}
			result[sourceID] = append(result[sourceID], c)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}

	return result, nil
}

// PopulateWithConfig resolves related records for the given content item according
// to cfg, supporting nested paths (dot notation), wildcards (*), and depth-based
// auto-population. Fields are populated in-place on c.Data.
//
// This is a convenience wrapper around the batch variant for single-item callers.
// For multiple items, prefer PopulateWithConfigBatch to avoid N+1 queries.
func (s *ContentStore) PopulateWithConfig(ctx context.Context, c *domain.Content, cfg domain.PopulateConfig) error {
	return s.PopulateWithConfigBatch(ctx, []*domain.Content{c}, cfg)
}

// PopulateWithConfigBatch resolves related records for multiple content items
// using batched queries. Instead of fetching each relation field per-item
// (N+1 pattern), it collects all foreign-key IDs at each recursion level and
// resolves them with a single IN (...) query per relation field.
//
// All items must share the same schema. The first item's SchemaName is used
// for the schema lookup. Items with different schema names will cause
// incorrect results or errors.
func (s *ContentStore) PopulateWithConfigBatch(ctx context.Context, items []*domain.Content, cfg domain.PopulateConfig) error {
	return s.PopulateGated(ctx, items, cfg, nil)
}

// RelationGate decides, per relation field, whether a populated relation may
// be served and what of each related record may be seen. It is handed the
// field, so it can judge the relation's kind as well as its target schema.
// ok false leaves the relation unpopulated. mask, when not nil, is applied to
// every related record before it is written into the parent, and a nil from
// it hides that record: a to-one relation is served as null and a to-many one
// leaves it out. hides, when not nil, reports whether the caller's field mask
// hides a column of the target, which a has_one or has_many must not read its
// key from, and a relation field of the target, which the walk does not read
// below. drafts reports whether the caller may read drafts of the target.
// When it is false, a has_one or has_many reads only the published entries,
// so a has_one serves the newest entry the caller may read rather than null
// behind a newer draft.
type RelationGate func(ctx context.Context, f domain.SchemaField) (mask func(map[string]any) map[string]any, hides func(column string) bool, drafts bool, ok bool)

// PopulateGated is PopulateWithConfigBatch with every relation, at every
// depth, put to gate before its target schema is read. A nil gate serves
// every relation whole, which is the ungated behavior. A relation the
// database fails to read is logged and served empty. A walk past its bounds
// is ErrPopulateTooLarge, and the items are then partly populated.
//
// The gate runs inside the recursion because ?depth= and the wildcard reach
// relations no request names, so a check of the requested paths alone would
// miss the schemas the walk discovers on its own.
func (s *ContentStore) PopulateGated(ctx context.Context, items []*domain.Content, cfg domain.PopulateConfig, gate RelationGate) error {
	return s.populateWalk(ctx, items, cfg, nil, gate, false)
}

// PopulateStrict is PopulateGated with a relation the database fails to read
// returned as an error rather than served empty, for a caller that has to
// refuse an answer it cannot complete. hidden, when not nil, reports whether
// the caller's field mask hides a field of the items' own schema, and a
// relation field it hides is not read.
func (s *ContentStore) PopulateStrict(ctx context.Context, items []*domain.Content, cfg domain.PopulateConfig, hidden func(field string) bool, gate RelationGate) error {
	return s.populateWalk(ctx, items, cfg, hidden, gate, true)
}

// populateWalk starts one walk over items. The bounds hold the walk as a
// whole, so the served count is checked again at the top, where every copy
// of an entry the answer carries has been counted.
func (s *ContentStore) populateWalk(ctx context.Context, items []*domain.Content, cfg domain.PopulateConfig, hidden func(string) bool, gate RelationGate, strict bool) error {
	if cfg.IsEmpty() || len(items) == 0 {
		return nil
	}
	w := &populateWalk{gate: gate, strict: strict, sizes: map[*domain.Content]int{}}
	_, err := s.populateRecursiveBatch(ctx, w, items, cfg, hidden, 1)
	return err
}

// populateRecursiveBatch is the internal recursive batch implementation.
// currentDepth tracks recursion depth (1-indexed) to enforce MaxPopulateDepth.
// At each level, it batches FK resolution across all items before recursing
// into the fetched related records. It returns what each item now carries:
// the related entries served under it at every level below, and their bytes.
// hidden reports the fields of the items' schema the caller's mask hides.
func (s *ContentStore) populateRecursiveBatch(ctx context.Context, w *populateWalk, items []*domain.Content, cfg domain.PopulateConfig, hidden func(string) bool, currentDepth int) ([]populateLoad, error) {
	carried := make([]populateLoad, len(items))
	if cfg.IsEmpty() || currentDepth > MaxPopulateDepth || len(items) == 0 {
		return carried, nil
	}

	// All items must share the same schema (enforced by callers).
	schemaName := items[0].SchemaName
	sc, err := s.schemas.GetByName(ctx, schemaName)
	if err != nil {
		return nil, fmt.Errorf("bulk populate %s: %w", schemaName, err)
	}

	// Build field lookup map and relation field list once for all items.
	fieldsByName := make(map[string]domain.SchemaField, len(sc.Fields))
	for _, f := range sc.Fields {
		fieldsByName[f.Name] = f
	}

	allRelFields := domain.RelationFields(sc.Fields)
	if len(allRelFields) == 0 {
		return carried, nil // no relations to populate
	}

	// Determine which fields to populate at this level.
	populateFields, nestedPaths, fieldMasks := populateRootFields(cfg, allRelFields)

	// Calculate next depth for wildcard/depth-based recursion.
	nextDepth := 0
	explicitDepthActive := cfg.MaxDepth > 0
	wildcardActive := false
	for _, p := range cfg.Paths {
		if p == "*" || strings.HasSuffix(p, ".*") {
			wildcardActive = true
			break
		}
	}
	switch {
	case explicitDepthActive:
		nextDepth = cfg.MaxDepth - 1
	case wildcardActive:
		nextDepth = 0
	}
	if nextDepth < 0 {
		nextDepth = 0
	}

	named := namedFields(cfg)
	var total populateLoad
	for _, fieldName := range populateFields {
		f, ok := fieldsByName[fieldName]
		if !ok || f.FieldType != "relation" || f.RelationTo == "" {
			continue
		}
		// The mask strips a hidden relation field from the entry that holds
		// it, so what lies below it is never served, and counting it toward
		// the bounds would let a refusal say how much it holds.
		if hidden != nil && hidden(fieldName) {
			continue
		}

		var relMask func(map[string]any) map[string]any
		var hides func(string) bool
		drafts := true
		if w.gate != nil {
			m, h, d, ok := w.gate(ctx, f)
			if !ok {
				continue
			}
			relMask, hides, drafts = m, h, d
		}
		rel := relationPopulate{field: f, name: fieldName, masks: fieldMasks, relMask: relMask, hides: hides, drafts: drafts, walk: w, named: named[fieldName]}

		nextCfg := nextPopulateConfig(fieldName, nestedPaths, nextDepth)

		var loads []populateLoad
		var err error
		switch f.RelationType {
		case domain.RelBelongsTo, "":
			loads, err = s.populateBelongsToBatch(ctx, items, rel, nextCfg, currentDepth)

		case domain.RelHasOne:
			loads, err = s.populateHasOneBatch(ctx, items, rel, nextCfg, currentDepth)

		case domain.RelHasMany:
			loads, err = s.populateHasManyBatch(ctx, items, rel, nextCfg, currentDepth)

		case domain.RelManyToMany:
			loads, err = s.populateManyToManyBatch(ctx, items, rel, nextCfg, currentDepth)
		}
		if err != nil {
			return nil, err
		}
		for i, l := range loads {
			carried[i] = carried[i].plus(l)
			total = total.plus(l)
		}
		if err := total.within(); err != nil {
			return nil, err
		}
	}
	return carried, nil
}

// relationPopulate is one relation field being populated at one level: the
// field, the name it is written under, the projection the request asked for,
// the gate's mask for the target schema, the columns it hides there and
// whether drafts there count, the walk it belongs to, and whether the request
// names it rather than reaching it by a wildcard or a depth.
type relationPopulate struct {
	field   domain.SchemaField
	name    string
	masks   map[string][]string
	relMask func(map[string]any) map[string]any
	hides   func(column string) bool
	drafts  bool
	walk    *populateWalk
	named   bool
}

// readFailed answers a relation read that failed: the error when the walk is
// strict or refused the request, and nil once it is logged otherwise, so the
// caller serves the relation empty.
func (r relationPopulate) readFailed(ctx context.Context, kind string, err error) error {
	if r.walk.strict || errors.Is(err, domain.ErrBadRequest) {
		return fmt.Errorf("populate %s %s: %w", kind, r.name, err)
	}
	// Logged because the caller sees a 200 either way and cannot tell a
	// relation that is absent from one the database failed to read.
	slog.ErrorContext(ctx, "content: relation populate failed, field served empty",
		"field", r.name, "relation", r.field.RelationTo, "kind", kind, "err", err)
	return nil
}

// recurse populates the records one relation fetched, one level down, and
// returns what each of them carries from there. A record several parents
// share is walked once. A record the gate's mask hides is not walked, and
// neither is a relation field the mask hides, so what lies below either
// neither costs reads nor counts toward the bounds, where a refusal would
// tell the caller how much it holds. A failure there is returned only when the walk is strict or
// refused the request.
func (s *ContentStore) recurse(ctx context.Context, rel relationPopulate, fetched []*domain.Content, nextCfg domain.PopulateConfig, currentDepth int) (map[*domain.Content]populateLoad, error) {
	seen := make(map[*domain.Content]bool, len(fetched))
	distinct := make([]*domain.Content, 0, len(fetched))
	for _, c := range fetched {
		if seen[c] {
			continue
		}
		seen[c] = true
		if rel.relMask != nil && rel.relMask(flattenContent(c)) == nil {
			continue
		}
		distinct = append(distinct, c)
	}
	loads, err := s.populateRecursiveBatch(ctx, rel.walk, distinct, nextCfg, rel.hides, currentDepth+1)
	if err != nil {
		if rel.walk.strict || errors.Is(err, domain.ErrBadRequest) {
			return nil, err
		}
		return nil, nil
	}
	below := make(map[*domain.Content]populateLoad, len(distinct))
	for i, c := range distinct {
		below[c] = loads[i]
	}
	return below, nil
}

// mask applies the gate's mask, then the requested projection, to one
// flattened related record. The gate sees the whole record, so it can judge
// a field the projection would drop, such as the status. A nil from the gate
// hides the record, and the caller serves the relation as absent.
func (r relationPopulate) mask(flat map[string]any) map[string]any {
	if r.relMask != nil {
		flat = r.relMask(flat)
		if flat == nil {
			return nil
		}
	}
	if m := r.masks[r.name]; len(m) > 0 {
		flat = applyFieldMask(flat, m)
	}
	return flat
}

// populateBelongsToBatch batch-resolves belongs_to relations across all items.
func (s *ContentStore) populateBelongsToBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) ([]populateLoad, error) {
	f, fieldName := rel.field, rel.name
	fkCol := domain.FKColumn(f.Name, f.RelationFKName)
	loads := make([]populateLoad, len(items))

	// Collect all unique FK IDs across items.
	// Map: item index -> FK ID (each item has at most one belongs_to FK).
	itemFKs := make(map[int]uuid.UUID, len(items))
	fkSet := make(map[uuid.UUID]bool)
	for i, item := range items {
		rawID := item.Data[fkCol]
		idStr, _ := rawID.(string)
		if idStr == "" {
			item.Data[fieldName] = nil
			continue
		}
		relID, err := uuid.Parse(idStr)
		if err != nil {
			item.Data[fieldName] = nil
			continue
		}
		itemFKs[i] = relID
		fkSet[relID] = true
	}

	if len(fkSet) == 0 {
		return loads, nil
	}

	// Batch fetch all related records.
	fkList := make([]uuid.UUID, 0, len(fkSet))
	for id := range fkSet {
		fkList = append(fkList, id)
	}
	relatedMap, err := s.getByIDsCounted(ctx, rel.walk, f.RelationTo, fkList)
	if err != nil {
		if err := rel.readFailed(ctx, "belongs_to", err); err != nil {
			return nil, err
		}
		for i := range itemFKs {
			items[i].Data[fieldName] = nil
		}
		return loads, nil
	}

	// Collect fetched records for recursive population.
	var fetched []*domain.Content
	for i, relID := range itemFKs {
		related, ok := relatedMap[relID]
		if !ok {
			items[i].Data[fieldName] = nil
			continue
		}
		fetched = append(fetched, related)
	}

	// Recurse into fetched records first.
	below, err := s.recurse(ctx, rel, fetched, nextCfg, currentDepth)
	if err != nil {
		return nil, err
	}

	// Assign results back to items.
	for i, relID := range itemFKs {
		related, ok := relatedMap[relID]
		if !ok {
			items[i].Data[fieldName] = nil
			continue
		}
		if flat := rel.mask(flattenContent(related)); flat != nil {
			items[i].Data[fieldName] = flat
			loads[i] = rel.walk.served(related, below)
		} else {
			items[i].Data[fieldName] = nil
		}
	}
	return loads, nil
}

// getByIDsCounted is GetByIDs with each row counted against the walk as it
// is read, so a read past the bound stops at the row that passes it rather
// than after every row is in memory. A belongs_to reads at most one row for
// each id, so an id list longer than the entries the walk may still read is
// refused before any query.
func (s *ContentStore) getByIDsCounted(ctx context.Context, w *populateWalk, schemaName string, ids []uuid.UUID) (map[uuid.UUID]*domain.Content, error) {
	if len(ids) > maxPopulateEntries-w.read.entries {
		return nil, ErrPopulateTooLarge
	}
	for range (len(ids) + maxBatchSize - 1) / maxBatchSize {
		if err := w.query(); err != nil {
			return nil, err
		}
	}
	return s.getByIDs(ctx, schemaName, ids, w.take)
}

// itemIDs is the id of each item.
func itemIDs(items []*domain.Content) []uuid.UUID {
	ids := make([]uuid.UUID, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

// populateHasOneBatch batch-resolves has_one relations across all items.
func (s *ContentStore) populateHasOneBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) ([]populateLoad, error) {
	f, fieldName := rel.field, rel.name
	loads := make([]populateLoad, len(items))

	fkCol, err := s.inverseKeyOf(ctx, rel, items[0].SchemaName)
	if err == nil && fkCol == "" {
		return loads, nil
	}
	var relatedMap map[uuid.UUID][]*domain.Content
	if err == nil {
		relatedMap, err = s.GetByFKs(ctx, rel.walk, f.RelationTo, fkCol, itemIDs(items), rel.drafts)
	}
	if err != nil {
		if err := rel.readFailed(ctx, "has_one", err); err != nil {
			return nil, err
		}
		for _, item := range items {
			item.Data[fieldName] = nil
		}
		return loads, nil
	}

	// Collect fetched records for recursion.
	var fetched []*domain.Content

	for _, item := range items {
		children, ok := relatedMap[item.ID]
		if !ok || len(children) == 0 {
			item.Data[fieldName] = nil
			continue
		}
		// has_one: take the first matching record.
		fetched = append(fetched, children[0])
	}

	// Recurse into fetched records.
	below, err := s.recurse(ctx, rel, fetched, nextCfg, currentDepth)
	if err != nil {
		return nil, err
	}

	// Assign results back.
	for i, item := range items {
		children, ok := relatedMap[item.ID]
		if !ok || len(children) == 0 {
			item.Data[fieldName] = nil
			continue
		}
		if flat := rel.mask(flattenContent(children[0])); flat != nil {
			item.Data[fieldName] = flat
			loads[i] = rel.walk.served(children[0], below)
		} else {
			item.Data[fieldName] = nil
		}
	}
	return loads, nil
}

// populateHasManyBatch batch-resolves has_many relations across all items.
func (s *ContentStore) populateHasManyBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) ([]populateLoad, error) {
	fkCol, err := s.inverseKeyOf(ctx, rel, items[0].SchemaName)
	if err == nil && fkCol == "" {
		return make([]populateLoad, len(items)), nil
	}
	var relatedMap map[uuid.UUID][]*domain.Content
	if err == nil {
		relatedMap, err = s.GetByFKs(ctx, rel.walk, rel.field.RelationTo, fkCol, itemIDs(items), rel.drafts)
	}
	return s.assignToMany(ctx, items, rel, nextCfg, currentDepth, "has_many", relatedMap, err)
}

// inverseKeyOf is the key column the has_one or has_many rel of the type
// source reads on its target, by core.InverseKey's rule. Empty with no error is a
// relation left out of every item:
//
//   - one whose key the caller's mask hides on the target, named or not. The
//     entries grouped under each item would show which item each points at,
//     and a refusal would say what the mask hides.
//   - one the definition gives no key, reached by a wildcard or a depth,
//     since that request asked for whatever can be read. Named, it is an
//     InverseKeyError.
func (s *ContentStore) inverseKeyOf(ctx context.Context, rel relationPopulate, source string) (string, error) {
	target, err := s.schemas.GetByName(ctx, rel.field.RelationTo)
	if err != nil {
		return "", fmt.Errorf("resolve schema %q: %w", rel.field.RelationTo, err)
	}
	key, ok := core.InverseKey(source, rel.field, target)
	switch {
	case !ok && rel.named:
		return "", &InverseKeyError{Field: rel.name, Target: rel.field.RelationTo}
	case !ok, rel.hides != nil && rel.hides(key):
		return "", nil
	}
	return key, nil
}

// populateManyToManyBatch batch-resolves many_to_many relations across all items.
func (s *ContentStore) populateManyToManyBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) ([]populateLoad, error) {
	relatedMap, err := s.ListRelatedBatch(ctx, rel.walk, items[0].SchemaName, itemIDs(items), rel.field)
	return s.assignToMany(ctx, items, rel, nextCfg, currentDepth, "many_to_many", relatedMap, err)
}

// assignToMany serves a to-many relation read under kind: it populates the
// related records one level down, then writes each item's list, and returns
// what each item carries. An entry two items share counts under each.
func (s *ContentStore) assignToMany(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int, kind string, relatedMap map[uuid.UUID][]*domain.Content, readErr error) ([]populateLoad, error) {
	fieldName := rel.name
	loads := make([]populateLoad, len(items))
	if readErr != nil {
		if err := rel.readFailed(ctx, kind, readErr); err != nil {
			return nil, err
		}
		for _, item := range items {
			item.Data[fieldName] = []map[string]any{}
		}
		return loads, nil
	}

	// Collect all fetched records for recursive population.
	var fetched []*domain.Content
	for _, item := range items {
		children, ok := relatedMap[item.ID]
		if !ok {
			item.Data[fieldName] = []map[string]any{}
			continue
		}
		fetched = append(fetched, children...)
	}

	// Recurse into all fetched children.
	below, err := s.recurse(ctx, rel, fetched, nextCfg, currentDepth)
	if err != nil {
		return nil, err
	}

	// Assign results back.
	for i, item := range items {
		children, ok := relatedMap[item.ID]
		if !ok {
			item.Data[fieldName] = []map[string]any{}
			continue
		}
		out := make([]map[string]any, 0, len(children))
		for _, r := range children {
			if flat := rel.mask(flattenContent(r)); flat != nil {
				out = append(out, flat)
				loads[i] = loads[i].plus(rel.walk.served(r, below))
			}
		}
		item.Data[fieldName] = out
	}
	return loads, nil
}

// ListRelated returns items from the target schema related to id via a
// many_to_many pivot table. A soft-deleted target is never listed, and an
// unpublished one only when drafts is true. The total counts what the same
// filter would serve, so a page count never reveals a hidden row. Targets are
// ordered as List orders them, with the id breaking created_at ties, so an
// offset page never repeats or skips a target.
func (s *ContentStore) ListRelated(ctx context.Context, schemaName string, id uuid.UUID, f domain.SchemaField, limit, offset int, drafts bool) ([]*domain.Content, int, error) {
	pivotName, err := s.pivotTable(ctx, schemaName, f)
	if err != nil {
		return nil, 0, fmt.Errorf("list related: %w", err)
	}
	if err := s.ensureTable(ctx, pivotName); err != nil {
		return nil, 0, fmt.Errorf("list related: ensure pivot tenant column: %w", err)
	}
	targetTable := s.qualify(domain.TableName(f.RelationTo))
	aCol := strings.TrimPrefix(domain.TableName(schemaName), "_") + "_id"
	bCol := strings.TrimPrefix(domain.TableName(f.RelationTo), "_") + "_id"
	pivotName = s.qualify(pivotName)
	tenantID := tenant.ID(ctx)

	// Resolve target schema for data column expression.
	relSchema, relErr := s.schemas.GetByName(ctx, f.RelationTo)
	if relErr != nil {
		return nil, 0, fmt.Errorf("list related: resolve schema %q: %w", f.RelationTo, relErr)
	}
	visible := draftWhere(relSchema, "t", drafts)
	if relSchema.WithSoftDelete {
		visible += " AND t.deleted_at IS NULL"
	}

	// Total count, scoped to the caller's tenant.
	var total int
	row, qErr := s.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s t JOIN %s p ON p.%s = t.id
			WHERE p.%s = $1 AND p.tenant_id = $2 AND t.tenant_id = $3%s`,
			targetTable, pivotName, bCol, aCol, visible), id, tenantID, tenantID)
	if qErr != nil {
		slog.Warn("content: relation count query failed, pagination total defaults to 0", "table", pivotName, "err", qErr)
	} else if err := row.Scan(&total); err != nil {
		slog.Warn("content: relation count query failed, pagination total defaults to 0", "table", pivotName, "err", err)
	}

	// Dialect-aware pagination ($4=limit, $5=offset; $1 is the relation id,
	// $2/$3 the tenant, one per side of the join).
	relPagination := dialect.LimitOffsetPlaceholders(dialect.Must(s.pool.Engine()), 4, 5)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT t.id, t.created_at, t.updated_at,
		       %s
		FROM %s t
		JOIN %s p ON p.%s = t.id
		WHERE p.%s = $1 AND p.tenant_id = $2 AND t.tenant_id = $3%s
		ORDER BY t.created_at DESC, t.id DESC
		%s`,
		dataColumnExpr(s.pool.Engine(), relSchema, "t"),
		targetTable, pivotName, bCol, aCol, visible, relPagination,
	), id, tenantID, tenantID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list related %s->%s: %w", schemaName, f.RelationTo, err)
	}
	defer rows.Close()

	items := make([]*domain.Content, 0)
	for rows.Next() {
		c, err := scanFullRow(s.pool.Engine(), rows, f.RelationTo, relSchema)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, c)
	}
	return items, total, rows.Err()
}

// SetRelations replaces all many_to_many pivot rows for (schemaName, id, field)
// with the provided targetIDs, scoped to the calling tenant. Runs in a
// transaction.
//
// Both ends are held to the tenant, because the pivot's keys are not: a
// source row the tenant does not hold is domain.ErrNotFound, and a target it
// does not hold refuses the whole set as a validation error naming the field.
// Another tenant's row is refused exactly as a row nobody holds.
func (s *ContentStore) SetRelations(ctx context.Context, schemaName string, id uuid.UUID, f domain.SchemaField, targetIDs []uuid.UUID) error {
	pivotName, err := s.pivotTable(ctx, schemaName, f)
	if err != nil {
		return fmt.Errorf("set relations: %w", err)
	}
	if err := s.ensureTable(ctx, pivotName); err != nil {
		return fmt.Errorf("set relations: ensure pivot tenant column: %w", err)
	}
	pivotName = s.qualify(pivotName)
	aCol := strings.TrimPrefix(domain.TableName(schemaName), "_") + "_id"
	bCol := strings.TrimPrefix(domain.TableName(f.RelationTo), "_") + "_id"

	d := dialect.Must(s.pool.Engine())
	tenantID := tenant.ID(ctx)

	source := []core.RelationValue{{Field: f.Name, Target: schemaName, Value: id}}
	targets := make([]core.RelationValue, 0, len(targetIDs))
	for _, tid := range targetIDs {
		targets = append(targets, core.RelationValue{Field: f.Name, Target: f.RelationTo, Value: tid})
	}
	if err := s.ensureTargetTables(ctx, append(source, targets...)); err != nil {
		return err
	}
	return WriteInTransaction(ctx, s.pool, func(ctx context.Context) error {
		if err := s.checkRelationTargets(ctx, source); err != nil {
			if errors.Is(err, domain.ErrValidation) {
				return domain.ErrNotFound
			}
			return err
		}
		if err := s.checkRelationTargets(ctx, targets); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE %s = $1 AND tenant_id = $2`, pivotName, aCol), id, tenantID); err != nil {
			return fmt.Errorf("set relations delete: %w", err)
		}
		insert := dialect.InsertDoNothing(d, pivotName, []string{TenantColumnName, aCol, bCol}, []string{aCol, bCol})
		for _, tid := range targetIDs {
			if _, err := s.pool.Exec(ctx, insert, tenantID, id, tid); err != nil {
				return fmt.Errorf("set relations insert: %w", err)
			}
		}
		return nil
	})
}

// flattenContent merges Content.Data + id/created_at/updated_at into one map.
func flattenContent(c *domain.Content) map[string]any {
	out := make(map[string]any, len(c.Data)+3)
	for k, v := range c.Data {
		out[k] = v
	}
	out["id"] = c.ID
	out["created_at"] = c.CreatedAt
	out["updated_at"] = c.UpdatedAt
	return out
}
