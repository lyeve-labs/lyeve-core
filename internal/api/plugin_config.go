package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// Plugin configuration.
//
// Stored configuration fills keys the environment and the configuration file do
// not set, and a file key may open itself to this layer with the !overridable
// tag. A save installs the new values on the running engine, so a plugin that
// reads its configuration per request picks them up without a restart. One that
// cached it at Start sees the change when it reloads.
//
// Credentials are encrypted at rest and never returned: a set credential reads
// as db.SecretMask, and submitting the mask back leaves the stored value alone.

// pluginConfigHandler serves a plugin's stored configuration.
// GET /api/admin/plugins/{name}/config
func pluginConfigHandler(store *db.PluginConfigStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		if name == "" {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "plugin name is required")
			return
		}
		if store == nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "plugin configuration store unavailable")
			return
		}

		cfg, err := store.Get(r.Context(), name)
		if errors.Is(err, domain.ErrNotFound) {
			// The editor reads 404 as "no stored configuration" and falls back
			// to the schema defaults, so this is a normal answer, not a fault.
			httpx.ErrorReq(w, r, http.StatusNotFound, "no stored configuration for plugin: "+name)
			return
		}
		if err != nil {
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to load plugin configuration")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, cfg)
	}
}

// pluginConfigSaveHandler replaces a plugin's stored configuration.
// PUT /api/admin/plugins/{name}/config
// Body: a JSON object of configuration keys.
func pluginConfigSaveHandler(store *db.PluginConfigStore, provider PluginSchemaProvider, reloader PluginConfigReloader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		if name == "" {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "plugin name is required")
			return
		}
		if store == nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "plugin configuration store unavailable")
			return
		}
		// Refuse to store configuration for a plugin that declares no schema:
		// the keys would be unreadable by anything and the editor would show an
		// operator settings that do nothing.
		if provider == nil || provider.PluginSchema(name) == nil {
			httpx.ErrorReq(w, r, http.StatusNotFound, "no configuration schema for plugin: "+name)
			return
		}

		var cfg map[string]any
		if err := jsonpool.DecodeJSON(r.Body, &cfg); err != nil {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "configuration must be a JSON object")
			return
		}
		if cfg == nil {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "configuration must be a JSON object")
			return
		}

		// The resolver ignores a stored value for an operator-only key, so
		// accepting one would show a setting that never takes effect.
		for key := range cfg {
			if core.IsOperatorOnlyKey(key) {
				httpx.ErrorReq(w, r, http.StatusUnprocessableEntity,
					"this setting is set only by the operator, through an environment variable or the configuration file")
				return
			}
		}

		if err := store.Set(r.Context(), name, cfg, actorID(r)); err != nil {
			if errors.Is(err, db.ErrNoSealer) {
				httpx.ErrorReq(w, r, http.StatusPreconditionFailed,
					"cannot store a credential without an encryption key: set ENCRYPTION_KEY")
				return
			}
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to save plugin configuration")
			return
		}
		applyStoredConfig(r.Context(), store, reloader)

		// Read back rather than echoing the request: what was submitted holds
		// credentials in the clear, and the stored form is what the next reader
		// will see.
		saved, err := store.Get(r.Context(), name)
		if err != nil {
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to save plugin configuration")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, saved)
	}
}

// pluginConfigResetHandler drops a plugin's stored configuration, returning it
// to the defaults declared by its schema.
// POST /api/admin/plugins/{name}/config/reset
func pluginConfigResetHandler(store *db.PluginConfigStore, provider PluginSchemaProvider, reloader PluginConfigReloader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		if name == "" {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "plugin name is required")
			return
		}
		if store == nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "plugin configuration store unavailable")
			return
		}
		if err := store.Delete(r.Context(), name); err != nil {
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to reset plugin configuration")
			return
		}
		applyStoredConfig(r.Context(), store, reloader)
		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, schemaDefaults(provider, name))
	}
}

// schemaDefaults extracts the default value of every property a plugin's config
// schema declares. Returns an empty object when the plugin declares no schema.
func schemaDefaults(provider PluginSchemaProvider, name string) map[string]any {
	out := map[string]any{}
	if provider == nil {
		return out
	}
	schema := provider.PluginSchema(name)
	if schema == nil {
		return out
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return out
	}
	for key, raw := range props {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if def, ok := prop["default"]; ok {
			out[key] = def
		}
	}
	return out
}

// applyStoredConfig reinstalls the operator-set layer after a change so the
// running engine serves the new values.
//
// A failure is logged and not returned: the write succeeded, the value is
// stored, and it applies on the next boot regardless. Reporting the save as
// failed would invite an operator to submit it again.
func applyStoredConfig(ctx context.Context, store *db.PluginConfigStore, reloader PluginConfigReloader) {
	if _, err := config.RefreshAdminLayer(ctx, store); err != nil {
		slog.ErrorContext(ctx, "configuration saved but not applied to the running engine, so it takes effect on restart",
			"err", err)
		return
	}
	if reloader == nil {
		return
	}
	// Detached from the request: a plugin rebuilding a client should not be
	// canceled because the operator's browser gave up on the response, and the
	// save has already been committed either way.
	reloaded, errs := reloader.ReloadConfig(context.WithoutCancel(ctx))
	for _, err := range errs {
		slog.ErrorContext(ctx, "plugin kept its previous configuration after a reload failure", "err", err)
	}
	if reloaded > 0 {
		slog.InfoContext(ctx, "plugins reloaded configuration", "count", reloaded)
	}
}
