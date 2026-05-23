package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// settingsSectionName is the engine settings' key in a configuration bundle.
const settingsSectionName = "settings"

// portableSettings are the engine settings a configuration bundle carries.
//
// A setting is on the list when the same value is right on every instance a
// project runs on: how long a token lives, how large a body may be, which
// headers a browser may send and how hard the public routes are limited. A
// setting that names where an instance lives (its origins, its URLs, its
// database) is left out, because copying it from staging to production is the
// mistake a bundle must not make for anyone. Credentials are never on it.
var portableSettings = []string{
	"CORS_ALLOW_HEADERS",
	"CORS_ALLOW_METHODS",
	"CORS_EXPOSE_HEADERS",
	"CORS_MAX_AGE",
	"JWT_EXPIRY_SECS",
	"MAX_BODY_BYTES",
	"MAX_JSON_BODY_BYTES",
	"PUBLIC_RATE_LIMITS",
	"PUBLIC_RATE_LIMIT_GLOBAL",
	"RATE_LIMIT_RPS",
}

// settingsSection moves the portable engine settings an operator saved in the
// admin layer. A value the environment or the configuration file supplies is
// a fact about that instance's deployment, so it is neither exported nor
// overwritten.
type settingsSection struct {
	store    *db.PluginConfigStore
	reloader PluginConfigReloader
	dialect  string
}

// newSettingsSection returns the section, or nil when the engine keeps no
// configuration store.
func newSettingsSection(store *db.PluginConfigStore, reloader PluginConfigReloader, dialect string) core.ConfigSection {
	if store == nil {
		return nil
	}
	return &settingsSection{store: store, reloader: reloader, dialect: dialect}
}

func (s *settingsSection) ConfigSectionName() string { return settingsSectionName }

// ExportConfig writes the portable settings stored in the admin layer as a
// map of key to value.
func (s *settingsSection) ExportConfig(ctx context.Context, _ core.ConfigOptions) (json.RawMessage, error) {
	stored, err := s.store.Get(ctx, config.CoreSettingsOwner)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("export settings: %w", err)
	}
	out := map[string]any{}
	for _, key := range portableSettings {
		if v, ok := stored[key]; ok {
			out[key] = v
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("export settings: %w", err)
	}
	return b, nil
}

// PlanConfig compares the bundle's settings with what this instance stored.
func (s *settingsSection) PlanConfig(ctx context.Context, data json.RawMessage, opts core.ConfigOptions) (core.ConfigPlan, error) {
	want, err := parseSettings(data)
	if err != nil {
		return core.ConfigPlan{}, err
	}
	stored, err := s.store.Get(ctx, config.CoreSettingsOwner)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return core.ConfigPlan{}, fmt.Errorf("plan settings: %w", err)
	}
	return planSettings(want, stored, opts.Prune), nil
}

// ApplyConfig writes the bundle's settings into the admin layer row through
// tx. Every other key the row holds, sealed credentials included, is carried
// over as it was stored.
func (s *settingsSection) ApplyConfig(ctx context.Context, tx core.Querier, data json.RawMessage, opts core.ConfigOptions) (core.ConfigApplied, error) {
	want, err := parseSettings(data)
	if err != nil {
		return core.ConfigApplied{}, err
	}
	tenantID := tenant.ID(ctx)
	stored, err := readSettingsRow(ctx, tx, tenantID)
	if err != nil {
		return core.ConfigApplied{}, &core.ConfigApplyError{Key: settingsSectionName, Err: err}
	}
	plan := planSettings(want, stored, opts.Prune)
	if len(plan.Problems) > 0 {
		return core.ConfigApplied{}, &core.ConfigApplyError{Key: plan.Problems[0].Key, Err: errors.New(plan.Problems[0].Message)}
	}

	changed := false
	for _, c := range plan.Changes {
		switch c.Action {
		case core.ConfigCreate, core.ConfigUpdate:
			if stored == nil {
				stored = map[string]any{}
			}
			stored[c.Key] = want[c.Key]
			changed = true
		case core.ConfigDelete:
			delete(stored, c.Key)
			changed = true
		}
	}
	if !changed {
		return core.ConfigApplied{Changes: plan.Changes}, nil
	}

	b, err := json.Marshal(stored)
	if err != nil {
		return core.ConfigApplied{}, &core.ConfigApplyError{Key: settingsSectionName, Err: err}
	}
	if _, err := tx.Exec(ctx, db.PluginConfigUpsertSQL(s.dialect), tenantID, config.CoreSettingsOwner, string(b), actorIDFromCtx(ctx)); err != nil {
		return core.ConfigApplied{}, &core.ConfigApplyError{Key: settingsSectionName, Err: fmt.Errorf("save settings: %w", err)}
	}
	return core.ConfigApplied{
		Changes: plan.Changes,
		AfterCommit: func(ctx context.Context) {
			applyStoredConfig(ctx, s.store, s.reloader)
		},
	}, nil
}

// parseSettings reads the section as a map of setting names to values.
func parseSettings(data json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%w: settings must be an object of setting names to values", core.ErrConfigUnreadable)
	}
	return out, nil
}

// planSettings is the plan for want against what the admin layer stored.
func planSettings(want, stored map[string]any, prune bool) core.ConfigPlan {
	plan := core.ConfigPlan{Changes: []core.ConfigChange{}}
	resolver := config.ActiveResolver()

	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// A key off the list is refused rather than ignored, so a bundle edited
		// by hand cannot carry a setting the export would never have written.
		if !slices.Contains(portableSettings, key) {
			plan.Problems = append(plan.Problems, core.ConfigProblem{Key: key, Message: "not a setting a bundle carries"})
			continue
		}
		if res := resolver.Resolve(key); !res.Overridable {
			plan.Problems = append(plan.Problems, core.ConfigProblem{Key: key,
				Message: "this instance takes the setting from its environment or configuration file, so a bundle cannot change it: leave it out of the export, or let the admin layer own it here"})
			continue
		}
		cur, ok := stored[key]
		switch {
		case !ok:
			plan.Changes = append(plan.Changes, core.ConfigChange{Key: key, Action: core.ConfigCreate})
		case fmt.Sprint(cur) != fmt.Sprint(want[key]):
			plan.Changes = append(plan.Changes, core.ConfigChange{Key: key, Action: core.ConfigUpdate, Fields: []string{"value"}})
		default:
			plan.Changes = append(plan.Changes, core.ConfigChange{Key: key, Action: core.ConfigUnchanged})
		}
	}
	if prune {
		for _, key := range portableSettings {
			if _, inBundle := want[key]; inBundle {
				continue
			}
			if _, ok := stored[key]; ok {
				plan.Changes = append(plan.Changes, core.ConfigChange{Key: key, Action: core.ConfigDelete})
			}
		}
	}
	return plan
}

// readSettingsRow reads the admin layer row as stored, sealed credentials and
// all, through q.
func readSettingsRow(ctx context.Context, q core.Querier, tenantID string) (map[string]any, error) {
	row, err := q.QueryRow(ctx,
		`SELECT config FROM sys_plugin_config WHERE tenant_id = $1 AND plugin_name = $2`,
		tenantID, config.CoreSettingsOwner)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read settings: %w", err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("read settings: %w", err)
		}
	}
	return out, nil
}
