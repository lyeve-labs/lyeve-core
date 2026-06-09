package compliance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Mock exporters

type mockExporter struct {
	mu   sync.Mutex
	data map[string]any
	err  error
}

func (m *mockExporter) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data, m.err
}

// Registry tests

func TestSubjectExporterRegistry_RegisterAndLen(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	tests := []struct {
		name       string
		exporters  []SubjectExporter
		wantLength int
	}{
		{
			name:       "empty registry",
			exporters:  nil,
			wantLength: 0,
		},
		{
			name:       "single exporter",
			exporters:  []SubjectExporter{&mockExporter{data: map[string]any{"a": "1"}}},
			wantLength: 1,
		},
		{
			name:       "multiple exporters",
			exporters:  []SubjectExporter{&mockExporter{}, &mockExporter{}, &mockExporter{}},
			wantLength: 3,
		},
		{
			name:       "nil exporter is silently ignored",
			exporters:  []SubjectExporter{nil, &mockExporter{}, nil},
			wantLength: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResetSubjectExporters()

			for _, e := range tt.exporters {
				RegisterSubjectExporter(e)
			}

			if got := globalExporterRegistry.Len(); got != tt.wantLength {
				t.Errorf("Len() = %d; want %d", got, tt.wantLength)
			}
		})
	}
}

func TestSubjectExporterRegistry_Exporters_Snapshot(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	e1 := &mockExporter{data: map[string]any{"a": "1"}}
	e2 := &mockExporter{data: map[string]any{"b": "2"}}

	RegisterSubjectExporter(e1)
	RegisterSubjectExporter(e2)

	snapshot := globalExporterRegistry.Exporters()
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 exporters in snapshot, got %d", len(snapshot))
	}

	// Register another after snapshot.
	RegisterSubjectExporter(&mockExporter{data: map[string]any{"c": "3"}})

	// Original snapshot should be unchanged (defensive copy).
	if len(snapshot) != 2 {
		t.Fatalf("snapshot changed after new registration (not a defensive copy)")
	}

	// Registry should have 3.
	if got := globalExporterRegistry.Len(); got != 3 {
		t.Errorf("Len() = %d; want 3", got)
	}
}

func TestSubjectExporterRegistry_ConcurrentAccess(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	var wg sync.WaitGroup
	const goroutines = 20

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RegisterSubjectExporter(&mockExporter{data: map[string]any{"x": "1"}})
		}()
	}
	wg.Wait()

	if got := globalExporterRegistry.Len(); got != goroutines {
		t.Errorf("expected %d exporters after concurrent registration, got %d", goroutines, got)
	}
}

// Dispatcher tests

