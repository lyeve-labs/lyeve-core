// Package cluster lets the replicas of one engine tell each other what
// changed. It carries control messages (a cache to flush, a license to apply,
// a schedule to reload), not user traffic: a handful a minute, delivered in
// about a second.
//
// The engine owns the contract and the fan-out to handlers. It owns no
// transport. An engine on its own is therefore one replica, and a broadcast
// with nobody to carry it is dropped after it is checked, because the caller
// has already applied the change locally. A plugin supplies the transport
// through the host role core.ClusterTransportProvider, and the engine hands
// it to the relay once that plugin has started.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Message is one broadcast as another replica sees it.
type Message struct {
	ID        int64
	Topic     string
	Payload   json.RawMessage
	Origin    string
	CreatedAt time.Time
}

// Handler receives a message broadcast by another replica. It runs on the
// transport's delivery goroutine, so it must return promptly.
type Handler func(ctx context.Context, m Message)

// Bus is what a replica uses to reach the others.
type Bus interface {
	// Broadcast records a message for every other replica. The caller has
	// already applied the change locally. Its own replica does not receive it.
	Broadcast(ctx context.Context, topic string, payload any) error
	// Subscribe registers h for messages on topic from other replicas.
	Subscribe(topic string, h Handler)
	// InstanceID names this replica in the origin column.
	InstanceID() string
}

// Transport moves a message between replicas. It is the half the engine does
// not ship: a store every replica of one install can reach, and an order over
// what it holds. The relay keeps the subscriptions, the size limit, the topic
// prefixes and the panic containment, so a transport only has to carry a
// message out and report the ones that arrive.
type Transport interface {
	// Publish records m for the other replicas. The transport assigns the id
	// and the timestamp.
	Publish(ctx context.Context, m Message) error
	// Run delivers what the other replicas published, in the order the
	// transport assigns, until ctx ends. A message this replica published is
	// delivered too, and the relay drops it, so a transport needs no identity
	// of its own.
	Run(ctx context.Context, deliver func(context.Context, Message))
}

// MaxPayload bounds one message. Anything larger belongs in a table the
// message points at.
const MaxPayload = 64 << 10

// ErrPayloadTooLarge is returned by Broadcast for a payload over MaxPayload.
var ErrPayloadTooLarge = errors.New("cluster: payload exceeds 64 KiB")

// NewInstanceID returns hostname-pid-random. The random part keeps two
// containers that share a hostname and a pid apart.
func NewInstanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "lyeve"
	}
	var r [3]byte
	_, _ = rand.Read(r[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(r[:]))
}

// Prefixed returns a Bus whose topics all start with prefix and a dot. The
// engine hands each plugin one named after the plugin, so a plugin can neither
// forge nor receive another's messages.
func Prefixed(inner Bus, prefix string) Bus {
	if inner == nil {
		return nil
	}
	return prefixed{inner: inner, prefix: strings.TrimSuffix(prefix, ".") + "."}
}

type prefixed struct {
	inner  Bus
	prefix string
}

func (p prefixed) Broadcast(ctx context.Context, topic string, payload any) error {
	return p.inner.Broadcast(ctx, p.prefix+topic, payload)
}

func (p prefixed) Subscribe(topic string, h Handler) {
	if h == nil {
		return
	}
	p.inner.Subscribe(p.prefix+topic, func(ctx context.Context, m Message) {
		m.Topic = strings.TrimPrefix(m.Topic, p.prefix)
		h(ctx, m)
	})
}

func (p prefixed) InstanceID() string { return p.inner.InstanceID() }

// Coalescer gathers keys marked within a short window and broadcasts them as
// one message. Content writes invalidate caches on every request. Without it
// each write would cost a second insert.
type Coalescer struct {
	bus    Bus
	topic  string
	window time.Duration
	logger *slog.Logger

	mu    sync.Mutex
	keys  map[string]struct{}
	armed bool
}

// NewCoalescer returns a Coalescer that broadcasts on topic at most once per
// window, with payload {"keys": [...]}.
func NewCoalescer(bus Bus, topic string, window time.Duration) *Coalescer {
	return &Coalescer{bus: bus, topic: topic, window: window, logger: slog.Default(), keys: map[string]struct{}{}}
}

// Mark records key for the next broadcast. It never blocks on the database.
func (c *Coalescer) Mark(key string) {
	if c == nil || c.bus == nil {
		return
	}
	c.mu.Lock()
	c.keys[key] = struct{}{}
	arm := !c.armed
	c.armed = true
	c.mu.Unlock()
	if arm {
		time.AfterFunc(c.window, c.flush)
	}
}

func (c *Coalescer) flush() {
	c.mu.Lock()
	keys := make([]string, 0, len(c.keys))
	for k := range c.keys {
		keys = append(keys, k)
	}
	c.keys = map[string]struct{}{}
	c.armed = false
	c.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	sort.Strings(keys)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.bus.Broadcast(ctx, c.topic, KeysPayload{Keys: keys}); err != nil {
		c.logger.Warn("cluster bus coalesced broadcast failed", "topic", c.topic, "err", err)
	}
}

// KeysPayload is the payload a Coalescer broadcasts.
type KeysPayload struct {
	Keys []string `json:"keys"`
}
