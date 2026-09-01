package schemaimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Action is what importing one schema would do to the target.
type Action string

const (
	// ActionCreate means the target has no schema by this name.
	ActionCreate Action = "create"
	// ActionUpdate means a schema by this name exists and differs.
	ActionUpdate Action = "update"
	// ActionUnchanged means the target already holds this definition.
	ActionUnchanged Action = "unchanged"
)

// PlannedSchema is one schema's place in an import.
type PlannedSchema struct {
	Name   string `json:"name"`
	Action Action `json:"action"`

	// DDL is the statements the schema engine would run. Empty for an
	// unchanged schema.
	DDL []string `json:"ddl,omitempty"`

	// Dependencies names the schemas this one references. A name not in the
	// bundle must already exist in the target.
	Dependencies []string `json:"dependencies,omitempty"`

	// Missing names dependencies that are neither in the bundle nor in the
	// target, so applying this schema would fail.
	Missing []string `json:"missing,omitempty"`
}

// Plan is the full effect of an import, in the order it would be applied.
type Plan struct {
	Schemas []PlannedSchema `json:"schemas"`

	// SecondPass names schemas caught in a reference cycle, which are applied
	// twice: once to create the table, and again once every table exists to add
	// the constraints the first pass had to leave out.
	SecondPass []string `json:"second_pass,omitempty"`
}

// Blocked reports the schemas whose dependencies cannot be satisfied. An import
// with any of these would fail part-way, leaving the target holding some of the
// bundle and not the rest.
func (p Plan) Blocked() []string {
	var out []string
	for _, s := range p.Schemas {
		if len(s.Missing) > 0 {
			out = append(out, s.Name)
		}
	}
	return out
}

// Changes reports how many schemas the import would create or alter.
func (p Plan) Changes() int {
	n := 0
	for _, s := range p.Schemas {
		if s.Action != ActionUnchanged {
			n++
		}
	}
	return n
}

// Engine is the part of core.SchemaEngine an import needs.
type Engine interface {
	Apply(ctx context.Context, name string, definition json.RawMessage) error
	PreviewDDL(ctx context.Context, name string, definition json.RawMessage) ([]core.DDLStatement, error)
	Get(ctx context.Context, name string) (json.RawMessage, error)
	List(ctx context.Context) ([]json.RawMessage, error)
}

// PlanImport works out what importing a bundle would do without changing
// anything.
//
// Always run before Apply on anything that matters: an import is a sequence of
// DDL statements against a live database, and the point of a preview is to see
// the destructive ones before they run rather than after.
func PlanImport(ctx context.Context, eng Engine, b *Bundle) (Plan, error) {
	if eng == nil {
		return Plan{}, errors.New("no schema engine")
	}
	ordered, cyclic := b.Ordered()

	existing, err := existingNames(ctx, eng)
	if err != nil {
		return Plan{}, err
	}
	inBundle := make(map[string]struct{}, len(b.Schemas))
	for _, s := range b.Schemas {
		inBundle[s.Name] = struct{}{}
	}

	plan := Plan{Schemas: make([]PlannedSchema, 0, len(ordered)), SecondPass: cyclic}
	for _, s := range ordered {
		item := PlannedSchema{Name: s.Name, Dependencies: Dependencies(s)}
		for _, dep := range item.Dependencies {
			if _, ok := inBundle[dep]; ok {
				continue
			}
			if _, ok := existing[dep]; ok {
				continue
			}
			item.Missing = append(item.Missing, dep)
		}

		definition, err := json.Marshal(s)
		if err != nil {
			return Plan{}, fmt.Errorf("schema %q: %w", s.Name, err)
		}

		item.Action = ActionCreate
		if _, ok := existing[s.Name]; ok {
			same, err := matchesStored(ctx, eng, s.Name, definition)
			if err != nil {
				return Plan{}, err
			}
			item.Action = ActionUpdate
			if same {
				item.Action = ActionUnchanged
			}
		}

		if item.Action != ActionUnchanged && len(item.Missing) == 0 {
			// A preview against a table whose dependency is absent would report
			// the missing reference rather than the statements, and Missing
			// already says that more usefully.
			stmts, err := eng.PreviewDDL(ctx, s.Name, definition)
			if err != nil {
				return Plan{}, fmt.Errorf("preview %q: %w", s.Name, err)
			}
			for _, st := range stmts {
				item.DDL = append(item.DDL, st.SQL)
			}
		}
		plan.Schemas = append(plan.Schemas, item)
	}
	return plan, nil
}

