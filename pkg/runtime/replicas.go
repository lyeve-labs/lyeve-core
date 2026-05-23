package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/cluster"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
)

// Topics the engine itself uses on the instance bus. Plugin topics are
// prefixed with "plugin.<name>." by the host, so they cannot collide.
const topicContentFlush = "core.content.flush"

// broadcastTimeout bounds a broadcast made from a request path. The write it
// follows has already committed, so a slow notice must not hold the response.
const broadcastTimeout = 2 * time.Second

// replicaSync carries what one replica changes to the others: every cache
// the engine keeps in process memory, and, through the bus it hands the
// licensing implementation, the license. It wires its
// subscriptions during boot, long before anything can say whether this
// install runs more than one replica, so the relay it wires them on exists
// from the start and a transport is attached later or never. Never is the
// single-instance install: every method below still runs, and every broadcast
// it makes is dropped because there is nobody to tell.
type replicaSync struct {
	bus    *cluster.Relay
	logger *slog.Logger

	content      atomic.Pointer[db.ContentStore]
	contentFlush *cluster.Coalescer
}

// newReplicaSync builds the relay this process broadcasts and subscribes on.
// instanceID is what the operator set, and an empty one is generated.
func newReplicaSync(instanceID string, logger *slog.Logger) *replicaSync {
	relay := cluster.NewRelay(instanceID, logger)
	return &replicaSync{
		bus:          relay,
		logger:       logger,
		contentFlush: cluster.NewCoalescer(relay, topicContentFlush, 250*time.Millisecond),
	}
}

// attach starts carrying broadcasts over t, which a plugin owns. Polling runs
// on ctx.
func (r *replicaSync) attach(ctx context.Context, t cluster.Transport) {
	r.bus.Attach(ctx, t)
}

// Bus returns the relay.
func (r *replicaSync) Bus() cluster.Bus { return r.bus }

// broadcast sends payload detached from the caller's cancellation. See
// broadcastOn.
func (r *replicaSync) broadcast(ctx context.Context, topic string, payload any) {
	broadcastOn(ctx, r.bus, r.logger, topic, payload)
}

// broadcastOn sends payload on bus detached from the caller's cancellation. A
// failure is logged: the local change stands, and the other replicas converge
// when their TTL lapses. A nil bus is a process with no replicas to tell.
func broadcastOn(ctx context.Context, bus cluster.Bus, logger *slog.Logger, topic string, payload any) {
	if bus == nil {
		return
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), broadcastTimeout)
	defer cancel()
	if err := bus.Broadcast(bctx, topic, payload); err != nil {
		logger.WarnContext(ctx, "instance bus broadcast failed", "topic", topic, "err", err)
	}
}

// wireContent is called with every content store a router build creates. The
// latest is the one reads are served from. The content caches live in process
// memory, so without this a write on one replica would leave the others
// serving the old rows until CACHE_TTL.
func (r *replicaSync) wireContent(cs *db.ContentStore) {
	if cs == nil {
		return
	}
	first := r.content.Swap(cs) == nil
	cs.OnInvalidate(func(_ context.Context, schemaName string) { r.contentFlush.Mark(schemaName) })
	if first {
		r.bus.Subscribe(topicContentFlush, func(ctx context.Context, _ cluster.Message) {
			if cur := r.content.Load(); cur != nil {
				cur.FlushCaches(ctx)
			}
		})
	}
}

// wireQueryCache makes a plugin's InvalidateCache reach every replica's query
// cache. The host sends. This subscribes.
func (r *replicaSync) wireQueryCache(host any) {
	local, ok := host.(interface{ InvalidateCacheLocal(string) int })
	if !ok {
		return
	}
	r.bus.Subscribe(enginehost.TopicQueryCacheInvalidate, func(_ context.Context, m cluster.Message) {
		var p cluster.KeysPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			r.logger.Warn("instance bus: unreadable query cache invalidation", "err", err)
			return
		}
		for _, k := range p.Keys {
			local.InvalidateCacheLocal(k)
		}
	})
}
