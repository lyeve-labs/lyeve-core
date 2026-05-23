package config

import (
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Layered configuration lookup.
//
// Every value the engine reads passes through one Resolver so there is a single
// answer to "where did this setting come from". The single path is what lets
// a configuration file and the admin UI supply values, and what lets an
// operator see why the value they set is not the value in effect.
//
// Sources are consulted highest first:
//
//	environment  a variable set in the process
//	file         a key set in the YAML configuration tree
//	admin        a key set through the admin UI, stored in sys_plugin_config
//	(unset)      the caller's own default
//
// A file key may declare itself overridable, which lets the admin layer take it
// over. The file value then acts as the default the UI starts from. Without that
// marker a file key is locked. The admin UI reads Provenance to render a locked
// key read-only with its origin named, rather than offering a field whose edits
// do nothing.
//
// The environment has the same opt-in and it is spelled LYEVE_OVERRIDABLE: a
// comma list of key names the admin layer may take over, read once at boot. A
// listed key resolves exactly as an !overridable file key does, so there is one
// mechanism and one code path rather than two.
//
// The opt-in is a variable and never a control on the admin page, and that is
// the whole design. What makes the file's marker safe is not that it is narrow.
// It is that the operator declares it at deploy time, in the artifact they
// control. A switch inside the admin that let the admin layer outrank the
// environment would hand whoever holds the admin password the authority of
// whoever holds the deployment, and those are not always the same person.
//
// core.IsOperatorOnlyKey keeps its veto over the list. A key it names stays
// operator-only however LYEVE_OVERRIDABLE is written, so a secret cannot be
// opened by a typo in a variable.

// Source names where a resolved value came from.
type Source uint8

const (
	// SourceNone means no layer supplied the key and the caller's own default
	// applies.
	SourceNone Source = iota
	// SourceAdmin is a value set through the admin UI.
	SourceAdmin
	// SourceFile is a value set in the YAML configuration tree.
	SourceFile
	// SourceEnv is a variable set in the process environment.
	SourceEnv
)

// String renders the source as the lowercase word the admin UI and the
// diagnostics endpoint display.
func (s Source) String() string {
	switch s {
	case SourceAdmin:
		return "admin"
	case SourceFile:
		return "file"
	case SourceEnv:
		return "env"
	default:
		return "default"
	}
}

// Resolution is the full answer to a lookup: the value, which layer supplied
// it, and where an operator would go to change it.
type Resolution struct {
	Key   string
	Value string
	From  Source

	// Origin locates a file-sourced value, as "lyeve.yaml:42". Empty for every
	// other source.
	Origin string

	// Overridable reports whether the admin layer is allowed to set this key.
	// False when the environment supplies it, when a file supplies it without
	// the !overridable tag, or when the key is operator-only.
	Overridable bool

	// OperatorOnly marks a key the admin layer may never set
	// (core.OperatorOnlyKeys), whatever the other layers hold.
	OperatorOnly bool
}

// Resolver answers configuration lookups from the layered sources.
//
// The file layer is fixed at boot. The admin layer is swapped atomically when
// an operator saves a change, so a live process picks up new values without a
// restart. Readers see either the whole old map or the whole new one.
type Resolver struct {
	file  map[string]FileValue
	admin atomic.Pointer[map[string]string]

	// envOpen holds the keys LYEVE_OVERRIDABLE names, normalized. Read once at
	// boot and never written after, so it needs no lock: an opt-in that could
	// change under a running process is the escalation this design refuses.
	envOpen map[string]struct{}

	// read holds every key the engine has asked for, which is what this build
	// treats as configuration. Provenance needs it to report a setting the
	// environment alone supplies: such a key is in no layer it can walk, so
	// without this it is invisible to the admin API while the save handler
	// refuses it as env-pinned, and a page cannot tell that from a setting
	// nobody has set.
	//
	// Only Get records. Resolve is also called with names that arrive in a
	// request body, and recording those would let a caller put anything it
	// liked on an admin screen.
	mu   sync.RWMutex
	read map[string]struct{}
}

// NewResolver builds a resolver over a loaded file layer. Pass nil for an
// engine configured entirely from the environment.
func NewResolver(file map[string]FileValue) *Resolver {
	if file == nil {
		file = map[string]FileValue{}
	}
	return &Resolver{file: file, envOpen: parseOverridable(os.Getenv(OverridableEnvKey))}
}

// OverridableEnvKey names the variable that opens environment-set keys to the
// admin layer.
const OverridableEnvKey = "LYEVE_OVERRIDABLE"

// parseOverridable reads the comma list. An empty entry is skipped rather than
// normalized to "", which would otherwise put a zero-length key in the set and
// open nothing while looking like it opened something.
func parseOverridable(list string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(list, ",") {
		if name := normalizeKey(part); name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

// envOverridable reports whether LYEVE_OVERRIDABLE opened this key. The
// variable can never open itself: a deployment that listed it would let the
// admin layer rewrite the opt-in, which is the escalation the design refuses.
func (r *Resolver) envOverridable(name string) bool {
	if r == nil || name == OverridableEnvKey {
		return false
	}
	_, ok := r.envOpen[name]
	return ok
}

// SetAdminLayer installs operator-set configuration. Safe to call on a running
// engine. The previous map is replaced wholesale rather than mutated.
func (r *Resolver) SetAdminLayer(values map[string]string) {
	if r == nil {
		return
	}
	upper := make(map[string]string, len(values))
	for k, v := range values {
		upper[normalizeKey(k)] = v
	}
	r.admin.Store(&upper)
}

// Resolve returns the effective value for a key along with its provenance.
//
// The key is matched case-insensitively in the environment's upper-cased form,
// so the plugin-side key example_host and the variable EXAMPLE_HOST are the same
// setting, which is the mapping plugins already relied on.
func (r *Resolver) Resolve(key string) Resolution {
	name := normalizeKey(key)
	res := Resolution{Key: name}
	if name == "" {
		return res
	}

	fv, inFile := r.fileValue(name)

	// An environment variable wins, and presence is what counts rather than a
	// non-empty value: setting a variable to empty is how an operator says
	// "unset this", so a lower layer must not paper over it. An operator
	// debugging a live system sets a variable and expects it to hold.
	//
	// The exception is a key the deployment listed in LYEVE_OVERRIDABLE, which
	// is the same opt-in the file's !overridable tag gives, declared in the same
	// place the rest of the deployment is. An operator-only key is checked
	// first, below, so the list can never open one.
	envValue, inEnv := os.LookupEnv(name)
	if inEnv && !r.envOverridable(name) {
		res.Value, res.From = envValue, SourceEnv
		return res
	}

	// An operator-only key answers from the environment or the file, and never
	// from the admin layer. A value the admin layer holds for it, stored before
	// the key was reserved or written around the save route, is ignored, and so
	// is LYEVE_OVERRIDABLE naming it.
	if core.IsOperatorOnlyKey(name) {
		res.OperatorOnly = true
		switch {
		case inEnv:
			res.Value, res.From = envValue, SourceEnv
		case inFile:
			res.Value, res.From = fv.Value, SourceFile
			res.Origin = fv.Origin
		}
		return res
	}

	// An opened environment key. Reaching here means the variable is set and
	// LYEVE_OVERRIDABLE named it, because the pinned case returned above. The
	// file is not consulted at all: the environment outranks it, and opening a
	// key hands it to the admin layer rather than to the file underneath.
	// Otherwise the file's locked value would beat the variable, inverting the
	// one ordering every deployment depends on.
	if inEnv {
		if v, ok := r.adminValue(name); ok {
			res.Value, res.From = v, SourceAdmin
		} else {
			res.Value, res.From = envValue, SourceEnv
		}
		res.Overridable = true
		return res
	}

	// A file key that has not opened itself to the admin layer is next, and is
	// the final answer.
	if inFile && !fv.Overridable {
		res.Value, res.From = fv.Value, SourceFile
		res.Origin = fv.Origin
		return res
	}

	if v, ok := r.adminValue(name); ok {
		res.Value, res.From = v, SourceAdmin
		res.Overridable = true
		if inFile {
			res.Origin = fv.Origin
		}
		return res
	}

	// An overridable file key with nothing stored against it yet: the file
	// value applies and the UI may still edit it.
	if inFile {
		res.Value, res.From = fv.Value, SourceFile
		res.Origin = fv.Origin
		res.Overridable = true
		return res
	}

	// Unknown to both files and the environment, so the admin layer owns it.
	res.Overridable = true
	return res
}

// Lookup returns the effective value and whether any layer supplied it.
func (r *Resolver) Lookup(key string) (string, bool) {
	res := r.Resolve(key)
	return res.Value, res.From != SourceNone
}

// Get returns the effective value, or "" when no layer supplies the key.
func (r *Resolver) Get(key string) string {
	res := r.Resolve(key)
	if res.Key != "" {
		r.markRead(res.Key)
	}
	return res.Value
}

// markRead adds a key to the set this build treats as configuration. A nil
// resolver is usable for reads, so it accepts one and records nothing.
func (r *Resolver) markRead(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.read == nil {
		r.read = make(map[string]struct{})
	}
	r.read[name] = struct{}{}
	r.mu.Unlock()
}

// ReadKeys returns the keys the engine has asked for, sorted. It is what the
// engine has read so far rather than a fixed list: Load reads every engine key
// before anything is served, so the set is complete by the time a request
// arrives, and a key no build ever reads is not that build's configuration.
func (r *Resolver) ReadKeys() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]string, 0, len(r.read))
	for k := range r.read {
		out = append(out, k)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Provenance reports every key any layer knows about, sorted by key. The admin
// UI uses it to show which settings it may edit and which are pinned by the
// deployment.
//
// Secret values are not redacted here. Callers that expose this over HTTP must
// redact before responding. See the plugin configuration endpoints.
func (r *Resolver) Provenance() []Resolution {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{})
	for k := range r.file {
		seen[k] = struct{}{}
	}
	if m := r.admin.Load(); m != nil {
		for k := range *m {
			seen[k] = struct{}{}
		}
	}
	// The environment is listed only where it supplies a key the engine
	// actually reads. The full environment of a container is not configuration
	// and would put unrelated process state on an admin screen, which is why
	// this is bounded by the read set rather than by os.Environ.
	//
	// Leaving it out entirely would be worse. A .env file becomes process
	// environment, so on an install configured that way no setting would
	// appear at all, while the save handler would refuse each one as env-pinned.
	r.mu.RLock()
	for k := range r.read {
		if _, ok := os.LookupEnv(k); ok {
			seen[k] = struct{}{}
		}
	}
	r.mu.RUnlock()

	out := make([]Resolution, 0, len(seen))
	for k := range seen {
		out = append(out, r.Resolve(k))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// FileKeys reports how many keys the file layer supplied, for boot logging.
func (r *Resolver) FileKeys() int {
	if r == nil {
		return 0
	}
	return len(r.file)
}

// IgnoredAdminKeys reports operator-only keys the admin layer holds a value
// for. The resolver ignores those values, and an install that stored one
// would otherwise change behavior with no word:
// backups encrypted with KMS would fall back to local keys. Boot names them.
func (r *Resolver) IgnoredAdminKeys() []string {
	if r == nil {
		return nil
	}
	admin := r.admin.Load()
	if admin == nil {
		return nil
	}
	var out []string
	for k := range *admin {
		if core.IsOperatorOnlyKey(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// BlankedKeys reports keys a lower layer sets that an empty environment
// variable pins to empty.
//
// Setting a variable to empty deliberately turns a setting off, so the resolver
// honors it. But the same thing happens by accident when a deployment moves
// settings from a .env file into the YAML tree and leaves the old names behind
// with nothing after the equals sign: godotenv loads them into the process, and
// the new file values silently do nothing. Boot logs this so the operator sees
// which settings their leftovers are suppressing.
func (r *Resolver) BlankedKeys() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(r.file))
	for k := range r.file {
		seen[k] = struct{}{}
	}
	if m := r.admin.Load(); m != nil {
		for k := range *m {
			seen[k] = struct{}{}
		}
	}
	var out []string
	for k := range seen {
		if v, ok := os.LookupEnv(k); ok && v == "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (r *Resolver) fileValue(name string) (FileValue, bool) {
	if r == nil || r.file == nil {
		return FileValue{}, false
	}
	fv, ok := r.file[name]
	return fv, ok
}

func (r *Resolver) adminValue(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	m := r.admin.Load()
	if m == nil {
		return "", false
	}
	v, ok := (*m)[name]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// normalizeKey renders a key in the environment-variable form every layer is
// indexed by: upper case, with dots and dashes folded to underscores so a
// nested YAML path and a flat variable name reach the same entry.
func normalizeKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		switch {
		case r == '.' || r == '-' || r == ' ':
			b.WriteByte('_')
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Process-wide resolver
//
// Load installs the resolver the engine reads through. It is process-wide
// because the configuration it answers for is: the engine loads one file tree
// and starts one set of plugins. Tests that need a different tree call
// SetActiveResolver and restore the previous value.

var active atomic.Pointer[Resolver]

// ActiveResolver returns the resolver in force. It never returns nil: before
// Load runs, an empty resolver answers from the environment alone, which is the
// behavior of an engine with no configuration file.
func ActiveResolver() *Resolver {
	if r := active.Load(); r != nil {
		return r
	}
	return emptyResolver
}

var emptyResolver = NewResolver(nil)

// SetActiveResolver installs the process-wide resolver and returns the one it
// replaced, so a caller can restore it.
func SetActiveResolver(r *Resolver) *Resolver {
	prev := active.Load()
	active.Store(r)
	if prev == nil {
		return emptyResolver
	}
	return prev
}

// lookup resolves a key through the active resolver. Every environment read in
// this package goes through it so a value can come from the file tree or the
// admin layer without each call site knowing that.
func lookup(key string) string {
	return ActiveResolver().Get(key)
}
