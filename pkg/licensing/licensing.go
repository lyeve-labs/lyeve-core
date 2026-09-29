// Package licensing is the seam the engine reads a license through.
//
// The engine holds no license format, no verification key, no license server
// and no entitlement policy. A build that verifies licenses links a Verifier,
// and the Manager it builds answers every question the engine asks: which
// compiled plugins may start, what the instance is entitled to, what a tenant
// is refused, and which routes the implementation serves itself. A build that
// links none runs on Open, which starts every compiled plugin.
//
// Which licenses a binary honors is a property of what was compiled into it.
// Nothing here reads an environment variable, a configuration key or a
// request to choose a verifier.
package licensing

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/cluster"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Verifier is the licensing implementation a build links. A build that links
// none gets Open.
type Verifier interface {
	// NewManager builds the license in force and loads it once, before any
	// plugin starts. A missing or invalid license is not an error: it is an
	// install the license entitles to less. An error means the verifier
	// itself is broken, and it stops the boot.
	NewManager(ctx context.Context, env Env) (Manager, error)
}

// Env is what the engine hands a verifier. It names no license server, key
// format or table, so what a verifier makes of each field is its own
// business.
type Env struct {
	// Credential is LYEVE_LICENSE_KEY as the operator set it. The verifier
	// decides what it is.
	Credential string
	// CacheDir is LYEVE_LICENSE_CACHE_DIR, where a verifier may keep what
	// has to outlive the process without a database.
	CacheDir string
	// InstanceID is INSTANCE_ID.
	InstanceID string
	// ServerURL is LYEVE_LICENSE_SERVER_URL. Empty means the verifier's
	// default.
	ServerURL string
	// DB is the engine's pool. It is nil when the engine runs without a
	// database.
	DB *sql.DB
	// Dialect is the database in use as the engine spells it: "postgres",
	// "mysql" or "mssql". It is empty when DB is nil.
	Dialect string
	// EncryptionKey is the instance master key, for a verifier that keeps a
	// secret at rest.
	EncryptionKey string
	// Bus is the relay the replicas of one install share. On a single
	// instance it carries nothing, and it is nil when the engine runs
	// without a database.
	Bus cluster.Bus
	// Names is every compiled plugin's name and every name a compiled plugin
	// declares through core.FeatureDeclarer, sorted.
	Names []string
	// Logger is the engine's logger.
	Logger *slog.Logger
}

// Manager is the license in force and the policy that reads it. It is safe
// for concurrent use.
type Manager interface {
	// Start runs the manager's background work until ctx ends: expiry,
	// renewal and following a change another replica made.
	Start(ctx context.Context)
	// OnChange registers the function called after the entitlement changed,
	// on this replica or on another one.
	OnChange(fn func(Change))
	// Snapshot is the finished entitlement of the whole instance.
	Snapshot() Snapshot
	// Plugin says whether the compiled plugin name may start and how its
	// failure counts.
	Plugin(name string) PluginGrant
	// Withholds reports whether name is refused to tenant although the
	// instance is entitled to it. It is false for an empty tenant.
	Withholds(tenant, name string) bool
	// WithheldFrom is every name refused to tenant, including each name that
	// falls with one withheld, sorted. It is never nil.
	WithheldFrom(tenant string) []string
}

// Snapshot is the finished entitlement of the instance. GET
// /api/admin/entitlements serves it, so the JSON names are that endpoint's
// wire format.
type Snapshot struct {
	Plan string `json:"plan"`
	// PlanLabel is the plan as an operator reads it. The implementation
	// chooses the words, and empty leaves a console to show Plan.
	PlanLabel string `json:"plan_label,omitempty"`
	State     string `json:"state"`
	// Features is every name the instance is entitled to, sorted. It is
	// never nil.
	Features []string `json:"features"`
	// TenantQuota is how many tenants the instance may hold, where 0 is
	// unlimited.
	TenantQuota int `json:"tenant_quota"`
	// Caps is every named capacity ceiling, where 0 is unlimited. It is never
	// nil.
	Caps map[string]int `json:"caps"`
	// Source says what renews the license, for the operator.
	Source string `json:"license_source,omitempty"`
	// ExpiresAt is when the license in force expires.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// GraceEndsAt is when the licensed features stop. It is set only while
	// State is "grace".
	GraceEndsAt *time.Time `json:"grace_ends_at,omitempty"`
	// Problem is why the license is not what the operator configured, for
	// the operator. It never carries a key or a token.
	Problem string `json:"license_error,omitempty"`
}

// PluginGrant is whether a compiled plugin may start.
type PluginGrant struct {
	// Start is true when the plugin may start.
	Start bool
	// Ungated is true for a plugin that starts whatever the license says.
	// Its failure to start fails readiness, and its routes name no license
	// feature.
	Ungated bool
	// Reason says why the plugin may not start. The plugin status shows it.
	Reason string
	// UpgradeURL is where an operator goes to get the plugin. The plugin
	// status shows it when the plugin may not start.
	UpgradeURL string
}

// Change is what a change of the entitlement publishes: the plan and the
// feature names the license carries, as the license states them.
type Change struct {
	Plan     string
	Features []string
}

// RouteProvider is a Manager that serves routes of its own, such as the one
// a super admin renews the license through. The engine mounts them for the
// owner it names.
type RouteProvider interface {
	RouteOwner() string
	Routes() []core.RouteDecl
}

// StartObserver is a Manager that needs the names of the plugins serving
// routes, which is what a tenant can be refused. The engine calls it after
// the plugins start and again whenever the routes they serve change.
type StartObserver interface {
	PluginsStarted(names []string)
}