func TestRunSubjectExport_Success(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	RegisterSubjectExporter(&mockExporter{
		data: map[string]any{
			"widgets": []map[string]any{
				{"id": "1", "body": "hello"},
				{"id": "2", "body": "world"},
			},
		},
	})
	RegisterSubjectExporter(&mockExporter{
		data: map[string]any{
			"gadgets": []map[string]any{
				{"event": "login"},
			},
		},
	})

	result, err := RunSubjectExport(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Identifier != "user@example.com" {
		t.Errorf("identifier = %q; want %q", result.Identifier, "user@example.com")
	}
	if len(result.Plugins) != 2 {
		t.Errorf("expected 2 plugin sections, got %d", len(result.Plugins))
	}
	if result.Summary.PluginsQueried != 2 {
		t.Errorf("plugins_queried = %d; want 2", result.Summary.PluginsQueried)
	}
	if result.Summary.PluginsWithData != 2 {
		t.Errorf("plugins_with_data = %d; want 2", result.Summary.PluginsWithData)
	}
	if result.Summary.TotalRecords != 3 {
		t.Errorf("total_records = %d; want 3", result.Summary.TotalRecords)
	}
}

func TestRunSubjectExport_NoExporters(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	result, err := RunSubjectExport(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("unexpected error when no exporters: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Summary.PluginsQueried != 0 {
		t.Errorf("plugins_queried = %d; want 0", result.Summary.PluginsQueried)
	}
	if result.Summary.TotalRecords != 0 {
		t.Errorf("total_records = %d; want 0", result.Summary.TotalRecords)
	}
}

func TestRunSubjectExport_SomeExportersHaveNoData(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	// Exporter 1: returns data.
	RegisterSubjectExporter(&mockExporter{
		data: map[string]any{
			"tickets": []map[string]any{
				{"id": "99"},
			},
		},
	})
	// Exporter 2: no matching records, returns nil map.
	RegisterSubjectExporter(&mockExporter{data: nil})

	result, err := RunSubjectExport(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Summary.PluginsQueried != 2 {
		t.Errorf("plugins_queried = %d; want 2", result.Summary.PluginsQueried)
	}
	if result.Summary.PluginsWithData != 1 {
		t.Errorf("plugins_with_data = %d; want 1", result.Summary.PluginsWithData)
	}
	if result.Summary.TotalRecords != 1 {
		t.Errorf("total_records = %d; want 1", result.Summary.TotalRecords)
	}
	if len(result.Plugins) != 1 {
		t.Errorf("expected 1 plugin section, got %d", len(result.Plugins))
	}
}

func TestRunSubjectExport_PartialFailure(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	RegisterSubjectExporter(&mockExporter{
		data: map[string]any{"good": []map[string]any{{"x": "1"}}},
	})
	RegisterSubjectExporter(&mockExporter{
		err: errors.New("export failed"),
	})
	RegisterSubjectExporter(&mockExporter{
		data: map[string]any{"ok2": []map[string]any{{"y": "2"}}},
	})

	result, err := RunSubjectExport(context.Background(), "user@example.com")
	// The function always returns a non-nil result. Error is nil even on partial failure.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Summary.PluginsQueried != 2 {
		t.Errorf("plugins_queried = %d; want 2 (the failing exporter is NOT queried)", result.Summary.PluginsQueried)
	}
	if result.Summary.PluginsWithData != 2 {
		t.Errorf("plugins_with_data = %d; want 2", result.Summary.PluginsWithData)
	}
	if result.Summary.TotalRecords != 2 {
		t.Errorf("total_records = %d; want 2", result.Summary.TotalRecords)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if result.Errors[0].Error != "export failed for plugin" {
		t.Errorf("error[0] = %q; want %q", result.Errors[0].Error, "export failed for plugin")
	}
	if result.Errors[0].Index != 1 {
		t.Errorf("error index = %d; want 1", result.Errors[0].Index)
	}
}

func TestRunSubjectExport_AllFail(t *testing.T) {
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	RegisterSubjectExporter(&mockExporter{err: errors.New("timeout")})
	RegisterSubjectExporter(&mockExporter{err: errors.New("denied")})

	result, err := RunSubjectExport(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Summary.PluginsQueried != 0 {
		t.Errorf("plugins_queried = %d; want 0", result.Summary.PluginsQueried)
	}
	if len(result.Errors) != 2 {
		t.Errorf("expected 2 errors, got %d", len(result.Errors))
	}
}

// Table-driven dispatcher test

func TestRunSubjectExport_TableDriven(t *testing.T) {
	tests := []struct {
		name          string
		exporters     []SubjectExporter
		wantPlugins   int  // number of plugin sections in result
		wantQueried   int  // plugins_queried
		wantWithData  int  // plugins_with_data
		wantRecords   int  // total_records
		wantErrors    int  // number of errors
		wantNilDataOk bool // nil data within exporter is OK (not counted as error)
	}{
		{
			name:         "no exporters",
			exporters:    nil,
			wantPlugins:  0,
			wantQueried:  0,
			wantWithData: 0,
			wantRecords:  0,
			wantErrors:   0,
		},
		{
			name: "single exporter with data",
			exporters: []SubjectExporter{
				&mockExporter{data: map[string]any{
					"items": []map[string]any{{"id": "1"}, {"id": "2"}},
				}},
			},
			wantPlugins:  1,
			wantQueried:  1,
			wantWithData: 1,
			wantRecords:  2,
			wantErrors:   0,
		},
		{
			name: "multiple exporters with data",
			exporters: []SubjectExporter{
				&mockExporter{data: map[string]any{"a": []map[string]any{{"v": 1}}}},
				&mockExporter{data: map[string]any{"b": []map[string]any{{"v": 2}, {"v": 3}}}},
				&mockExporter{data: map[string]any{"c": []map[string]any{{"v": 4}}}},
			},
			wantPlugins:  3,
			wantQueried:  3,
			wantWithData: 3,
			wantRecords:  4,
			wantErrors:   0,
		},
		{
			name: "nil data (no matching records) - not counted as data",
			exporters: []SubjectExporter{
				&mockExporter{data: nil},
				&mockExporter{data: map[string]any{"x": []map[string]any{{"id": "99"}}}},
			},
			wantPlugins:   1,
			wantQueried:   2,
			wantWithData:  1,
			wantRecords:   1,
			wantErrors:    0,
			wantNilDataOk: true,
		},
		{
			name: "one exporter fails, others succeed",
			exporters: []SubjectExporter{
				&mockExporter{data: map[string]any{"ok": []map[string]any{{"r": 1}}}},
				&mockExporter{err: errors.New("exploded")},
				&mockExporter{data: map[string]any{"ok2": []map[string]any{{"r": 2}}}},
			},
			wantPlugins:  2,
			wantQueried:  2,
			wantWithData: 2,
			wantRecords:  2,
			wantErrors:   1,
		},
		{
			name: "multiple failures",
			exporters: []SubjectExporter{
				&mockExporter{err: errors.New("boom")},
				&mockExporter{err: errors.New("bam")},
			},
			wantPlugins:  0,
			wantQueried:  0,
			wantWithData: 0,
			wantRecords:  0,
			wantErrors:   2,
		},
		{
			name: "exporter returns non-slice values counted as zero",
			exporters: []SubjectExporter{
				&mockExporter{data: map[string]any{
					"config": "string_value", // not []map[string]any: counted as 0 rows
				}},
			},
			wantPlugins:  1,
			wantQueried:  1,
			wantWithData: 1,
			wantRecords:  0, // not a slice, so no records counted
			wantErrors:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResetSubjectExporters()
			defer ResetSubjectExporters()

			for _, e := range tt.exporters {
				RegisterSubjectExporter(e)
			}

			result, err := RunSubjectExport(context.Background(), "test-identifier")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("result is nil")
			}
			if result.Identifier != "test-identifier" {
				t.Errorf("identifier = %q; want %q", result.Identifier, "test-identifier")
			}
			if len(result.Plugins) != tt.wantPlugins {
				t.Errorf("plugins sections = %d; want %d", len(result.Plugins), tt.wantPlugins)
			}
			if result.Summary.PluginsQueried != tt.wantQueried {
				t.Errorf("plugins_queried = %d; want %d", result.Summary.PluginsQueried, tt.wantQueried)
			}
			if result.Summary.PluginsWithData != tt.wantWithData {
				t.Errorf("plugins_with_data = %d; want %d", result.Summary.PluginsWithData, tt.wantWithData)
			}
			if result.Summary.TotalRecords != tt.wantRecords {
				t.Errorf("total_records = %d; want %d", result.Summary.TotalRecords, tt.wantRecords)
			}
			if len(result.Errors) != tt.wantErrors {
				t.Errorf("errors count = %d; want %d - got %+v", len(result.Errors), tt.wantErrors, result.Errors)
			}
		})
	}
}

// Integration: eraser + exporter round-trip

func TestSubjectErasureAndExport_Integration(t *testing.T) {
	ResetSubjectErasers()
	ResetSubjectExporters()
	defer ResetSubjectErasers()
	defer ResetSubjectExporters()

	ctx := context.Background()

	// Register one eraser and one exporter.
	eraser := &mockEraser{affected: 3}
	exporter := &mockExporter{
		data: map[string]any{
			"mock": []map[string]any{
				{"id": "1"},
				{"id": "2"},
				{"id": "3"},
			},
		},
	}

	RegisterSubjectEraser(eraser)
	RegisterSubjectExporter(exporter)

	// Export first.
	result, err := RunSubjectExport(ctx, "user@example.com")
	if err != nil {
		t.Fatalf("export error: %v", err)
	}
	if result.Summary.TotalRecords != 3 {
		t.Errorf("expected 3 exported records, got %d", result.Summary.TotalRecords)
	}

	// Then erase.
	total, err := RunSubjectErasure(ctx, "user@example.com")
	if err != nil {
		t.Fatalf("erase error: %v", err)
	}
	if total != 3 {
		t.Errorf("expected 3 erased rows, got %d", total)
	}
}

// Error sanitization tests

func TestRunSubjectExport_ErrorsSanitized(t *testing.T) {
	// ExportError.Error must always be the static sanitized message,
	// regardless of what raw driver error the exporter returns.
	ResetSubjectExporters()
	defer ResetSubjectExporters()

	RegisterSubjectExporter(&mockExporter{
		err: errors.New("ERROR: relation \"sys_profiles\" does not exist (SQLSTATE 42P01)"),
	})
	RegisterSubjectExporter(&mockExporter{
		err: errors.New("connection refused: 127.0.0.1:5432"),
	})

	result, err := RunSubjectExport(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Errors) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(result.Errors))
	}

	for i, e := range result.Errors {
		if e.Error != "export failed for plugin" {
			t.Errorf("ExportError[%d].Error = %q; want %q (raw driver error leaked!)",
				i, e.Error, "export failed for plugin")
		}
		if strings.Contains(e.Error, "SQLSTATE") || strings.Contains(e.Error, "sys_profiles") ||
			strings.Contains(e.Error, "connection refused") {
			t.Errorf("ExportError[%d] contains raw driver error text: %q", i, e.Error)
		}
	}
	// Index should still correlate to the failing exporter.
	if result.Errors[0].Index != 0 || result.Errors[1].Index != 1 {
		t.Errorf("error indices: got [%d, %d]; want [0, 1]",
			result.Errors[0].Index, result.Errors[1].Index)
	}
}

