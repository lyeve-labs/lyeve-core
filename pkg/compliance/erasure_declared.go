package compliance

import (
	"context"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// SubjectEraserDeclarer is a plugin that holds data subjects' rows. The
// engine reads it from every plugin compiled into the build at boot, whether
// or not the plugin is allowed to start, and registers each SubjectErasure it
// returns.
//
// A plugin that stops running keeps its tables and the people in them.
// Registering the eraser from Start would leave that data out of every
// erasure the moment the plugin stops starting, while the erasure still
// reported success.
type SubjectEraserDeclarer interface {
	SubjectErasures() []SubjectErasure
}

// SubjectEraseFunc erases one data subject from a set of a plugin's tables
// and returns the rows it deleted or anonymized. host is scoped to database
// reads and writes. It runs on an instance that has never started, so it
// reads nothing Start builds.
type SubjectEraseFunc func(ctx context.Context, host core.Host, identifier string) (int64, error)

// SubjectErasure is one set of a plugin's tables holding subjects' rows, and
// the function that erases a subject from them.
//
// Tables and Columns follow core.TenantPurge. A set whose tables are all
// absent is skipped, because the plugin never ran here. A set whose Columns
// are all absent runs Unkeyed instead of Erase, and is skipped when Unkeyed
// is nil, because the plugin last ran before the migration that added them
// and Erase would fail on its first statement. A set partly present is an
// error naming what is missing, so the erasure reports itself incomplete
// rather than quietly erasing part of the set.
type SubjectErasure struct {
	Tables  []string
	Columns []string
	Erase   SubjectEraseFunc
	Unkeyed SubjectEraseFunc
}

// DeclaredEraser runs a SubjectErasure behind the check for its tables. The
// activator builds one per declared set, and plugin tests run them the same
// way.
type DeclaredEraser struct {
	name    string
	host    core.Host
	set     core.TableSet
	erase   SubjectEraseFunc
	unkeyed SubjectEraseFunc
}

var _ SubjectEraser = (*DeclaredEraser)(nil)

// NewDeclaredEraser returns the eraser for se, named for the plugin that
// declared it. It returns nil when se has no function or no tables.
func NewDeclaredEraser(name string, host core.Host, se SubjectErasure) *DeclaredEraser {
	if se.Erase == nil || host == nil {
		return nil
	}
	set := core.NewTableSet(se.Tables, se.Columns)
	if len(set.Tables()) == 0 {
		return nil
	}
	return &DeclaredEraser{name: name, host: host, set: set, erase: se.Erase, unkeyed: se.Unkeyed}
}

// EraserName names the plugin that declared the eraser.
func (e *DeclaredEraser) EraserName() string { return e.name }

// EraseSubject erases the subject where the set's tables are whole, and
// reports nothing erased where the plugin never ran.
func (e *DeclaredEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	state, err := e.set.Check(ctx, e.host.Querier(ctx), e.host.Dialect())
	if err != nil {
		return 0, fmt.Errorf("%s erasure: %w", e.name, err)
	}
	switch state {
	case core.TableSetReady:
		return e.erase(ctx, e.host, identifier)
	case core.TableSetUnkeyed:
		if e.unkeyed != nil {
			return e.unkeyed(ctx, e.host, identifier)
		}
	}
	return 0, nil
}
