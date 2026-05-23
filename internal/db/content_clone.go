package db

import "github.com/lyeve-labs/lyeve-core/internal/domain"

// Content read caches store what the database returned, and every caller gets
// its own copy of it.
//
// The read paths hand callers a *domain.Content whose Data map is decorated
// afterwards: PopulateWithConfigBatch writes the resolved relation into
// Data[field], the API layer replaces Data with a masked copy, and the
// after-response hook is handed the map to do as it likes with. If the cache
// stored and returned the same object, two concurrent requests for the same
// page would share one map. One request writing a populated relation into it
// while another marshals it for its ETag ends the process with
//
//	fatal error: concurrent map iteration and map write
//
// which is not a panic and cannot be recovered.
//
// Copying at the cache boundary rather than adding populate to the cache key
// is deliberate. A key that carried populate would still leave two concurrent
// identical populate requests mutating one shared map. A shared object would
// also let whichever request missed the cache first decide whether every later
// reader sees populated relations or not. A cache of raw rows has no such
// ambiguity.
//
// The cost is one copy per read of a cached row. These are single content
// documents, and the copy is a map walk rather than a re-serialization.

// cloneContent returns a copy of c that shares no mutable state with it.
// The scalar fields are copied by value. Only Data needs deep work.
func cloneContent(c *domain.Content) *domain.Content {
	if c == nil {
		return nil
	}
	out := *c
	out.Data = cloneJSONMap(c.Data)
	return &out
}

// cloneContents copies a page of rows. A nil slice stays nil, because callers
// distinguish "no cache entry" from "an entry holding no rows".
func cloneContents(in []*domain.Content) []*domain.Content {
	if in == nil {
		return nil
	}
	out := make([]*domain.Content, len(in))
	for i, c := range in {
		out[i] = cloneContent(c)
	}
	return out
}

func cloneJSONMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneJSONValue(v)
	}
	return out
}

// cloneJSONValue copies the container shapes a content document can hold. Those
// are the shapes a JSON column decodes into, plus the []map[string]any that
// relation population writes for a has-many field. Anything else is a scalar or
// an immutable value and is copied as it is.
func cloneJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneJSONMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneJSONValue(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, e := range t {
			out[i] = cloneJSONMap(e)
		}
		return out
	default:
		return v
	}
}
