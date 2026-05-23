package config

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The admin layer.
//
// Operator-set configuration lives in sys_plugin_config, one row per plugin,
// and is flattened into the single key space the resolver answers from. Keys
// are global because the names they resolve under are: a plugin key resolves to
// its upper-cased variable name, which is process-wide. Two plugins declaring
// the same key therefore already shared one setting, so the collision is
// reported rather than silently resolved.
//
// Only the default tenant's rows apply, whether they were written with that
// slug or with no tenant at all. Plugins start once per process, before any
// request has identified a tenant, so a row belonging to some other tenant has
// nothing to configure.

// appliesProcessWide reports whether a stored row configures the process
// rather than one tenant among several.
//
// A save made through the admin API carries the request's tenant, which on a
// single-tenant install is the default slug rather than the empty string. A
// row written outside a request has no tenant at all, and both mean the same
// thing here.
func appliesProcessWide(tenantID string) bool {
	return tenantID == "" || tenantID == core.DefaultTenantSlug
}

// AdminConfigStore is the part of db.PluginConfigStore this package needs.
type AdminConfigStore interface {
	ListAll(ctx context.Context) ([]db.PluginConfigRow, error)
}

// AdminLayer is a flattened set of operator-set values, with the problems found
// while building it.
type AdminLayer struct {
	Values map[string]string

	// Conflicts names keys more than one plugin claims, as
	// "cache_ttl: analytics, search".
	Conflicts []string

	// Undecryptable names credentials that could not be decrypted, usually
	// because ENCRYPTION_KEY changed since they were saved.
	Undecryptable []string
}

// BuildAdminLayer flattens stored plugin configuration into resolver keys.
func BuildAdminLayer(rows []db.PluginConfigRow) AdminLayer {
	layer := AdminLayer{Values: make(map[string]string)}
	owner := make(map[string]string)
	conflicts := make(map[string]map[string]struct{})

	for _, row := range rows {
		if !appliesProcessWide(row.TenantID) {
			continue
		}
		for _, key := range row.UndecryptableKeys {
			layer.Undecryptable = append(layer.Undecryptable, row.PluginName+"."+key)
		}
		for key, value := range row.Config {
			name := normalizeKey(key)
			if prev, taken := owner[name]; taken && prev != row.PluginName {
				if conflicts[name] == nil {
					conflicts[name] = map[string]struct{}{prev: {}}
				}
				conflicts[name][row.PluginName] = struct{}{}
			}
			owner[name] = row.PluginName
			layer.Values[name] = configValueString(value)
		}
	}

	for name, plugins := range conflicts {
		names := make([]string, 0, len(plugins))
		for p := range plugins {
			names = append(names, p)
		}
		sort.Strings(names)
		layer.Conflicts = append(layer.Conflicts, name+": "+strings.Join(names, ", "))
	}
	sort.Strings(layer.Conflicts)
	sort.Strings(layer.Undecryptable)
	return layer
}

// RefreshAdminLayer reloads operator-set configuration and installs it on the
// active resolver.
//
// Called at boot and again after every save, so a value set in the admin UI
// takes effect on a running engine. A plugin reading its configuration per
// request sees the new value immediately. One that cached it in Start sees it
// when it reloads.
func RefreshAdminLayer(ctx context.Context, store AdminConfigStore) (AdminLayer, error) {
	if store == nil {
		return AdminLayer{}, nil
	}
	rows, err := store.ListAll(ctx)
	if err != nil {
		return AdminLayer{}, fmt.Errorf("load operator configuration: %w", err)
	}
	layer := BuildAdminLayer(rows)
	ActiveResolver().SetAdminLayer(layer.Values)
	return layer, nil
}

// configValueString renders a stored JSON value the way the environment would
// carry it. Strings pass through unquoted. Everything else is JSON-encoded so
// the parsers on the other side: ParseBool, ParseDuration, comma-split lists -
// see the same text an operator would have put in a variable.
func configValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode to float64. Render integers without a decimal
		// point so "30" does not reach a plugin as "30.000000".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case []any:
		// A list setting is read by splitting on commas, so render it the way
		// the environment would have carried it rather than as a JSON array.
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, configValueString(item))
		}
		return strings.Join(parts, ",")
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// CoreSettingsOwner is the row in sys_plugin_config that holds engine settings
// saved through the admin API, as opposed to a plugin's own configuration.
//
// Engine settings have no owning plugin, but the admin layer is keyed by one,
// so they are stored under a name no plugin may register. Sharing the table
// keeps one refresh path and one reload for both kinds of save.
const CoreSettingsOwner = "core"
