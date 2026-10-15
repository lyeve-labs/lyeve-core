package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// ProviderWindows reads each tenant's provider window: the
// core.ProviderWindowSettingKey of the tenant's engine settings row.
//
// The admin layer applies only the default tenant's row process-wide, so this
// key is read per call for the tenant named, and nothing is cached: a change
// applies to the next password login, reset or magic link.
type ProviderWindows struct {
	pool db.DB
}

var _ core.ProviderWindowProvider = (*ProviderWindows)(nil)

// NewProviderWindows constructs a reader over pool.
func NewProviderWindows(pool db.DB) *ProviderWindows {
	return &ProviderWindows{pool: pool}
}

// ProviderWindowDays implements core.ProviderWindowProvider. A tenant with no
// row, or a row without the key, has the default window. A stored value outside
// the bounds is held to them, so a row written by hand cannot turn the window
// off.
func (p *ProviderWindows) ProviderWindowDays(ctx context.Context, tenantID string) (int, error) {
	row, err := p.pool.QueryRow(ctx,
		`SELECT config FROM sys_plugin_config WHERE tenant_id = $1 AND plugin_name = $2`,
		tenantID, CoreSettingsOwner)
	if err != nil {
		return 0, fmt.Errorf("read provider window: %w", err)
	}
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.ProviderWindowDefaultDays, nil
		}
		return 0, fmt.Errorf("read provider window: %w", err)
	}
	var settings map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return 0, fmt.Errorf("read provider window: %w", err)
		}
	}
	days, ok := settings[core.ProviderWindowSettingKey].(float64)
	if !ok {
		return core.ProviderWindowDefaultDays, nil
	}
	return int(math.Max(0, math.Min(float64(core.ProviderWindowMaxDays), math.Floor(days)))), nil
}

// ProviderWindowAdmitsAccount implements core.ProviderWindowProvider.
func (p *ProviderWindows) ProviderWindowAdmitsAccount(ctx context.Context, tenantID string, seenAt *time.Time) (bool, error) {
	if seenAt == nil {
		return true, nil
	}
	days, err := p.ProviderWindowDays(ctx, tenantID)
	if err != nil {
		return false, err
	}
	return core.ProviderWindowAdmits(seenAt, days, time.Now()), nil
}
