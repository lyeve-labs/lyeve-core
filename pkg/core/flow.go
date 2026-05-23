package core

import (
	"context"
	"errors"
	"strings"
)

// Flow contributions.
//
// A plugin can offer node types and trigger types to whichever plugin runs
// flows without either importing the other: both sides depend only on the
// shapes here. A contributing plugin implements FlowNodeProvider or
// FlowTriggerProvider. The engine collects the contributions of every started
// plugin into one FlowRegistry and hands that to the plugin that asserts
// FlowRegistryHost. A plugin that is not started, because the license does
// not grant it or because it failed to boot, has no entry in the registry,
// so its types do not exist as far as a flow is concerned.
//
// The compile-time pin is the same one every optional capability uses:
//
//	var _ core.FlowNodeProvider = (*Plugin)(nil)
//
// FlowInvocation and FlowNodeSpec are structs so a field can be added without
// touching a single provider. Nothing here is renamed once a provider exists.

// FlowPort is one named input or output of a node. Multiple means several
// edges may land on it and the node receives a list.
type FlowPort struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Multiple bool   `json:"multiple,omitempty"`
}

// FlowNodeSpec describes one node or trigger type: what the palette shows,
// what the inspector renders and what the compiler validates against.
//
// Type is "<plugin>.<verb>", where the plugin half is the contributing
// plugin's name as FlowTypePrefix spells it. The engine refuses a provider
// whose types carry any other prefix, so a plugin cannot publish a type under
// another plugin's name.
//
// ConfigSchema is JSON Schema 2020-12 with additionalProperties false, and
// Example is a config that passes Validate. Plugin is the contributing
// plugin's name. The engine sets it and overwrites whatever the provider put
// there.
type FlowNodeSpec struct {
	Type         string         `json:"type"`
	Category     string         `json:"category"`
	Label        string         `json:"label"`
	Description  string         `json:"description"`
	Inputs       []FlowPort     `json:"inputs"`
	Outputs      []FlowPort     `json:"outputs"`
	ConfigSchema map[string]any `json:"config_schema"`
	Example      map[string]any `json:"example"`
	SideEffects  bool           `json:"side_effects"`
	Trigger      bool           `json:"trigger"`
	Plugin       string         `json:"plugin"`
}

// FlowInvocation is what a node receives for one run.
//
// Config has every expression already evaluated. Inputs are keyed by input
// port, each holding the values that arrived on it. DryRun asks the node to
// report what it would do without doing it. A node with side effects must
// honor it. Eval evaluates an expression against the run's scope, with extra
// layered on top, for a node that evaluates per item. Log writes into the
// run's step record.
//
// The tenant travels in ctx, set with WithTenantID, and a node reads it with
// TenantIDFromCtx. It is never in Config: a flow runs as its tenant, and a
// node that could name another one would be a way across the boundary.
type FlowInvocation struct {
	TenantID string
	NodeID   string
	Config   map[string]any
	Inputs   map[string][]any
	DryRun   bool
	Eval     func(expr string, extra map[string]any) (any, error)
	Log      func(level, msg string, fields map[string]any)
}

// FlowResult carries a node's outputs, keyed by output port.
type FlowResult struct {
	Outputs map[string]any
}

// FlowNode is one node type. Validate sees the authored config, expressions
// included, and refuses unknown keys. Run executes one invocation.
type FlowNode interface {
	Spec() FlowNodeSpec
	Validate(cfg map[string]any) error
	Run(ctx context.Context, inv *FlowInvocation) (*FlowResult, error)
}

// FlowTrigger is one trigger type. Subscribe arms the trigger for a tenant
// with the given config and calls fire with the payload each time it goes
// off, until unsubscribe is called.
type FlowTrigger interface {
	Spec() FlowNodeSpec
	Validate(cfg map[string]any) error
	Subscribe(ctx context.Context, tenantID string, cfg map[string]any, fire func(payload map[string]any)) (unsubscribe func(), err error)
}

// FlowNodeProvider is implemented by a plugin that contributes node types.
// The engine asks for them after the plugin has started.
type FlowNodeProvider interface {
	FlowNodes() []FlowNode
}

// FlowTriggerProvider is implemented by a plugin that contributes trigger
// types. The engine asks for them after the plugin has started.
type FlowTriggerProvider interface {
	FlowTriggers() []FlowTrigger
}

