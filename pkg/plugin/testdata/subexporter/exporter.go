// Package subexporter declares a subject exporter outside the package of
// any plugin, as an exporter kept in a plugin's subpackage would be.
package subexporter

import "context"

// Exporter returns one row for any subject.
type Exporter struct{}

// ExportSubject returns one row keyed by the subject.
func (Exporter) ExportSubject(_ context.Context, identifier string) (map[string]any, error) {
	return map[string]any{"sub_rows": []map[string]any{{"subject": identifier}}}, nil
}
