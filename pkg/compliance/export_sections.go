package compliance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Export sections
//
// A person exporting their own data chooses what goes in the bundle, and a
// tenant chooses what it offers them. Both choose among sections. A section is
// the data of one plugin that is running now, named by the plugin, or the
// engine's own data, named EngineExportSection. A plugin that is not running
// offers no section and contributes nothing, even though an exporter it
// registered earlier stays in the registry.
//
// The routes that export everything, RunSubjectExport and
// RunSubjectExportAcrossTenants, do not read sections at all.

// EngineExportSection is the section of the exporters the engine registers
// for the records it owns itself, such as the account.
const EngineExportSection = "engine"

// ErrExportSectionUnavailable is returned when a requested section names no
// section the install can export now.
var ErrExportSectionUnavailable = errors.New("export section is not available")

// RegisterSubjectExporterAs registers an exporter as part of the named
// section. The runtime registers a plugin's exporter this way when it takes
// it from the plugin, and the engine registers its own under
// EngineExportSection. An exporter a plugin registers itself from Start names
// no section, and the runtime attributes it to the plugin that defines it.
func RegisterSubjectExporterAs(section string, e SubjectExporter) {
	globalExporterRegistry.registerAs(section, e)
}

// ExportSectionResolver names the section an exporter answers for and
// reports whether that section is active now. section is what the exporter
// was registered under, and is empty when the registration named none. An
// exporter that resolves to no section is in no section.
type ExportSectionResolver func(e SubjectExporter, section string) (resolved string, active bool)

var sectionResolver = struct {
	mu sync.RWMutex
	fn ExportSectionResolver
}{}

// SetExportSectionResolver installs the resolver the runtime builds from the
// plugins it runs. Nil restores the default, which keeps every exporter in
// the section it was registered under and treats that section as active.
func SetExportSectionResolver(r ExportSectionResolver) {
	sectionResolver.mu.Lock()
	defer sectionResolver.mu.Unlock()
	sectionResolver.fn = r
}

func resolveSection(e SubjectExporter, section string) (string, bool) {
	sectionResolver.mu.RLock()
	fn := sectionResolver.fn
	sectionResolver.mu.RUnlock()
	if fn == nil {
		return section, section != ""
	}
	return fn(e, section)
}

// UnsectionedExporter is a registered exporter that resolves to no section.
// No export of chosen sections can reach it, so its data is missing from
// every self-service bundle.
type UnsectionedExporter struct {
	// Index is the exporter's position in the registry.
	Index int

	// Package is the import path of the package that declares the
	// exporter's concrete type, which is what attribution matched on.
	Package string
}

// UnsectionedSubjectExporters lists every registered exporter that resolves
// to no section now.
func UnsectionedSubjectExporters() []UnsectionedExporter {
	_, unsectioned := resolveExportRuns()
	return unsectioned
}

// activeExportRuns returns every registered exporter whose section is active
// now, with its section and its position in the registry.
func activeExportRuns() []exportRun {
	runs, _ := resolveExportRuns()
	return runs
}

// resolveExportRuns resolves every registered exporter once, and splits off
// the ones that resolve to no section from the ones whose section is active.
func resolveExportRuns() ([]exportRun, []UnsectionedExporter) {
	r := globalExporterRegistry
	r.mu.Lock()
	exporters := append([]SubjectExporter(nil), r.exporters...)
	sections := append([]string(nil), r.sections...)
	r.mu.Unlock()

	var runs []exportRun
	var unsectioned []UnsectionedExporter
	for i, e := range exporters {
		section, active := resolveSection(e, sections[i])
		if section == "" {
			unsectioned = append(unsectioned, UnsectionedExporter{Index: i, Package: exporterPackage(e)})
			continue
		}
		if active {
			runs = append(runs, exportRun{index: i, section: section, exporter: e})
		}
	}
	return runs, unsectioned
}

// exporterPackage is the import path of the package that declares e's
// concrete type, looking through pointers.
func exporterPackage(e SubjectExporter) string {
	t := reflect.TypeOf(e)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return ""
	}
	return t.PkgPath()
}

// SubjectExportSections lists, sorted, the sections an export can hold now.
func SubjectExportSections() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, run := range activeExportRuns() {
		if !seen[run.section] {
			seen[run.section] = true
			out = append(out, run.section)
		}
	}
	sort.Strings(out)
	return out
}

// SubjectExportSectionsResult is the bundle of an export of chosen sections.
type SubjectExportSectionsResult struct {
	*SubjectExportResult

	// Sections maps each section that put data in the bundle to the keys of
	// Plugins it put there.
	Sections map[string][]string `json:"sections"`
}

// RunSubjectExportSections runs the exporters of the chosen sections for one
// subject, in the tenant ctx names and in no other. A context naming no
// tenant is refused, because the only scope left would be every tenant. A
// section that is not active now is refused with ErrExportSectionUnavailable
// before any exporter runs, so a bundle never silently lacks a section that
// was asked for. An exporter that resolves to no section cannot be asked for
// at all, so while one is registered every bundle is marked incomplete.
//
// The subject's addresses are resolved as RunSubjectExport resolves them, and
// a set that cannot be read returns ErrSubjectAddressesUnresolved.
func RunSubjectExportSections(ctx context.Context, identifier string, sections []string) (*SubjectExportSectionsResult, error) {
	tenant, err := core.RequireTenantID(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, s := range sections {
		want[s] = true
	}
	active, unsectioned := resolveExportRuns()
	var runs []exportRun
	for _, run := range active {
		if want[run.section] {
			runs = append(runs, run)
		}
	}
	available := map[string]bool{}
	for _, run := range runs {
		available[run.section] = true
	}
	var missing []string
	for s := range want {
		if !available[s] {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%w: %s", ErrExportSectionUnavailable, strings.Join(missing, ", "))
	}

	identifier = canonicalSubject(identifier)
	ctx, err = resolveExportAddresses(ctx, identifier)
	if err != nil {
		return nil, err
	}
	per, keys := runExporters(ctx, identifier, runs)
	// An exporter in no section is outside every choice, so the bundle can
	// never hold its data. Saying so keeps the bundle from reading as whole.
	for _, u := range unsectioned {
		per.Errors = append(per.Errors, ExportError{
			Index: u.Index,
			Error: "some data is held by an exporter that answers for no section, so no export of chosen sections reaches it",
		})
	}
	result := &SubjectExportResult{
		Identifier: identifier,
		Plugins:    per.Plugins,
		Errors:     per.Errors,
		Summary:    per.Summary,
		Scope:      ExportScope{Mode: "tenant", Tenants: []string{tenant}, Requested: 1, Covered: 1},
		Incomplete: len(per.Errors) > 0,
	}
	if per.Summary.TotalRecords > 0 {
		result.MatchedTenants = []string{tenant}
	}
	return &SubjectExportSectionsResult{SubjectExportResult: result, Sections: keys}, nil
}