// Two exporters that name a section the same way must both be kept, or a
// subject-access response reports one plugin's rows under a heading belonging
// to another and omits the rest entirely.
func TestExportSubject_SectionNameCollisionKeepsBoth(t *testing.T) {
	ResetSubjectExporters()
	t.Cleanup(ResetSubjectExporters)
	RegisterSubjectExporter(exporterFunc(func(context.Context, string) (map[string]any, error) {
		return map[string]any{"audit": []map[string]any{{"from": "first"}}}, nil
	}))
	RegisterSubjectExporter(exporterFunc(func(context.Context, string) (map[string]any, error) {
		return map[string]any{"audit": []map[string]any{{"from": "second"}}}, nil
	}))

	res, err := RunSubjectExport(context.Background(), "subject@example.invalid")
	if err != nil {
		t.Fatalf("ExportSubject: %v", err)
	}
	if len(res.Plugins) != 2 {
		t.Fatalf("sections = %d, want 2: %v", len(res.Plugins), res.Plugins)
	}
	if res.Summary.TotalRecords != 2 {
		t.Errorf("total_records = %d, want 2", res.Summary.TotalRecords)
	}
}

// exporterFunc adapts a function to the SubjectExporter interface.
type exporterFunc func(context.Context, string) (map[string]any, error)

