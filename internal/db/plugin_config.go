package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
)

// PluginConfigStore persists operator-set plugin configuration.
//
// sys_plugin_config lives in the public schema and is not replicated per
// tenant, so tenant_id on every query is the isolation boundary.
type PluginConfigStore struct {
	pool   DB
	sealer ConfigSealer
}

// NewPluginConfigStore constructs a store backed by the given pool.
func NewPluginConfigStore(pool DB) *PluginConfigStore {
	return &PluginConfigStore{pool: pool}
}

// WithSealer attaches the key store that encrypts credentials at rest. Without
// one the store still serves ordinary settings and refuses to save credentials,
// rather than writing them to a table in plain text.
func (s *PluginConfigStore) WithSealer(sealer ConfigSealer) *PluginConfigStore {
	s.sealer = sealer
	return s
}

// PluginConfigRow is one plugin's stored configuration, with its tenant, as
// returned by ListAll.
type PluginConfigRow struct {
	TenantID   string
	PluginName string
	Config     map[string]any

	// UndecryptableKeys names credentials that could not be decrypted, usually
	// because ENCRYPTION_KEY changed since they were saved. They are absent
	// from Config: a plugin handed ciphertext as though it were an API key
	// fails at the far end of an integration with nothing pointing back here.
	UndecryptableKeys []string
}

// Get returns the stored configuration for a plugin with every credential
// replaced by SecretMask, or domain.ErrNotFound when the operator has never
// saved one.
//
// Masking here rather than at the handler is deliberate: this is the method the
// admin API serves, and a credential that is only redacted at the edge is one
// refactor away from being disclosed.
func (s *PluginConfigStore) Get(ctx context.Context, pluginName string) (map[string]any, error) {
	cfg, err := s.getRaw(ctx, pluginName)
	if err != nil {
		return nil, err
	}
	return maskValues(cfg), nil
}

// GetResolved returns the stored configuration with credentials decrypted. It
// feeds the configuration layer that serves plugins. It must never back an HTTP
// response.
func (s *PluginConfigStore) GetResolved(ctx context.Context, pluginName string) (map[string]any, error) {
	cfg, err := s.getRaw(ctx, pluginName)
	if err != nil {
		return nil, err
	}
	out, _ := unsealValues(s.sealer, tenant.ID(ctx), cfg)
	return out, nil
}

func (s *PluginConfigStore) getRaw(ctx context.Context, pluginName string) (map[string]any, error) {
	var raw jsonColumn
	row, qErr := s.pool.QueryRow(ctx,
		`SELECT config FROM sys_plugin_config WHERE tenant_id = $1 AND plugin_name = $2`,
		tenant.ID(ctx), pluginName)
	if qErr != nil {
		return nil, fmt.Errorf("get plugin config %q: %w", pluginName, qErr)
	}
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get plugin config %q: %w", pluginName, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw.raw, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal plugin config %q: %w", pluginName, err)
	}
	return cfg, nil
}

// Set replaces the stored configuration for a plugin, encrypting any
// credential it carries. updatedBy may be nil.
//
// A credential submitted as SecretMask keeps the value already stored, so a
// client that reads a configuration, edits one field and submits the whole
// object does not overwrite credentials with the mask it was shown.
func (s *PluginConfigStore) Set(ctx context.Context, pluginName string, cfg map[string]any, updatedBy *uuid.UUID) error {
	tenantID := tenant.ID(ctx)
	sealed, preserve, err := sealValues(s.sealer, tenantID, cfg)
	if err != nil {
		return err
	}
	if len(preserve) > 0 {
		existing, err := s.getRaw(ctx, pluginName)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		for _, key := range preserve {
			if v, ok := existing[key]; ok {
				sealed[key] = v
			}
		}
	}

	b, err := json.Marshal(sealed)
	if err != nil {
		return fmt.Errorf("marshal plugin config %q: %w", pluginName, err)
	}
	_, err = s.pool.Exec(ctx, pluginConfigUpsertQuery(s.pool.Engine()),
		tenant.ID(ctx), pluginName, string(b), updatedBy)
	if err != nil {
		return fmt.Errorf("save plugin config %q: %w", pluginName, err)
	}
	return nil
}

// Delete removes a plugin's stored configuration, returning it to schema
// defaults. Deleting an absent row is not an error.
func (s *PluginConfigStore) Delete(ctx context.Context, pluginName string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM sys_plugin_config WHERE tenant_id = $1 AND plugin_name = $2`,
		tenant.ID(ctx), pluginName)
	if err != nil {
		return fmt.Errorf("delete plugin config %q: %w", pluginName, err)
	}
	return nil
}

// ListAll returns every tenant's stored configuration with credentials
// decrypted. Used to build the process-wide configuration layer. Not
// tenant-scoped by design, and never a response body.
func (s *PluginConfigStore) ListAll(ctx context.Context) ([]PluginConfigRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, plugin_name, config FROM sys_plugin_config ORDER BY plugin_name`)
	if err != nil {
		return nil, fmt.Errorf("list plugin config: %w", err)
	}
	defer rows.Close()

	var out []PluginConfigRow
	for rows.Next() {
		var r PluginConfigRow
		var raw jsonColumn
		if err := rows.Scan(&r.TenantID, &r.PluginName, &raw); err != nil {
			return nil, fmt.Errorf("scan plugin config: %w", err)
		}
		var stored map[string]any
		if err := json.Unmarshal(raw.raw, &stored); err != nil {
			return nil, fmt.Errorf("unmarshal plugin config %q: %w", r.PluginName, err)
		}
		r.Config, r.UndecryptableKeys = unsealValues(s.sealer, r.TenantID, stored)
		out = append(out, r)
	}
	return out, rows.Err()
}

func pluginConfigUpsertQuery(engine string) string {
	switch engine {
	case "mysql":
		return `INSERT INTO sys_plugin_config (tenant_id, plugin_name, config, updated_at, updated_by)
		 VALUES ($1, $2, $3, NOW(6), $4)
		 ON DUPLICATE KEY UPDATE config = VALUES(config), updated_at = NOW(6), updated_by = VALUES(updated_by)`
	case "mssql":
		return `MERGE sys_plugin_config WITH (HOLDLOCK) AS target
		 USING (SELECT $1 AS tenant_id, $2 AS plugin_name, $3 AS config, $4 AS updated_by) AS source
		 ON target.tenant_id = source.tenant_id AND target.plugin_name = source.plugin_name
		 WHEN MATCHED THEN UPDATE SET config = source.config, updated_at = SYSUTCDATETIME(), updated_by = source.updated_by
		 WHEN NOT MATCHED THEN INSERT (tenant_id, plugin_name, config, updated_at, updated_by)
		 VALUES (source.tenant_id, source.plugin_name, source.config, SYSUTCDATETIME(), source.updated_by);`
	default:
		return `INSERT INTO sys_plugin_config (tenant_id, plugin_name, config, updated_at, updated_by)
		 VALUES ($1, $2, $3, NOW(), $4)
		 ON CONFLICT (tenant_id, plugin_name) DO UPDATE
		   SET config = EXCLUDED.config, updated_at = NOW(), updated_by = EXCLUDED.updated_by`
	}
}

// PluginConfigUpsertSQL is the statement Set writes a row with, for a caller
// that writes the row inside a transaction of its own. Its arguments are
// tenant_id, plugin_name, the config as JSON text and updated_by.
func PluginConfigUpsertSQL(engine string) string { return pluginConfigUpsertQuery(engine) }
