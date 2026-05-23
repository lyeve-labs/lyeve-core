package api

import (
	"context"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// PluginStatusProvider is anything that can produce the runtime's plugin
// status report. The runtime passes its activator. Tests can pass a stub.
type PluginStatusProvider interface {
	Status() plugin.PluginStatusReport
}

// PluginSchemaProvider is anything that can produce a JSON Schema for a
// named plugin's configuration. The runtime passes its activator. Tests can
// pass a stub.
type PluginSchemaProvider interface {
	PluginSchema(name string) map[string]any
}

// PluginConfigReloader asks active plugins to rebuild configuration-derived
// state. Satisfied by the plugin activator.
type PluginConfigReloader interface {
	ReloadConfig(ctx context.Context) (int, []error)
}

// pluginsStatusHandler returns the plugin activation status report.
// GET /api/admin/plugins/status
// Auth: admin or super_admin.
func pluginsStatusHandler(provider PluginStatusProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		if provider == nil {
			_ = jsonpool.WriteJSON(w, plugin.PluginStatusReport{}) // err suppressed: response write to client
			return
		}
		_ = jsonpool.WriteJSON(w, provider.Status()) // err suppressed: response write to client
	}
}

// runningPluginsBody is which plugins serve the caller's tenant. It carries
// names only, never a reason, an error or a URL, because every signed-in role
// reads it and those are an operator's.
type runningPluginsBody struct {
	// Plugins is every compiled plugin running or waiting to start on first
	// use, less the ones withheld from the caller's tenant, sorted.
	Plugins []string `json:"plugins"`
	// Withheld is every plugin that runs but is withheld from the caller's
	// tenant, sorted, so a console can say a screen is turned off for this
	// tenant rather than missing from the install.
	Withheld []string `json:"withheld"`
}

// runningPluginsHandler returns the plugins that serve the caller's tenant,
// so a console shows a screen only when the plugin behind it runs. Neither
// list is ever null.
// GET /api/admin/plugins/running
// Auth: any signed-in role.
func runningPluginsHandler(provider PluginStatusProvider, ent EntitlementProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		body := runningPluginsBody{Plugins: []string{}, Withheld: []string{}}
		if provider != nil {
			withheld := map[string]bool{}
			for _, name := range ent.WithheldFrom(core.TenantIDFromCtx(r.Context())) {
				withheld[name] = true
			}
			for _, s := range provider.Status().Plugins {
				if !s.Compiled || (s.Phase != plugin.PhaseRunning && s.Phase != plugin.PhaseLazy) {
					continue
				}
				if withheld[s.Name] {
					body.Withheld = append(body.Withheld, s.Name)
				} else {
					body.Plugins = append(body.Plugins, s.Name)
				}
			}
			sort.Strings(body.Plugins)
			sort.Strings(body.Withheld)
		}
		_ = jsonpool.WriteJSON(w, body) // err suppressed: response write to client
	}
}

// pluginSchemaHandler returns the JSON Schema for a plugin's configuration.
// GET /api/admin/plugins/{name}/schema
// Auth: admin or super_admin.
func pluginSchemaHandler(provider PluginSchemaProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		if provider == nil {
			httpx.ErrorReq(w, r, http.StatusNotFound, "plugin schema provider not configured")
			return
		}

		name := chi.URLParam(r, "name")
		if name == "" {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "plugin name is required")
			return
		}

		schema := provider.PluginSchema(name)
		if schema == nil {
			httpx.ErrorReq(w, r, http.StatusNotFound, "no configuration schema for plugin: "+name)
			return
		}

		_ = jsonpool.WriteJSON(w, schema) // err suppressed: response write to client
	}
}
