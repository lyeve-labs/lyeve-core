package core

import (
	"context"
	"sort"
	"sync"
	"time"
)

// NamedLimit is a count a plugin allows per window for one key, such as one
// mail per address every five minutes.
//
// A plugin enforces a named limit itself, because the key is something only
// its handler knows: the address in a request body, the user an MFA challenge
// belongs to. What it does not own is the number. The plugin declares a
// default and reads the current value at check time, so an operator can move
// the value without the plugin knowing who stores it.
type NamedLimit struct {
	Requests int           `json:"requests"`
	Window   time.Duration `json:"window"`
}

// Valid reports whether the limit can be enforced: at least one request per
// window of at least one second. A zero limit would lock every caller out of
// a login or reset flow, and nothing that edits a limit may produce one.
func (l NamedLimit) Valid() bool {
	return l.Requests >= 1 && l.Window >= time.Second
}

// NamedLimitDecl describes a named limit a plugin enforces.
type NamedLimitDecl struct {
	// Name is "<plugin>.<key>", such as "my-plugin.email".
	Name string `json:"name"`
	// Plugin is the declaring plugin's name.
	Plugin string `json:"plugin"`
	// Description says what the limit counts, for the admin page.
	Description string `json:"description"`
	// Default is the value the plugin enforces until an operator sets one.
	// The plugin passes the same value as ResolveNamedLimit's fallback.
	Default NamedLimit `json:"default"`
}

// NamedLimitResolver answers the configured value of a declared limit. A
// plugin may register one. With none registered every limit runs at the value
// its plugin was built with.
type NamedLimitResolver interface {
	NamedLimit(ctx context.Context, name string) (NamedLimit, bool)
}

var namedLimits = struct {
	sync.RWMutex
	decls    map[string]NamedLimitDecl
	resolver NamedLimitResolver
	// generation numbers each registration, so unregister can tell its own
	// resolver from a later one without comparing the values: a resolver
	// may be a map or another type == panics on.
	generation uint64
}{decls: map[string]NamedLimitDecl{}}

// DeclareNamedLimit records a limit a plugin enforces. Declaring a name again
// replaces the earlier declaration, so a plugin restarted in the same process
// declares from Start without accumulating copies. A declaration whose
// default is not Valid is ignored: it could never be enforced as written.
func DeclareNamedLimit(d NamedLimitDecl) {
	if d.Name == "" || !d.Default.Valid() {
		return
	}
	namedLimits.Lock()
	namedLimits.decls[d.Name] = d
	namedLimits.Unlock()
}

// DeclaredNamedLimits returns every declared limit, sorted by name.
func DeclaredNamedLimits() []NamedLimitDecl {
	namedLimits.RLock()
	out := make([]NamedLimitDecl, 0, len(namedLimits.decls))
	for _, d := range namedLimits.decls {
		out = append(out, d)
	}
	namedLimits.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NamedLimitDeclared reports whether name has been declared.
func NamedLimitDeclared(name string) bool {
	namedLimits.RLock()
	_, ok := namedLimits.decls[name]
	namedLimits.RUnlock()
	return ok
}

// RegisterNamedLimitResolver installs the resolver every ResolveNamedLimit
// call consults, replacing any earlier one. The returned function removes it
// again if it is still the one installed. A plugin calls it from Stop.
func RegisterNamedLimitResolver(r NamedLimitResolver) (unregister func()) {
	namedLimits.Lock()
	namedLimits.generation++
	mine := namedLimits.generation
	namedLimits.resolver = r
	namedLimits.Unlock()
	return func() {
		namedLimits.Lock()
		defer namedLimits.Unlock()
		if namedLimits.generation == mine {
			namedLimits.resolver = nil
		}
	}
}

// ResolveNamedLimit returns the value to enforce for name: the resolver's
// answer when it gives a Valid one, otherwise fallback. Callers pass the value
// they were configured with, normally the default they declared, so a limit
// nobody changed runs at the value its plugin was built with. The
// declared default is what the admin page lists, not a value this returns.
func ResolveNamedLimit(ctx context.Context, name string, fallback NamedLimit) NamedLimit {
	namedLimits.RLock()
	r := namedLimits.resolver
	namedLimits.RUnlock()
	if r != nil {
		if l, ok := r.NamedLimit(ctx, name); ok && l.Valid() {
			return l
		}
	}
	return fallback
}