// FlowRegistryHost is the host role the plugin that runs flows asserts to
// reach the contributions of every other started plugin. The engine host
// implements it. A scoped host without CapFlowRegistry answers with an empty
// registry and logs the denial once.
type FlowRegistryHost interface {
	FlowRegistry() FlowRegistry
}

// FlowRegistry is every node and trigger type the started plugins contribute,
// with Plugin set on each spec.
//
// Nodes and Triggers return the current snapshot. The engine rebuilds it after
// the plugins have started and again after each license change, once the
// plugins that lost their license have stopped and the ones that gained it
// have started. OnChange registers a function the engine calls after every
// rebuild, in registration order, on the goroutine that performed the
// rebuild: the boot goroutine after start, and the goroutine that published
// the license change after that. The subscriber may read Nodes and Triggers
// from inside the call and sees the new snapshot. It must not block, because
// the publisher is waiting on it.
type FlowRegistry interface {
	Nodes() []FlowNode
	Triggers() []FlowTrigger
	OnChange(func()) Subscription
}

// FlowProblem names one thing wrong with a definition. NodeID is empty for a
// flow-level problem, and Path is then a JSON pointer from the document root.
// With a NodeID, Path is a pointer from that node's object, so a config
// problem reads "/config/left_key". The trigger uses NodeID "trigger".
type FlowProblem struct {
	NodeID  string `json:"node_id,omitempty"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// FlowDefinitionValidator checks a flow definition the way the import route
// would, without persisting anything. Format is "json", "yaml" or "" to
// sniff. Normalized is the definition as the plugin that runs flows would
// store it, in JSON, and problems is every defect found. The err result is
// set only when the definition could not be read at all or the check itself
// failed. A definition that parses but is wrong answers with problems and a
// nil err, so a caller can show the draft with its defects instead of
// nothing.
//
// The plugin that runs flows registers one at Start through
// FlowDefinitionValidatorRegistrar. A plugin that drafts definitions, such
// as an assistant that writes them from a prompt, fetches it through
// FlowDefinitionValidatorProvider and validates before it shows the draft.
// Accepting the draft is still the import route, so that plugin's own gate
// and body limit apply and no second write path exists.
type FlowDefinitionValidator interface {
	Validate(ctx context.Context, tenantID string, def []byte, format string) (normalized []byte, problems []FlowProblem, err error)
}

// FlowDefinitionValidatorRegistrar is implemented by the engine host. The
// plugin that runs flows calls it in Start, and again in Stop with nil.
type FlowDefinitionValidatorRegistrar interface {
	RegisterFlowDefinitionValidator(v FlowDefinitionValidator)
}

// FlowDefinitionValidatorProvider is implemented by the engine host. Nil
// means no plugin has registered a validator: the plugin that runs flows is
// not licensed, not started, or has stopped.
type FlowDefinitionValidatorProvider interface {
	FlowDefinitionValidator() FlowDefinitionValidator
}

// FlowInvoker starts a published flow on behalf of a transport that is not
// the flow runner's own HTTP routes. A GraphQL mutation, a gRPC method or a
// realtime socket message hands the caller's context, the flow's slug and
// the decoded input to the plugin that runs flows, and gets back what the
// HTTP route would have answered. The flow's own auth mode, rate limit and
// response cache apply, keyed by surface, so callers on different
// transports have separate buckets and cache entries and an admin-only flow
// stays unreachable from any of them.
//
// The tenant and the caller's claims travel in ctx exactly as the
// transport's middleware left them. A transport never names a tenant in an
// argument. A flow answers a transport only when its trigger declares that
// transport's surface, and the license decision belongs to the plugin that
// runs flows. A transport reads no feature name. When the tenant's license
// does not cover the call, that plugin answers an error wrapping
// ErrNotGranted, and the transport relays it as a payment refusal.
type FlowInvoker interface {
	// Invoke runs the published flow named by slug in the tenant on ctx.
	// surface names the transport (FlowSurfaceGraphQL, FlowSurfaceGRPC or
	// FlowSurfaceRealtime), for the auth check, the rate-limit and cache
	// keys, and the run record. A flow that does not exist, is not
	// published, does not declare the surface, or whose auth mode refuses
	// it answers ErrFlowNotFound. A license that does not cover the call
	// answers an error wrapping ErrNotGranted. A refused rate limit answers
	// ErrFlowRateLimited. A call nested deeper than the flow runner allows
	// answers ErrFlowDepthExceeded.
	Invoke(ctx context.Context, slug, surface string, input map[string]any) (*FlowInvokeResult, error)
	// Published lists the flows a transport may expose in the tenant on
	// ctx: published flows on an HTTP trigger whose auth mode admits a
	// non-admin surface, each with the surfaces its trigger declares. A
	// transport lists only the flows whose Surfaces name it. A license that
	// does not cover the transports answers an error wrapping ErrNotGranted.
	Published(ctx context.Context) ([]FlowSummary, error)
}

// The surfaces a transport names when it invokes a flow, spelled the way a
// flow's trigger declares them.
const (
	FlowSurfaceGraphQL  = "graphql"
	FlowSurfaceGRPC     = "grpc"
	FlowSurfaceRealtime = "realtime"
)

// FlowSummary is what a transport may say about a flow it exposes.
// InputSchema is JSON Schema for the input, or nil when the trigger carries
// none. Surfaces lists the transports the flow's trigger admits, beside
// the REST routes every HTTP flow has.
type FlowSummary struct {
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Auth        string         `json:"auth"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Surfaces    []string       `json:"surfaces,omitempty"`
}

