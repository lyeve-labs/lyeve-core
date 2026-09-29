package licensing

import (
	"context"
	"sort"
)

// Open is the verifier of a build that links none. Every compiled plugin
// starts and is ungated, every name in Env.Names is granted, nothing is
// withheld from any tenant, no ceiling applies and no route is served.
//
// Plan and state read "free", the plan an install reports with no license.
func Open() Verifier { return open{} }

// IsOpen reports whether v verifies no license: it is Open, or it is nil,
// which the engine runs as Open. The entitlements endpoint reports the
// opposite as license_module, so a console can tell a build that links a
// licensing implementation from one that links none.
func IsOpen(v Verifier) bool {
	if v == nil {
		return true
	}
	_, ok := v.(open)
	return ok
}

type open struct{}

func (open) NewManager(_ context.Context, env Env) (Manager, error) {
	seen := make(map[string]struct{}, len(env.Names))
	names := make([]string, 0, len(env.Names))
	for _, n := range env.Names {
		if _, dup := seen[n]; dup || n == "" {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	sort.Strings(names)
	return openManager{names: names}, nil
}

// openManager grants what the build compiled and nothing else. It holds no
// license, so it never changes and has nothing to run.
type openManager struct{ names []string }

func (openManager) Start(context.Context) {}

func (openManager) OnChange(func(Change)) {}

func (m openManager) Snapshot() Snapshot {
	return Snapshot{
		Plan:     "free",
		State:    "free",
		Features: append([]string{}, m.names...),
		Caps:     map[string]int{},
	}
}

func (openManager) Plugin(string) PluginGrant { return PluginGrant{Start: true, Ungated: true} }

func (openManager) Withholds(string, string) bool { return false }

func (openManager) WithheldFrom(string) []string { return []string{} }
