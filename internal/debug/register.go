package debug

import (
	"context"
	"database/sql"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func init() {
	core.DebugQuerierFunc = func(ctx context.Context, q core.Querier, dialect string, rawDB *sql.DB) core.Querier {
		t := TracerFromCtx(ctx)
		if t == nil {
			return q
		}
		return NewQuerier(q, t, dialect, rawDB)
	}
	core.DebugHookBusFunc = func(inner core.HookBus) core.HookBus {
		return NewHookBus(inner)
	}
	core.DebugHookPublisherFunc = func(inner core.HookPublisher) core.HookPublisher {
		return NewHookPublisher(inner)
	}
}
