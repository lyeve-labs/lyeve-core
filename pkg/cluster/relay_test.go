package cluster_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/cluster"
)

// board is one in-memory notice board two relays share. The engine ships no
// transport, so a test of what the relay does with one brings its own.
// Delivery is explicit: nothing reaches a relay until the test calls poll,
// which is where a real transport's ticker would be.
type board struct {
	mu   sync.Mutex
	msgs []cluster.Message
	next int64
}

func (b *board) add(m cluster.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	m.ID = b.next
	m.CreatedAt = time.Now()
	b.msgs = append(b.msgs, m)
}

func (b *board) from(cursor int) []cluster.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cursor >= len(b.msgs) {
		return nil
	}
	out := make([]cluster.Message, len(b.msgs)-cursor)
	copy(out, b.msgs[cursor:])
	return out
}

type memTransport struct {
	board *board

	mu      sync.Mutex
	cursor  int
	deliver func(context.Context, cluster.Message)
	ready   chan struct{}
}

func newMemTransport(b *board) *memTransport {
	return &memTransport{board: b, ready: make(chan struct{})}
}

func (t *memTransport) Publish(_ context.Context, m cluster.Message) error {
	t.board.add(m)
	return nil
}

func (t *memTransport) Run(ctx context.Context, deliver func(context.Context, cluster.Message)) {
	t.mu.Lock()
	t.deliver = deliver
	t.mu.Unlock()
	close(t.ready)
	<-ctx.Done()
}

// poll hands the relay everything published since the last call.
func (t *memTransport) poll(ctx context.Context) {
	<-t.ready
	t.mu.Lock()
	cursor := t.cursor
	deliver := t.deliver
	t.mu.Unlock()
	msgs := t.board.from(cursor)
	t.mu.Lock()
	t.cursor = cursor + len(msgs)
	t.mu.Unlock()
	for _, m := range msgs {
		deliver(ctx, m)
	}
}

// replicaOn returns a relay named id attached to its own transport over b.
func replicaOn(t *testing.T, ctx context.Context, b *board, id string) (*cluster.Relay, *memTransport) {
	t.Helper()
	r := cluster.NewRelay(id, nil)
	tr := newMemTransport(b)
	r.Attach(ctx, tr)
	return r, tr
}

// recorder collects what a handler received.
type recorder struct {
	mu   sync.Mutex
	msgs []cluster.Message
}

func (r *recorder) handle(_ context.Context, m cluster.Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
}

func (r *recorder) topics() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.msgs))
	for _, m := range r.msgs {
		out = append(out, m.Topic)
	}
	return out
}

func TestRelay_DeliversToTheOtherReplicasOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shared := &board{}
	a, ta := replicaOn(t, ctx, shared, "replica-a")
	b, tb := replicaOn(t, ctx, shared, "replica-b")
	var gotA, gotB recorder
	a.Subscribe("perm.flush", gotA.handle)
	b.Subscribe("perm.flush", gotB.handle)

	if err := a.Broadcast(ctx, "perm.flush", map[string]string{"tenant": "t1"}); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	ta.poll(ctx)
	tb.poll(ctx)

	if n := len(gotA.topics()); n != 0 {
		t.Errorf("the broadcasting replica received its own message %d times", n)
	}
	if got := gotB.topics(); len(got) != 1 {
		t.Fatalf("replica b received %v, want one message", got)
	}
	m := gotB.msgs[0]
	if m.Origin != "replica-a" {
		t.Errorf("origin = %q", m.Origin)
	}
	var p map[string]string
	if err := json.Unmarshal(m.Payload, &p); err != nil || p["tenant"] != "t1" {
		t.Errorf("payload = %s (%v)", m.Payload, err)
	}
}

func TestRelay_PluginTopicsStayApart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shared := &board{}
	a, _ := replicaOn(t, ctx, shared, "replica-a")
	b, tb := replicaOn(t, ctx, shared, "replica-b")
	var same, other recorder
	cluster.Prefixed(b, "plugin.cron").Subscribe("reload", same.handle)
	cluster.Prefixed(b, "plugin.webhook").Subscribe("reload", other.handle)

	if err := cluster.Prefixed(a, "plugin.cron").Broadcast(ctx, "reload", nil); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	tb.poll(ctx)
	if topics := same.topics(); len(topics) != 1 || topics[0] != "reload" {
		t.Errorf("the same plugin on the other replica got %v, want [reload]", topics)
	}
	if topics := other.topics(); len(topics) != 0 {
		t.Errorf("another plugin received %v", topics)
	}
}

func TestRelay_HandlerPanicDoesNotStopDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shared := &board{}
	a, _ := replicaOn(t, ctx, shared, "replica-a")
	b, tb := replicaOn(t, ctx, shared, "replica-b")
	var got recorder
	b.Subscribe("t", func(context.Context, cluster.Message) { panic("subscriber bug") })
	b.Subscribe("t", got.handle)
	if err := a.Broadcast(ctx, "t", nil); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	tb.poll(ctx)
	if len(got.topics()) != 1 {
		t.Error("a panicking handler kept the next handler from running")
	}
}

func TestRelay_RefusesAnOversizedPayload(t *testing.T) {
	r := cluster.NewRelay("x", nil)
	err := r.Broadcast(context.Background(), "t", strings.Repeat("a", cluster.MaxPayload))
	if !errors.Is(err, cluster.ErrPayloadTooLarge) {
		t.Errorf("err = %v, want ErrPayloadTooLarge", err)
	}
}

// An install that runs no replication has no transport. It is the whole
// behavior of a single instance: a broadcast is checked, found to have
// nowhere to go, and dropped, and nothing logs or fails.
func TestRelay_WithoutATransportDropsWhatItCannotCarry(t *testing.T) {
	r := cluster.NewRelay("", nil)
	if r.Attached() {
		t.Fatal("a relay with no transport reports one")
	}
	if r.InstanceID() == "" {
		t.Error("a relay with no transport still names this replica")
	}
	var got recorder
	r.Subscribe("t", got.handle)
	if err := r.Broadcast(context.Background(), "t", map[string]string{"a": "b"}); err != nil {
		t.Errorf("broadcast on a single instance = %v, want no error", err)
	}
	if n := len(got.topics()); n != 0 {
		t.Errorf("a single instance delivered %d of its own messages back to itself", n)
	}
}

// A subscription taken during boot, before any plugin could supply a
// transport, receives once one is attached. Every engine subscription is
// taken that way, so this is the ordering the whole arrangement rests on.
func TestRelay_SubscriptionTakenBeforeAttachStillReceives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shared := &board{}
	sender, _ := replicaOn(t, ctx, shared, "replica-a")

	late := cluster.NewRelay("replica-b", nil)
	var got recorder
	late.Subscribe("t", got.handle)

	if err := sender.Broadcast(ctx, "t", nil); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	tr := newMemTransport(shared)
	late.Attach(ctx, tr)
	tr.poll(ctx)

	if topics := got.topics(); len(topics) != 1 || topics[0] != "t" {
		t.Errorf("a subscription taken before the transport got %v, want [t]", topics)
	}
}

// fakeBus records broadcasts for the Coalescer tests.
type fakeBus struct {
	mu   sync.Mutex
	sent []cluster.KeysPayload
}

func (f *fakeBus) Broadcast(_ context.Context, _ string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, payload.(cluster.KeysPayload))
	return nil
}
func (f *fakeBus) Subscribe(string, cluster.Handler) {}
func (f *fakeBus) InstanceID() string                { return "fake" }

func (f *fakeBus) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func TestCoalescer_SendsOneMessagePerWindow(t *testing.T) {
	f := &fakeBus{}
	c := cluster.NewCoalescer(f, "content", 20*time.Millisecond)
	for i := 0; i < 50; i++ {
		c.Mark("posts")
		c.Mark("pages")
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if f.count() != 1 {
		t.Fatalf("broadcasts = %d, want 1", f.count())
	}
	if got := strings.Join(f.sent[0].Keys, ","); got != "pages,posts" {
		t.Errorf("keys = %s", got)
	}
}

// lyeve:no-assert fails by panicking. A nil Coalescer has nothing to compare
func TestCoalescer_NilIsInert(t *testing.T) {
	var c *cluster.Coalescer
	c.Mark("x")
}