func (f exporterFunc) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	return f(ctx, identifier)
}

// A partial export that reads as complete tells the subject this is everything
// held about them when it is not.
func TestExportSubject_IncompleteFlag(t *testing.T) {
	t.Run("a failing exporter marks the bundle incomplete", func(t *testing.T) {
		ResetSubjectExporters()
		t.Cleanup(ResetSubjectExporters)
		RegisterSubjectExporter(exporterFunc(func(context.Context, string) (map[string]any, error) {
			return map[string]any{"ok": []map[string]any{{"a": 1}}}, nil
		}))
		RegisterSubjectExporter(exporterFunc(func(context.Context, string) (map[string]any, error) {
			return nil, errors.New("store unavailable")
		}))

		res, err := RunSubjectExport(context.Background(), "subject@example.invalid")
		if err != nil {
			t.Fatalf("RunSubjectExport: %v", err)
		}
		if !res.Incomplete {
			t.Error("incomplete must be true when an exporter failed")
		}
		if len(res.Errors) == 0 {
			t.Error("the failure must also be listed")
		}
	})

	t.Run("a clean export is complete", func(t *testing.T) {
		ResetSubjectExporters()
		t.Cleanup(ResetSubjectExporters)
		RegisterSubjectExporter(exporterFunc(func(context.Context, string) (map[string]any, error) {
			return map[string]any{"ok": []map[string]any{{"a": 1}}}, nil
		}))

		// Scoped, because this case is about exporters answering and not about
		// tenant coverage. An export that resolved no tenant reached nothing
		// and is incomplete for that reason alone, which would pass this
		// assertion for the wrong reason and fail it for the right one.
		ctx := core.WithTenantID(context.Background(), "acme")
		res, err := RunSubjectExport(ctx, "subject@example.invalid")
		if err != nil {
			t.Fatalf("RunSubjectExport: %v", err)
		}
		if res.Incomplete {
			t.Error("incomplete must be false when every exporter answered")
		}
	})
}

type namedExporter struct{ name string }

func (e *namedExporter) ExportSubject(context.Context, string) (map[string]any, error) {
	return map[string]any{e.name: []any{}}, nil
}

// A plugin that registers again on reconfiguration must not duplicate its rows
// in the export.
func TestRegister_SameExporterTwiceIsOneEntry(t *testing.T) {
	r := &SubjectExporterRegistry{}
	e := &namedExporter{name: "widgets"}

	r.Register(e)
	r.Register(e)
	r.Register(e)

	if got := r.Len(); got != 1 {
		t.Fatalf("registry holds %d exporters; want 1", got)
	}
}

// Two plugins that happen to be the same type are still two exporters.
func TestRegister_DistinctExportersAreBothKept(t *testing.T) {
	r := &SubjectExporterRegistry{}

	r.Register(&namedExporter{name: "widgets"})
	r.Register(&namedExporter{name: "gadgets"})

	if got := r.Len(); got != 2 {
		t.Fatalf("registry holds %d exporters; want 2", got)
	}
}
