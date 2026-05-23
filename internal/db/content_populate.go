package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
)

// Populate resolves relation fields listed in populateFields and attaches
// the related records directly inside content.Data[fieldName].
// For belongs_to / has_one -> the related object (or nil).
// For has_many / many_to_many -> []map[string]any.
func (s *ContentStore) Populate(ctx context.Context, c *domain.Content, populateFields []string) error {
	sc, err := s.schemas.GetByName(ctx, c.SchemaName)
	if err != nil {
		return fmt.Errorf("populate %s: %w", c.SchemaName, err)
	}
	fieldsByName := make(map[string]domain.SchemaField, len(sc.Fields))
	for _, f := range sc.Fields {
		fieldsByName[f.Name] = f
	}

	for _, fieldName := range populateFields {
		f, ok := fieldsByName[fieldName]
		if !ok || f.FieldType != "relation" || f.RelationTo == "" {
			continue
		}

		switch f.RelationType {
		case domain.RelBelongsTo, "":
			fkCol := domain.FKColumn(f.Name, f.RelationFKName)
			rawID := c.Data[fkCol]
			idStr, _ := rawID.(string)
			if idStr == "" {
				c.Data[fieldName] = nil
				continue
			}
			relID, err := uuid.Parse(idStr)
			if err != nil {
				c.Data[fieldName] = nil
				continue
			}
			related, err := s.GetByID(ctx, f.RelationTo, relID)
			if err != nil {
				c.Data[fieldName] = nil
				continue
			}
			c.Data[fieldName] = flattenContent(related)

		case domain.RelHasOne:
			// FK is on the other table: {schemaName}_id -> this record's id
			fkCol := domain.FKColumn(c.SchemaName, "")
			results, err := s.List(ctx, f.RelationTo, 1, 0, map[string]any{fkCol: c.ID.String()})
			if err != nil || len(results) == 0 {
				c.Data[fieldName] = nil
				continue
			}
			c.Data[fieldName] = flattenContent(results[0])

		case domain.RelHasMany:
			fkCol := domain.FKColumn(c.SchemaName, "")
			results, err := s.List(ctx, f.RelationTo, 500, 0, map[string]any{fkCol: c.ID.String()})
			if err != nil {
				c.Data[fieldName] = []map[string]any{}
				continue
			}
			out := make([]map[string]any, 0, len(results))
			for _, r := range results {
				out = append(out, flattenContent(r))
			}
			c.Data[fieldName] = out

		case domain.RelManyToMany:
			related, _, err := s.ListRelated(ctx, c.SchemaName, c.ID, f, 500, 0)
			if err != nil {
				c.Data[fieldName] = []map[string]any{}
				continue
			}
			out := make([]map[string]any, 0, len(related))
			for _, r := range related {
				out = append(out, flattenContent(r))
			}
			c.Data[fieldName] = out
		}
	}
	return nil
}

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

// maxPopulateDepth guards against infinite recursion (e.g., circular relations).
const maxPopulateDepth = 10

// maxBatchSize limits the number of IDs in a single IN (...) clause to avoid
// excessively large queries. If a batch exceeds this, it is split into chunks.
// 500 is chosen as a safe upper bound that works across all three dialects
// without hitting parameter limits (PG: 65535, MySQL: 65535, MSSQL: 2100).
const maxBatchSize = 500

