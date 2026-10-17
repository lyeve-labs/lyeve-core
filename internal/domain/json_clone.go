package domain

// CloneJSONMap returns a copy of m that shares no map or slice with it. Nil
// stays nil.
func CloneJSONMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = CloneJSONValue(v)
	}
	return out
}

// CloneJSONValue copies the container shapes a content document or a field
// default can hold. Those are the shapes a JSON value decodes into, plus the
// []map[string]any that relation population writes for a has-many field.
// Anything else is a scalar or an immutable value and is returned as it is.
func CloneJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return CloneJSONMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = CloneJSONValue(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, e := range t {
			out[i] = CloneJSONMap(e)
		}
		return out
	default:
		return v
	}
}
