package compliance

import (
	"context"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
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
//
// Addresses, when set, returns the addresses the set's tables hold for an
// account, and every eraser reaches the rows kept under them (see
// SubjectAddresses). It is read only where the set's tables are whole. Final
// runs the set with the account eraser, once every other eraser has
// succeeded, so the rows Addresses reads are still there on every run until
// then.
type SubjectErasure struct {
	Tables    []string
	Columns   []string
	Erase     SubjectEraseFunc
	Unkeyed   SubjectEraseFunc
	Addresses SubjectAddressFunc
	Final     bool
}

// DeclaredEraser runs a SubjectErasure behind the check for its tables. The
// activator builds one per declared set, and plugin tests run them the same
// way.
type DeclaredEraser struct {
	name      string
	host      core.Host
	set       core.TableSet
	erase     SubjectEraseFunc
	unkeyed   SubjectEraseFunc
	addresses SubjectAddressFunc
	final     bool
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
	return &DeclaredEraser{name: name, host: host, set: set, erase: se.Erase, unkeyed: se.Unkeyed, addresses: se.Addresses, final: se.Final}
}

// EraserName names the plugin that declared the eraser.
func (e *DeclaredEraser) EraserName() string { return e.name }

// EraseSubject erases the subject where the set's tables are whole, and
// reports nothing erased where the plugin never ran.
func (e *DeclaredEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	state, err := e.check(ctx)
	if err != nil {
		return 0, err
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

// accountAddresses returns the addresses the set's tables hold for the
// account, and none where the tables are not whole.
func (e *DeclaredEraser) accountAddresses(ctx context.Context, accountID string) ([]string, error) {
	if e.addresses == nil {
		return nil, nil
	}
	state, err := e.check(ctx)
	if err != nil || state != core.TableSetReady {
		return nil, err
	}
	return e.addresses(ctx, e.host, accountID)
}

// check reads which of the set's tables and columns are present.
func (e *DeclaredEraser) check(ctx context.Context) (core.TableSetState, error) {
	// On SQL Server the catalog read takes shared locks that a schema change
	// running at the same time can deadlock with, and the server may choose
	// the read as the victim. It runs on the pool, outside any transaction,
	// and changes nothing, so it is read again.
	var state core.TableSetState
	err := sqlx.RetryOnTxConflict(ctx, e.name+" erasure catalog read", func() error {
		var err error
		state, err = e.set.Check(ctx, e.host.Querier(ctx), e.host.Dialect())
		return err
	})
	if err != nil {
		return state, fmt.Errorf("%s erasure: %w", e.name, err)
	}
	return state, nil
}