// ApplyBundle imports a bundle, in dependency order.
//
// Refuses to start when any schema's dependencies are missing. Applying what it
// can and stopping at the first failure would leave the target holding part of
// a project, which is harder to recover from than an import that never began.
func ApplyBundle(ctx context.Context, eng Engine, b *Bundle) (Plan, error) {
	plan, err := PlanImport(ctx, eng, b)
	if err != nil {
		return Plan{}, err
	}
	if blocked := plan.Blocked(); len(blocked) > 0 {
		return plan, fmt.Errorf("import would fail: %v reference schemas that are neither in the bundle nor in the target", blocked)
	}

	byName := make(map[string]domain.Schema, len(b.Schemas))
	for _, s := range b.Schemas {
		byName[s.Name] = s
	}
	cyclic := make(map[string]bool, len(plan.SecondPass))
	for _, name := range plan.SecondPass {
		cyclic[name] = true
	}

	apply := func(name string, full bool) error {
		s, ok := byName[name]
		if !ok {
			return nil
		}
		if !full {
			s = FirstPass(s)
		}
		definition, err := json.Marshal(s)
		if err != nil {
			return fmt.Errorf("schema %q: %w", name, err)
		}
		if err := eng.Apply(ctx, name, definition); err != nil {
			return fmt.Errorf("apply %q: %w", name, err)
		}
		return nil
	}

	for _, item := range plan.Schemas {
		if item.Action == ActionUnchanged {
			continue
		}
		if err := apply(item.Name, !cyclic[item.Name]); err != nil {
			return plan, err
		}
	}

	// Schemas in a reference cycle could not carry their constraints on the
	// first pass, because the table on the other side did not exist yet.
	for _, name := range plan.SecondPass {
		if err := apply(name, true); err != nil {
			return plan, fmt.Errorf("second pass: %w", err)
		}
	}
	return plan, nil
}

// ExportBundle reads every schema in the target into a bundle.
func ExportBundle(ctx context.Context, eng Engine, source string) (*Bundle, error) {
	if eng == nil {
		return nil, errors.New("no schema engine")
	}
	raw, err := eng.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}
	b := &Bundle{Version: BundleVersion, Source: source}
	for _, def := range raw {
		var s domain.Schema
		if err := json.Unmarshal(def, &s); err != nil {
			return nil, fmt.Errorf("read stored schema: %w", err)
		}
		b.Schemas = append(b.Schemas, s)
	}
	// Exported in dependency order so the file can be replayed top to bottom by
	// anything that does not sort it again.
	ordered, _ := b.Ordered()
	b.Schemas = ordered
	return b, nil
}

func existingNames(ctx context.Context, eng Engine) (map[string]struct{}, error) {
	raw, err := eng.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list existing schemas: %w", err)
	}
	out := make(map[string]struct{}, len(raw))
	for _, def := range raw {
		var s struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(def, &s); err != nil {
			continue
		}
		if s.Name != "" {
			out[s.Name] = struct{}{}
		}
	}
	return out, nil
}

// matchesStored reports whether the target already holds this definition.
// Compared as normalized JSON rather than as text, so key order and whitespace
// do not make an identical schema look like a change.
func matchesStored(ctx context.Context, eng Engine, name string, definition json.RawMessage) (bool, error) {
	stored, err := eng.Get(ctx, name)
	if err != nil {
		// The name is listed but unreadable. Treat it as a change rather than
		// failing the plan: Apply will report the real problem.
		return false, nil
	}
	var a, bb any
	if err := json.Unmarshal(stored, &a); err != nil {
		return false, nil
	}
	if err := json.Unmarshal(definition, &bb); err != nil {
		return false, nil
	}
	x, err := json.Marshal(a)
	if err != nil {
		return false, nil
	}
	y, err := json.Marshal(bb)
	if err != nil {
		return false, nil
	}
	return bytes.Equal(x, y), nil
}
