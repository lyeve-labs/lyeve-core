package logging

import (
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// LevelMap is an alias for observability.LogLevelMap: internal code uses the
// same LevelManager interface a plugin can provide.
type LevelMap = observability.LogLevelMap

// LevelManager is an alias for observability.LogLevelManager.
type LevelManager = observability.LogLevelManager

// NewLevelMap creates a LevelMap with the given default level.
func NewLevelMap(defaultLevel slog.Level) *LevelMap {
	return observability.NewLogLevelMap(defaultLevel)
}