// Admits reports whether the flow's trigger declares surface.
func (f FlowSummary) Admits(surface string) bool {
	for _, s := range f.Surfaces {
		if s == surface {
			return true
		}
	}
	return false
}

// FlowInvokeResult is what one Invoke produced: the run's id, the status
// and headers a response node set or the flow runner derived, and the body.
// A rate-limited or cached answer carries its limit and cache headers here,
// so a transport can relay them where it has somewhere to put them.
type FlowInvokeResult struct {
	RunID   string
	Status  int
	Headers map[string]string
	Body    any
}

// FlowInvokerRegistrar is implemented by the engine host. The plugin that
// runs flows calls it when it activates and again with nil when it
// deactivates, so the registration follows the license, not the process.
type FlowInvokerRegistrar interface {
	RegisterFlowInvoker(inv FlowInvoker)
}

// FlowInvokerProvider is implemented by the engine host. Nil means no
// plugin has registered an invoker: the plugin that runs flows is not
// licensed, not started, or has stopped. A transport reads it at call time
// or at schema build time and exposes its flow surface only while it is
// non-nil.
type FlowInvokerProvider interface {
	FlowInvoker() FlowInvoker
}

var (
	// ErrFlowNotFound is answered by Invoke when the slug names no flow the
	// surface may see: missing, unpublished, blocked, or refused by its auth
	// mode. One error for all four, so a transport cannot tell a caller
	// which flows exist behind a mode that excludes them.
	ErrFlowNotFound = errors.New("flow not found")
	// ErrFlowRateLimited is answered by Invoke when the flow's own rate
	// limit refuses the call on this surface.
	ErrFlowRateLimited = errors.New("flow rate limit exceeded")
	// ErrFlowDepthExceeded is answered by Invoke when the call would nest
	// flows deeper than the flow runner allows.
	ErrFlowDepthExceeded = errors.New("flow call depth exceeded")
)

// FlowTypePrefix is the prefix every type a plugin contributes must carry:
// the plugin's name with hyphens as underscores, then a dot. A route names
// the plugin as "my-plugin". A type is an identifier the expression language
// reads, so the same plugin contributes "my_plugin.verb".
func FlowTypePrefix(plugin string) string {
	return strings.ReplaceAll(plugin, "-", "_") + "."
}

// FlowTypeOwnedBy reports whether typ is a type the named plugin may
// contribute: it starts with the plugin's prefix and carries a verb after it.
func FlowTypeOwnedBy(plugin, typ string) bool {
	prefix := FlowTypePrefix(plugin)
	return strings.HasPrefix(typ, prefix) && len(typ) > len(prefix)
}

// emptyFlowRegistry is what a scoped host without CapFlowRegistry hands
// back. It looks like a registry with no contributions, which is what a
// plugin sees on an install where nothing contributes, so the denial is
// logged where it happens.
type emptyFlowRegistry struct{}

func (emptyFlowRegistry) Nodes() []FlowNode            { return nil }
func (emptyFlowRegistry) Triggers() []FlowTrigger      { return nil }
func (emptyFlowRegistry) OnChange(func()) Subscription { return deniedSubscription{} }