// GetByIDs returns multiple content rows by their UUID primary keys in a single
// query. Items found in the item cache are returned directly. Only uncached IDs
// are fetched from the database. Results are written back to the item cache.
// Returns a map keyed by UUID for O(1) lookup. IDs that are not found are
// silently omitted from the result map (same behavior as GetByID returning
// ErrNotFound for a missing ID: the caller assigns nil to that field).
func (s *ContentStore) GetByIDs(ctx context.Context, schemaName string, ids []uuid.UUID) (map[uuid.UUID]*domain.Content, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]*domain.Content{}, nil
	}

	result := make(map[uuid.UUID]*domain.Content, len(ids))
	var uncached []uuid.UUID

	// Check item cache first.
	tenantID := tenant.ID(ctx)
	for _, id := range ids {
		if s.itemCache != nil {
			key := cacheItemKey(tenantID, schemaName, id)
			if cached, ok := s.itemCache.Get(ctx, key); ok {
				result[id] = cached
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
			c, scanErr := scanFullRow(s.pool.Engine(), rows, schemaName, sc)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			result[c.ID] = c
			// Write back to item cache.
			if s.itemCache != nil {
				_ = s.itemCache.Set(ctx, cacheItemKey(tenantID, schemaName, c.ID), c, 0)
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
// Used for batch resolution of has_one and has_many relations.
func (s *ContentStore) GetByFKs(ctx context.Context, schemaName string, fkCol string, parentIDs []uuid.UUID) (map[uuid.UUID][]*domain.Content, error) {
	if len(parentIDs) == 0 {
		return map[uuid.UUID][]*domain.Content{}, nil
	}

	sc, table, err := s.table(ctx, schemaName)
	if err != nil {
		return nil, fmt.Errorf("content get by fks: resolve table: %w", err)
	}

	result := make(map[uuid.UUID][]*domain.Content, len(parentIDs))
	dataExpr := dataColumnExpr(s.pool.Engine(), sc, "r")
	fkColQuoted := dialect.Must(s.pool.Engine()).QuoteIdentifier(fkCol)
	tenantID := tenant.ID(ctx)

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

		query := fmt.Sprintf(`
			SELECT id, created_at, updated_at,
			       %s
			FROM %s r
			WHERE %s IN (%s) AND tenant_id = $%d
			ORDER BY created_at DESC`,
			dataExpr, table, fkColQuoted, strings.Join(phs, ", "), len(chunk)+1)

		rows, qErr := s.pool.Query(ctx, query, args...)
		if qErr != nil {
			return nil, fmt.Errorf("content get by fks %s: %w", table, qErr)
		}

		for rows.Next() {
			c, scanErr := scanFullRow(s.pool.Engine(), rows, schemaName, sc)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
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
// by source ID.
func (s *ContentStore) ListRelatedBatch(ctx context.Context, schemaName string, sourceIDs []uuid.UUID, f domain.SchemaField) (map[uuid.UUID][]*domain.Content, error) {
	if len(sourceIDs) == 0 {
		return map[uuid.UUID][]*domain.Content{}, nil
	}

	pivotName := f.RelationThrough
	if pivotName == "" {
		pivotName = domain.PivotTableName(schemaName, f.RelationTo)
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

		rows, qErr := s.pool.Query(ctx, fmt.Sprintf(`
			SELECT p.%s AS source_id, t.id, t.created_at, t.updated_at,
			       %s
			FROM %s t
			JOIN %s p ON p.%s = t.id
			WHERE p.%s IN (%s) AND p.tenant_id = $%d AND t.tenant_id = $%d
			ORDER BY t.created_at DESC`,
			aCol,
			dataColumnExpr(s.pool.Engine(), relSchema, "t"),
			targetTable, pivotName, bCol, aCol,
			strings.Join(phs, ", "), len(chunk)+1, len(chunk)+2,
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

// RelationGate decides, per target schema, whether a populated relation may
// be served and what of each related record may be seen. ok false leaves the
// relation unpopulated. mask, when not nil, is applied to every related
// record before it is written into the parent.
type RelationGate func(ctx context.Context, target string) (mask func(map[string]any) map[string]any, ok bool)

// PopulateGated is PopulateWithConfigBatch with every relation, at every
// depth, put to gate before its target schema is read. A nil gate serves
// every relation whole, which is the ungated behavior.
//
// The gate runs inside the recursion because ?depth= and the wildcard reach
// relations no request names, so a check of the requested paths alone would
// miss the schemas the walk discovers on its own.
func (s *ContentStore) PopulateGated(ctx context.Context, items []*domain.Content, cfg domain.PopulateConfig, gate RelationGate) error {
	if cfg.IsEmpty() || len(items) == 0 {
		return nil
	}
	return s.populateRecursiveBatch(ctx, items, cfg, 1, gate)
}

// populateRecursiveBatch is the internal recursive batch implementation.
// currentDepth tracks recursion depth (1-indexed) to enforce maxPopulateDepth.
// At each level, it batches FK resolution across all items before recursing
// into the fetched related records.
func (s *ContentStore) populateRecursiveBatch(ctx context.Context, items []*domain.Content, cfg domain.PopulateConfig, currentDepth int, gate RelationGate) error {
	if cfg.IsEmpty() || currentDepth > maxPopulateDepth || len(items) == 0 {
		return nil
	}

	// All items must share the same schema (enforced by callers).
	schemaName := items[0].SchemaName
	sc, err := s.schemas.GetByName(ctx, schemaName)
	if err != nil {
		return fmt.Errorf("bulk populate %s: %w", schemaName, err)
	}

	// Build field lookup map and relation field list once for all items.
	fieldsByName := make(map[string]domain.SchemaField, len(sc.Fields))
	for _, f := range sc.Fields {
		fieldsByName[f.Name] = f
	}

	allRelFields := domain.RelationFields(sc.Fields)
	if len(allRelFields) == 0 {
		return nil // no relations to populate
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

	for _, fieldName := range populateFields {
		f, ok := fieldsByName[fieldName]
		if !ok || f.FieldType != "relation" || f.RelationTo == "" {
			continue
		}

		var relMask func(map[string]any) map[string]any
		if gate != nil {
			m, ok := gate(ctx, f.RelationTo)
			if !ok {
				continue
			}
			relMask = m
		}
		rel := relationPopulate{field: f, name: fieldName, masks: fieldMasks, relMask: relMask, gate: gate}

		nextCfg := nextPopulateConfig(fieldName, nestedPaths, nextDepth)

		switch f.RelationType {
		case domain.RelBelongsTo, "":
			s.populateBelongsToBatch(ctx, items, rel, nextCfg, currentDepth)

		case domain.RelHasOne:
			s.populateHasOneBatch(ctx, items, rel, nextCfg, currentDepth)

		case domain.RelHasMany:
			s.populateHasManyBatch(ctx, items, rel, nextCfg, currentDepth)

		case domain.RelManyToMany:
			s.populateManyToManyBatch(ctx, items, rel, nextCfg, currentDepth)
		}
	}
	return nil
}

// relationPopulate is one relation field being populated at one level: the
// field, the name it is written under, the projection the request asked for
// and the gate's mask for the target schema.
type relationPopulate struct {
	field   domain.SchemaField
	name    string
	masks   map[string][]string
	relMask func(map[string]any) map[string]any
	gate    RelationGate
}

// mask applies the requested projection, then the gate's mask, to one
// flattened related record. The projection can only narrow, so the gate's
// mask has the last word on what is served.
func (r relationPopulate) mask(flat map[string]any) map[string]any {
	if m := r.masks[r.name]; len(m) > 0 {
		flat = applyFieldMask(flat, m)
	}
	if r.relMask != nil {
		flat = r.relMask(flat)
	}
	return flat
}

// populateBelongsToBatch batch-resolves belongs_to relations across all items.
func (s *ContentStore) populateBelongsToBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) {
	f, fieldName := rel.field, rel.name
	fkCol := domain.FKColumn(f.Name, f.RelationFKName)

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
		return
	}

	// Batch fetch all related records.
	fkList := make([]uuid.UUID, 0, len(fkSet))
	for id := range fkSet {
		fkList = append(fkList, id)
	}
	relatedMap, err := s.GetByIDs(ctx, f.RelationTo, fkList)
	if err != nil {
		// Best-effort: the response still goes out, with this relation empty.
		// Logged because the caller sees a 200 either way and cannot tell a
		// relation that is absent from one the database failed to read.
		slog.ErrorContext(ctx, "content: relation populate failed, field served empty",
			"field", fieldName, "relation", f.RelationTo, "kind", "belongs_to", "err", err)
		for i := range itemFKs {
			items[i].Data[fieldName] = nil
		}
		return
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
	_ = s.populateRecursiveBatch(ctx, fetched, nextCfg, currentDepth+1, rel.gate)

	// Assign results back to items.
	for i, relID := range itemFKs {
		related, ok := relatedMap[relID]
		if !ok {
			items[i].Data[fieldName] = nil
			continue
		}
		flat := flattenContent(related)
		flat = rel.mask(flat)
		items[i].Data[fieldName] = flat
	}
}

// populateHasOneBatch batch-resolves has_one relations across all items.
func (s *ContentStore) populateHasOneBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) {
	f, fieldName := rel.field, rel.name
	fkCol := domain.FKColumn(items[0].SchemaName, "") // FK column on the target table pointing back to source.

	// Collect all source IDs.
	parentIDs := make([]uuid.UUID, len(items))
	for i, item := range items {
		parentIDs[i] = item.ID
	}

	relatedMap, err := s.GetByFKs(ctx, f.RelationTo, fkCol, parentIDs)
	if err != nil {
		slog.ErrorContext(ctx, "content: relation populate failed, field served empty",
			"field", fieldName, "relation", f.RelationTo, "kind", "has_one", "err", err)
		for _, item := range items {
			item.Data[fieldName] = nil
		}
		return
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
	_ = s.populateRecursiveBatch(ctx, fetched, nextCfg, currentDepth+1, rel.gate)

	// Assign results back.
	for _, item := range items {
		children, ok := relatedMap[item.ID]
		if !ok || len(children) == 0 {
			item.Data[fieldName] = nil
			continue
		}
		flat := flattenContent(children[0])
		flat = rel.mask(flat)
		item.Data[fieldName] = flat
	}
}

// populateHasManyBatch batch-resolves has_many relations across all items.
func (s *ContentStore) populateHasManyBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) {
	f, fieldName := rel.field, rel.name
	fkCol := domain.FKColumn(items[0].SchemaName, "")

	parentIDs := make([]uuid.UUID, len(items))
	for i, item := range items {
		parentIDs[i] = item.ID
	}

	relatedMap, err := s.GetByFKs(ctx, f.RelationTo, fkCol, parentIDs)
	if err != nil {
		slog.ErrorContext(ctx, "content: relation populate failed, field served empty",
			"field", fieldName, "relation", f.RelationTo, "kind", "has_many", "err", err)
		for _, item := range items {
			item.Data[fieldName] = []map[string]any{}
		}
		return
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
	_ = s.populateRecursiveBatch(ctx, fetched, nextCfg, currentDepth+1, rel.gate)

	// Assign results back.
	for _, item := range items {
		children, ok := relatedMap[item.ID]
		if !ok {
			item.Data[fieldName] = []map[string]any{}
			continue
		}
		out := make([]map[string]any, 0, len(children))
		for _, r := range children {
			flat := flattenContent(r)
			flat = rel.mask(flat)
			out = append(out, flat)
		}
		item.Data[fieldName] = out
	}
}

// populateManyToManyBatch batch-resolves many_to_many relations across all items.
func (s *ContentStore) populateManyToManyBatch(ctx context.Context, items []*domain.Content, rel relationPopulate, nextCfg domain.PopulateConfig, currentDepth int) {
	f, fieldName := rel.field, rel.name
	sourceIDs := make([]uuid.UUID, len(items))
	for i, item := range items {
		sourceIDs[i] = item.ID
	}

	relatedMap, err := s.ListRelatedBatch(ctx, items[0].SchemaName, sourceIDs, f)
	if err != nil {
		slog.ErrorContext(ctx, "content: relation populate failed, field served empty",
			"field", fieldName, "relation", f.RelationTo, "kind", "many_to_many", "err", err)
		for _, item := range items {
			item.Data[fieldName] = []map[string]any{}
		}
		return
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
	_ = s.populateRecursiveBatch(ctx, fetched, nextCfg, currentDepth+1, rel.gate)

	// Assign results back.
	for _, item := range items {
		children, ok := relatedMap[item.ID]
		if !ok {
			item.Data[fieldName] = []map[string]any{}
			continue
		}
		out := make([]map[string]any, 0, len(children))
		for _, r := range children {
			flat := flattenContent(r)
			flat = rel.mask(flat)
			out = append(out, flat)
		}
		item.Data[fieldName] = out
	}
}

// ListRelated returns items from the target schema related to id via a
// many_to_many pivot table.
func (s *ContentStore) ListRelated(ctx context.Context, schemaName string, id uuid.UUID, f domain.SchemaField, limit, offset int) ([]*domain.Content, int, error) {
	pivotName := f.RelationThrough
	if pivotName == "" {
		pivotName = domain.PivotTableName(schemaName, f.RelationTo)
	}
	if err := s.ensureTable(ctx, pivotName); err != nil {
		return nil, 0, fmt.Errorf("list related: ensure pivot tenant column: %w", err)
	}
	targetTable := s.qualify(domain.TableName(f.RelationTo))
	aCol := strings.TrimPrefix(domain.TableName(schemaName), "_") + "_id"
	bCol := strings.TrimPrefix(domain.TableName(f.RelationTo), "_") + "_id"
	pivotName = s.qualify(pivotName)
	tenantID := tenant.ID(ctx)

	// Total count, scoped to the caller's tenant.
	var total int
	row, qErr := s.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = $1 AND tenant_id = $2`, pivotName, aCol), id, tenantID)
	if qErr != nil {
		slog.Warn("content: relation count query failed, pagination total defaults to 0", "table", pivotName, "err", qErr)
	} else if err := row.Scan(&total); err != nil {
		slog.Warn("content: relation count query failed, pagination total defaults to 0", "table", pivotName, "err", err)
	}

	// Resolve target schema for data column expression.
	relSchema, relErr := s.schemas.GetByName(ctx, f.RelationTo)
	if relErr != nil {
		return nil, 0, fmt.Errorf("list related: resolve schema %q: %w", f.RelationTo, relErr)
	}

	// Dialect-aware pagination ($4=limit, $5=offset; $1 is the relation id,
	// $2/$3 the tenant, one per side of the join).
	relPagination := dialect.LimitOffsetPlaceholders(dialect.Must(s.pool.Engine()), 4, 5)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT t.id, t.created_at, t.updated_at,
		       %s
		FROM %s t
		JOIN %s p ON p.%s = t.id
		WHERE p.%s = $1 AND p.tenant_id = $2 AND t.tenant_id = $3
		ORDER BY t.created_at DESC
		%s`,
		dataColumnExpr(s.pool.Engine(), relSchema, "t"),
		targetTable, pivotName, bCol, aCol, relPagination,
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
func (s *ContentStore) SetRelations(ctx context.Context, schemaName string, id uuid.UUID, f domain.SchemaField, targetIDs []uuid.UUID) error {
	pivotName := f.RelationThrough
	if pivotName == "" {
		pivotName = domain.PivotTableName(schemaName, f.RelationTo)
	}
	if err := s.ensureTable(ctx, pivotName); err != nil {
		return fmt.Errorf("set relations: ensure pivot tenant column: %w", err)
	}
	pivotName = s.qualify(pivotName)
	aCol := strings.TrimPrefix(domain.TableName(schemaName), "_") + "_id"
	bCol := strings.TrimPrefix(domain.TableName(f.RelationTo), "_") + "_id"

	// tx is a raw *sql.Tx and bypasses the pool's placeholder rewrite, so the
	// statements are rewritten here for the target engine before execution.
	engine := s.pool.Engine()
	d := dialect.Must(engine)
	tenantID := tenant.ID(ctx)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set relations begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // deferred rollback. Commit result takes precedence

	delQ, delArgs := rewritePlaceholders(
		fmt.Sprintf(`DELETE FROM %s WHERE %s = $1 AND tenant_id = $2`, pivotName, aCol), engine, []any{id, tenantID})
	if _, err := tx.ExecContext(ctx, delQ, delArgs...); err != nil {
		return fmt.Errorf("set relations delete: %w", err)
	}

	insBase := dialect.InsertDoNothing(d, pivotName, []string{TenantColumnName, aCol, bCol}, []string{aCol, bCol})
	for _, tid := range targetIDs {
		insQ, insArgs := rewritePlaceholders(insBase, engine, []any{tenantID, id, tid})
		if _, err := tx.ExecContext(ctx, insQ, insArgs...); err != nil {
			return fmt.Errorf("set relations insert: %w", err)
		}
	}
	return tx.Commit()
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
