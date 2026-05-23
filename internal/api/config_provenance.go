package api

import (
	"errors"
	"net/http"
	"sort"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// Configuration provenance.
//
// A setting can arrive from an environment variable, the YAML tree, or the
// admin UI, and only the last of those is editable here. Without a way to ask
// which, the UI has to offer every field as editable and let the operator
// discover by experiment that the value they saved is being overridden. This
// endpoint answers where each setting comes from so a pinned one can be shown
// read-only with its origin named.

// configSettingResponse is one setting and where it came from.
type configSettingResponse struct {
	Key    string `json:"key"`
	Source string `json:"source"`

	// Value is omitted for credentials, which are never disclosed.
	Value string `json:"value,omitempty"`

	// Origin locates a file-sourced setting as "lyeve.yaml:42".
	Origin string `json:"origin,omitempty"`

	// Editable reports whether saving this key through the admin API will have
	// any effect.
	Editable bool `json:"editable"`

	// Secret marks a key whose value is withheld, so the UI renders a
	// write-only field rather than an empty one.
	Secret bool `json:"secret,omitempty"`

	// Description says what the setting does, and Default what applies when
	// no layer sets it. Both are empty for a key the engine does not describe.
	Description string `json:"description,omitempty"`
	Default     string `json:"default,omitempty"`
}

type configProvenanceResponse struct {
	Settings []configSettingResponse `json:"settings"`
	Counts   map[string]int          `json:"counts"`

	// Suppressed names settings a lower layer supplies that an empty
	// environment variable pins to empty. It is usually a leftover .env entry.
	Suppressed []string `json:"suppressed,omitempty"`
}

// configProvenanceHandler reports where every known setting comes from.
// GET /api/admin/config
func configProvenanceHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resolver := config.ActiveResolver()

		resolutions := resolver.Provenance()
		out := configProvenanceResponse{
			Settings: make([]configSettingResponse, 0, len(resolutions)),
			Counts:   map[string]int{},
		}
		for _, res := range resolutions {
			item := settingResponse(res)
			out.Settings = append(out.Settings, item)
			out.Counts[item.Source]++
		}
		out.Suppressed = resolver.BlankedKeys()

		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, out)
	}
}

// settingResponse renders one resolution, withholding the value of a
// credential so a read never discloses one.
func settingResponse(res config.Resolution) configSettingResponse {
	secret := core.IsSecretKey(res.Key)
	item := configSettingResponse{
		Key:      res.Key,
		Source:   res.From.String(),
		Origin:   res.Origin,
		Editable: res.Overridable,
		Secret:   secret,
	}
	if !secret {
		item.Value = res.Value
	}
	if doc, ok := config.DescribeSetting(res.Key); ok {
		item.Description = doc.Description
		item.Default = doc.Default
	}
	return item
}

// configSaveRequest is a set of engine settings to store in the admin layer.
type configSaveRequest struct {
	Values map[string]any `json:"values"`
}

// configSaveHandler stores engine settings in the admin layer.
// PUT /api/admin/config
//
// Only a key the resolver reports as editable is accepted. A key the
// environment sets, or one pinned by the configuration file without the
// !overridable tag, is refused and named in the response: storing it would
// leave the operator looking at a saved value the engine never reads.
func configSaveHandler(store *db.PluginConfigStore, reloader PluginConfigReloader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "configuration store unavailable")
			return
		}

		var req configSaveRequest
		if err := jsonpool.DecodeJSON(r.Body, &req); err != nil || req.Values == nil {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "body must be a JSON object with a values field")
			return
		}
		if len(req.Values) == 0 {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "no settings submitted")
			return
		}

		resolver := config.ActiveResolver()
		accepted := make(map[string]any, len(req.Values))
		var refused []configRefusal
		for key, value := range req.Values {
			res := resolver.Resolve(key)
			if res.Key == "" {
				refused = append(refused, configRefusal{Key: key, Reason: "not a settable name"})
				continue
			}
			if !res.Overridable {
				refused = append(refused, configRefusal{
					Key:    res.Key,
					Reason: overrideRefusalReason(res),
					Origin: res.Origin,
				})
				continue
			}
			accepted[res.Key] = value
		}
		if len(accepted) == 0 {
			w.Header().Set("Cache-Control", "no-store")
			respond(w, http.StatusConflict, configSaveResponse{Refused: refused})
			return
		}

		// Merge onto what is already stored rather than replacing it: a save
		// carries the fields one screen edited, and Set writes the whole row.
		// A credential reads back as the mask, and submitting the mask leaves
		// the stored value alone, so untouched secrets survive the round trip.
		merged, err := store.Get(r.Context(), config.CoreSettingsOwner)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to load stored settings")
			return
		}
		if merged == nil {
			merged = map[string]any{}
		}
		for key, value := range accepted {
			merged[key] = value
		}

		if err := store.Set(r.Context(), config.CoreSettingsOwner, merged, actorID(r)); err != nil {
			if errors.Is(err, db.ErrNoSealer) {
				httpx.ErrorReq(w, r, http.StatusPreconditionFailed,
					"cannot store a credential without an encryption key: set ENCRYPTION_KEY")
				return
			}
			httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to save settings")
			return
		}
		applyStoredConfig(r.Context(), store, reloader)

		// Report where each saved key now resolves from. A key that still reads
		// from a higher layer would otherwise look applied.
		out := configSaveResponse{Refused: refused}
		for key := range accepted {
			out.Saved = append(out.Saved, settingResponse(config.ActiveResolver().Resolve(key)))
		}
		sort.Slice(out.Saved, func(i, j int) bool { return out.Saved[i].Key < out.Saved[j].Key })

		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, out)
	}
}

// configRefusal names a key that was not stored, and why.
type configRefusal struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
	Origin string `json:"origin,omitempty"`
}

type configSaveResponse struct {
	Saved   []configSettingResponse `json:"saved,omitempty"`
	Refused []configRefusal         `json:"refused,omitempty"`
}

// overrideRefusalReason explains which layer holds a key the admin layer may
// not set, in the operator's terms rather than the resolver's.
func overrideRefusalReason(res config.Resolution) string {
	if res.OperatorOnly {
		return "set only by the operator, through an environment variable or the configuration file"
	}
	if res.From == config.SourceEnv {
		// Name the way out: the opt-in and where it is declared.
		return "set by an environment variable. Name it in " + config.OverridableEnvKey + " to edit it here"
	}
	return "pinned by the configuration file. Add the !overridable tag to edit it here"
}
