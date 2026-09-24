package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// handlerLimit bounds one handler. A subscriber that blocks holds up every
// later message, so it is given a deadline rather than the transport's whole
// poll.
const handlerLimit = 10 * time.Second

// Relay is the engine's Bus. It holds the subscriptions, delivers to them,
// and carries a broadcast to the other replicas through a Transport.
//
// Subscriptions are taken during boot, before any plugin has started, so the
// relay has to exist before a transport can. Until one is attached the relay
// is a working single-instance bus: a broadcast is checked and then dropped,
// because this process is the only replica and it has already applied the
// change. That is the whole behavior of an install that does not run
// replication, and it is not an error.
type Relay struct {
	instanceID string
	logger     *slog.Logger

	mu        sync.RWMutex
	handlers  map[string][]Handler
	transport Transport
}

// NewRelay returns a relay identified as instanceID, or by a generated id
// when that is empty.
func NewRelay(instanceID string, logger *slog.Logger) *Relay {
	if instanceID == "" {
		instanceID = NewInstanceID()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Relay{instanceID: instanceID, logger: logger, handlers: map[string][]Handler{}}
}

// InstanceID implements Bus.
func (r *Relay) InstanceID() string { return r.instanceID }

// Subscribe implements Bus. A subscription taken before a transport is
// attached receives from the moment it is.
func (r *Relay) Subscribe(topic string, h Handler) {
	if h == nil {
		return
	}
	r.mu.Lock()
	r.handlers[topic] = append(r.handlers[topic], h)
	r.mu.Unlock()
}

// Attach carries broadcasts over t and delivers what t reports, until ctx
// ends. It replaces whatever was attached before.
func (r *Relay) Attach(ctx context.Context, t Transport) {
	if t == nil {
		return
	}
	r.mu.Lock()
	r.transport = t
	r.mu.Unlock()
	go t.Run(ctx, r.deliver)
}

// Attached reports whether a transport is carrying broadcasts. An install
// with none runs single-instance.
func (r *Relay) Attached() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.transport != nil
}

// Broadcast implements Bus. The payload is checked whether or not a transport
// is attached, so a message too large to carry is refused on every install
// rather than only on a replicated one.
func (r *Relay) Broadcast(ctx context.Context, topic string, payload any) error {
	if topic == "" {
		return errors.New("cluster: empty topic")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("cluster: encode payload: %w", err)
	}
	if len(raw) > MaxPayload {
		return ErrPayloadTooLarge
	}
	r.mu.RLock()
	t := r.transport
	r.mu.RUnlock()
	if t == nil {
		return nil
	}
	if err := t.Publish(ctx, Message{Topic: topic, Payload: raw, Origin: r.instanceID}); err != nil {
		return fmt.Errorf("cluster: broadcast %s: %w", topic, err)
	}
	return nil
}

// deliver hands m to every handler on its topic. A message this replica
// published is dropped here rather than in the transport, so the transport
// carries no identity and cannot get the comparison wrong.
func (r *Relay) deliver(ctx context.Context, m Message) {
	if m.Origin == r.instanceID {
		return
	}
	r.mu.RLock()
	hs := append([]Handler(nil), r.handlers[m.Topic]...)
	r.mu.RUnlock()
	for _, h := range hs {
		r.call(ctx, h, m)
	}
}

// call runs one handler under a deadline and contains a panic, so one bad
// subscriber cannot stop every other replica-wide notification.
func (r *Relay) call(ctx context.Context, h Handler, m Message) {
	hctx, cancel := context.WithTimeout(ctx, handlerLimit)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.ErrorContext(ctx, "cluster bus handler panicked", "topic", m.Topic, "panic", rec)
		}
	}()
	h(hctx, m)
}
