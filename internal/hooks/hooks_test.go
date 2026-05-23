package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistry(t *testing.T) {
	r := hooks.NewRegistry()
	assert.NotNil(t, r)
}

func TestRegistry_RegisterAndRun(t *testing.T) {
	r := hooks.NewRegistry()

	called := false
	r.Register("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		called = true
		assert.Equal(t, hooks.AfterCreate, e.Type)
		assert.Equal(t, "articles", e.Schema)
		return nil
	})

	ev := hooks.Event{
		Type:   hooks.AfterCreate,
		Schema: "articles",
		Data:   map[string]any{"title": "test"},
	}
	err := r.Run(context.Background(), ev)
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestRegistry_Run_NoMatchingHooks(t *testing.T) {
	r := hooks.NewRegistry()
	ev := hooks.Event{Type: hooks.AfterCreate, Schema: "articles"}
	err := r.Run(context.Background(), ev)
	assert.NoError(t, err)
}

func TestRegistry_Run_StopsOnFirstError(t *testing.T) {
	r := hooks.NewRegistry()

	firstErr := errors.New("first hook error")
	secondCalled := false

	r.Register("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		return firstErr
	})
	r.Register("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		secondCalled = true
		return nil
	})

	ev := hooks.Event{Type: hooks.AfterCreate, Schema: "articles"}
	err := r.Run(context.Background(), ev)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "first hook error")
	assert.False(t, secondCalled, "second hook should not run after error")
}

func TestRegistry_Run_WildcardHooks(t *testing.T) {
	r := hooks.NewRegistry()
	order := make([]string, 0)

	r.Register("*", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		order = append(order, "wildcard")
		return nil
	})
	r.Register("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		order = append(order, "specific")
		return nil
	})

	ev := hooks.Event{Type: hooks.AfterCreate, Schema: "articles"}
	err := r.Run(context.Background(), ev)
	assert.NoError(t, err)
	assert.Equal(t, []string{"wildcard", "specific"}, order)
}

func TestRegistry_RegisterMultipleForSameSchema(t *testing.T) {
	r := hooks.NewRegistry()
	count := 0

	r.Register("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		count++
		return nil
	})
	r.Register("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		count++
		return nil
	})

	err := r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "articles"})
	assert.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestRegistry_Run_AllEventTypes(t *testing.T) {
	types := []hooks.EventType{
		hooks.BeforeCreate, hooks.AfterCreate,
		hooks.BeforeUpdate, hooks.AfterUpdate,
		hooks.BeforeDelete, hooks.AfterDelete,
		hooks.BeforeRequest, hooks.AfterResponse,
	}

	for _, et := range types {
		t.Run(string(et), func(t *testing.T) {
			r := hooks.NewRegistry()
			called := false
			r.Register("*", et, func(ctx context.Context, e hooks.Event) error {
				called = true
				return nil
			})
			err := r.Run(context.Background(), hooks.Event{Type: et, Schema: "test"})
			assert.NoError(t, err)
			assert.True(t, called, "hook for %s should fire", et)
		})
	}
}

func TestRegistry_RegisterDynamic(t *testing.T) {
	r := hooks.NewRegistry()
	called := false

	token := r.RegisterDynamic("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		called = true
		return nil
	})
	assert.NotEmpty(t, token)
	// Verify compound format: must contain exactly one dot with non-empty components
	parts := splitToken(t, token)
	assert.NotEmpty(t, parts[0])
	assert.NotEmpty(t, parts[1])

	err := r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "articles"})
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestRegistry_Unregister(t *testing.T) {
	r := hooks.NewRegistry()
	called := false

	token := r.RegisterDynamic("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		called = true
		return errors.New("should be unregistered")
	})
	r.Unregister(token)

	err := r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "articles"})
	assert.NoError(t, err)
	assert.False(t, called, "unregistered hook should not be called")
}

func TestRegistry_Unregister_WrongOwner(t *testing.T) {
	// Unregister with a mismatched owner must NOT remove the hook.
	r := hooks.NewRegistry()
	called := false

	token := r.RegisterDynamic("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		called = true
		return nil
	})
	parts := splitToken(t, token)
	// Forge a token with the correct public ID but wrong owner.
	forged := parts[0] + ".deadbeef-dead-beef-dead-beefdeadbeef"
	r.Unregister(forged)

	err := r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "articles"})
	assert.NoError(t, err)
	assert.True(t, called, "hook must still fire - wrong owner must not unregister")

	// Now unregister with the correct token.
	called = false
	r.Unregister(token)
	err = r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "articles"})
	assert.NoError(t, err)
	assert.False(t, called, "hook must not fire after correct unregister")
}

func TestRegistry_Unregister_PublicIDOnly(t *testing.T) {
	// Unregister with only the public ID must be ignored.
	r := hooks.NewRegistry()
	called := false

	token := r.RegisterDynamic("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		called = true
		return nil
	})
	parts := splitToken(t, token)

	// Try to unregister with just the public ID (no owner component).
	r.Unregister(parts[0])

	err := r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "articles"})
	assert.NoError(t, err)
	assert.True(t, called, "hook must still fire - public ID alone must not unregister")
}

func TestRegistry_Unregister_GarbageToken(t *testing.T) {
	// Unregister with arbitrary strings must not panic or crash.
	r := hooks.NewRegistry()
	called := false

	_ = r.RegisterDynamic("articles", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		called = true
		return nil
	})

	r.Unregister("")
	r.Unregister(".")
	r.Unregister("..")
	r.Unregister("garbage")
	r.Unregister("a.b.c") // too many dots

	err := r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "articles"})
	assert.NoError(t, err)
	assert.True(t, called, "hook must still fire after garbage token unregisters")
}

func TestRegistry_Unregister_UnknownID(t *testing.T) {
	r := hooks.NewRegistry()
	r.Unregister("00000000-0000-0000-0000-000000000000.00000000-0000-0000-0000-000000000000")
}

// splitToken splits a compound token and asserts it has exactly one dot.
func splitToken(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.SplitN(token, ".", 2)
	assert.Len(t, parts, 2, "token %q must contain exactly one dot", token)
	return parts
}

func TestNewBusHook(t *testing.T) {
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	ev := hooks.Event{
		Type:   hooks.AfterCreate,
		Schema: "articles",
		Data:   map[string]any{"id": 1},
	}
	err := hook(context.Background(), ev)
	require.NoError(t, err)
	assert.Equal(t, 1, pub.called)
	assert.Equal(t, "cms.articles.after_create", pub.lastTopic)
	assert.NotEmpty(t, pub.lastPayload)

	var parsed hooksBusPayloadJSON
	err = json.Unmarshal(pub.lastPayload, &parsed)
	require.NoError(t, err, "payload must be valid JSON")

	assert.Equal(t, "", parsed.TenantID)

	assert.NotEmpty(t, parsed.MessageID, "message_id must be set")
	assert.NotEmpty(t, parsed.CorrelationID, "correlation_id must be set")
	assert.NotEmpty(t, parsed.Timestamp, "timestamp must be set")
	assert.NotEmpty(t, parsed.ProducerID, "producer_id must be set")

	assert.Equal(t, parsed.MessageID, parsed.CorrelationID,
		"correlation_id must equal message_id when no context correlation is set")

	assert.Contains(t, parsed.Timestamp, "T", "timestamp must be RFC3339Nano")
	assert.Contains(t, parsed.Timestamp, "Z", "timestamp must be UTC")
}

func TestNewBusHook_WithTenant(t *testing.T) {
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	ctx := tenant.WithID(context.Background(), "acme-corp")

	ev := hooks.Event{
		Type:   hooks.AfterCreate,
		Schema: "articles",
		Data:   map[string]any{"id": 1},
	}
	err := hook(ctx, ev)
	require.NoError(t, err)
	assert.Equal(t, 1, pub.called)
	assert.Equal(t, "cms.tenant.acme-corp.articles.after_create", pub.lastTopic)

	var parsed hooksBusPayloadJSON
	err = json.Unmarshal(pub.lastPayload, &parsed)
	require.NoError(t, err)
	assert.Equal(t, "acme-corp", parsed.TenantID)

	assert.NotEmpty(t, parsed.MessageID)
	assert.NotEmpty(t, parsed.CorrelationID)
	assert.NotEmpty(t, parsed.Timestamp)
	assert.NotEmpty(t, parsed.ProducerID)
}

func TestNewBusHook_PublisherError_NoAbort(t *testing.T) {
	pub := &fakePublisher{err: errors.New("bus down")}
	hook := hooks.NewBusHook(pub)

	err := hook(context.Background(), hooks.Event{
		Type:   hooks.AfterCreate,
		Schema: "articles",
	})
	assert.NoError(t, err, "bus hook should not abort on publisher error")
	assert.Equal(t, 1, pub.called)
}

func TestKeyFunction(t *testing.T) {
	// key() is internal. Verified via wildcard + specific registration ordering
	r := hooks.NewRegistry()
	order := make([]string, 0)

	r.Register("*", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		order = append(order, "wc:"+e.Schema)
		return nil
	})
	r.Register("specific-schema", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		order = append(order, "sc:"+e.Schema)
		return nil
	})

	r.Run(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "specific-schema"})
	assert.Equal(t, []string{"wc:specific-schema", "sc:specific-schema"}, order)
}

func TestEvent_TenantID_Roundtrip(t *testing.T) {
	// Ensure TenantID added to hooks.Event arrives in the handler.
	r := hooks.NewRegistry()

	var captured string
	r.Register("*", hooks.AfterCreate, func(ctx context.Context, e hooks.Event) error {
		captured = e.TenantID
		return nil
	})

	err := r.Run(context.Background(), hooks.Event{
		Type:     hooks.AfterCreate,
		Schema:   "test",
		TenantID: "acme-corp",
	})
	assert.NoError(t, err)
	assert.Equal(t, "acme-corp", captured)
}

func TestEvent_TenantID_EmptyByDefault(t *testing.T) {
	// The zero value of TenantID is the empty string.
	var ev hooks.Event
	assert.Empty(t, ev.TenantID)
}

// TestRegistry_Run_CooperativeFieldMasking validates the by-design shared-Data
// contract the AfterResponse event documents. Hooks receive the same mutable
// Data map, so wildcard hooks can strip sensitive fields before schema-specific
// hooks see the data. This is cooperative field masking, not a bug.
func TestRegistry_Run_CooperativeFieldMasking(t *testing.T) {
	r := hooks.NewRegistry()
	masked := make(map[string]bool) // tracks what each hook saw

	// Wildcard hook strips "ssn" from Data: runs first by design.
	r.Register("*", hooks.AfterResponse, func(ctx context.Context, e hooks.Event) error {
		if _, ok := e.Data["ssn"]; ok {
			delete(e.Data, "ssn")
			masked["wildcard_saw_ssn"] = true
		}
		return nil
	})

	// Schema-specific hook asserts "ssn" is already gone.
	r.Register("users", hooks.AfterResponse, func(ctx context.Context, e hooks.Event) error {
		if _, ok := e.Data["ssn"]; !ok {
			masked["specific_did_not_see_ssn"] = true
		}
		return nil
	})

	ev := hooks.Event{
		Type:   hooks.AfterResponse,
		Schema: "users",
		Data: map[string]any{
			"name": "Alice",
			"ssn":  "123-45-6789",
		},
	}
	err := r.Run(context.Background(), ev)
	assert.NoError(t, err)
	assert.True(t, masked["wildcard_saw_ssn"], "wildcard should have seen ssn")
	assert.True(t, masked["specific_did_not_see_ssn"], "specific should not see ssn after wildcard stripped it")
}

// hooksBusPayloadJSON mirrors busPayload for JSON unmarshaling in tests.
type hooksBusPayloadJSON struct {
	Schema        string `json:"schema"`
	EventType     string `json:"event_type"`
	TenantID      string `json:"tenant_id"`
	MessageID     string `json:"message_id"`
	CorrelationID string `json:"correlation_id"`
	Timestamp     string `json:"timestamp"`
	ProducerID    string `json:"producer_id"`
}

func TestNewBusHook_ProducerID_Stable(t *testing.T) {
	// ProducerID must be stable across multiple invocations of
	// the same hook (producer identity, not per-message).
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	for i := 0; i < 5; i++ {
		err := hook(context.Background(), hooks.Event{
			Type:   hooks.AfterCreate,
			Schema: "articles",
		})
		require.NoError(t, err)
	}

	producerIDs := make(map[string]int)
	for _, payload := range pub.allPayloads {
		var p hooksBusPayloadJSON
		err := json.Unmarshal(payload, &p)
		require.NoError(t, err)
		producerIDs[p.ProducerID]++
	}
	assert.Len(t, producerIDs, 1, "producer_id must be stable across invocations")
}

func TestNewBusHook_MessageID_Unique(t *testing.T) {
	// Every published message must have a unique MessageID.
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	for i := 0; i < 10; i++ {
		err := hook(context.Background(), hooks.Event{
			Type:   hooks.AfterCreate,
			Schema: "articles",
		})
		require.NoError(t, err)
	}

	messageIDs := make(map[string]bool)
	for _, payload := range pub.allPayloads {
		var p hooksBusPayloadJSON
		err := json.Unmarshal(payload, &p)
		require.NoError(t, err)
		assert.False(t, messageIDs[p.MessageID],
			"message_id %s is duplicated", p.MessageID)
		messageIDs[p.MessageID] = true
	}
	assert.Len(t, messageIDs, 10, "all 10 messages must have unique message_id")
}

func TestNewBusHook_CorrelationID_FromContext(t *testing.T) {
	// When CorrelationIDKey is set in context, it's used as-is.
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	ctx := context.WithValue(context.Background(),
		hooks.CorrelationIDKey{}, "trace-abc-123")

	err := hook(ctx, hooks.Event{
		Type:   hooks.AfterCreate,
		Schema: "articles",
	})
	require.NoError(t, err)

	var p hooksBusPayloadJSON
	err = json.Unmarshal(pub.lastPayload, &p)
	require.NoError(t, err)

	assert.Equal(t, "trace-abc-123", p.CorrelationID,
		"correlation_id must come from context when set")
	assert.NotEmpty(t, p.MessageID,
		"message_id must still be unique")
	assert.NotEqual(t, p.MessageID, p.CorrelationID,
		"correlation_id must differ from message_id when set from context")
}

func TestNewBusHook_CorrelationID_EmptyContextValue(t *testing.T) {
	// An empty-string correlation ID in context is ignored
	// and falls back to message_id.
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	ctx := context.WithValue(context.Background(),
		hooks.CorrelationIDKey{}, "")

	err := hook(ctx, hooks.Event{
		Type:   hooks.AfterCreate,
		Schema: "articles",
	})
	require.NoError(t, err)

	var p hooksBusPayloadJSON
	err = json.Unmarshal(pub.lastPayload, &p)
	require.NoError(t, err)

	assert.Equal(t, p.MessageID, p.CorrelationID,
		"correlation_id must fall back to message_id when context value is empty")
}

func TestNewBusHook_Timestamp_RFC3339Nano(t *testing.T) {
	// Timestamp must parse as time.RFC3339Nano.
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	err := hook(context.Background(), hooks.Event{
		Type:   hooks.AfterCreate,
		Schema: "articles",
	})
	require.NoError(t, err)

	var p hooksBusPayloadJSON
	err = json.Unmarshal(pub.lastPayload, &p)
	require.NoError(t, err)

	_, err = time.Parse(time.RFC3339Nano, p.Timestamp)
	assert.NoError(t, err, "timestamp must parse as RFC3339Nano")
}

func TestNewBusHook_Timestamp_Monotonic(t *testing.T) {
	// Timestamps must be strictly monotonic within a single
	// goroutine (no two events can have the same timestamp if they're
	// fired sequentially, unless clock granularity is too coarse).
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	var last string
	for i := 0; i < 3; i++ {
		err := hook(context.Background(), hooks.Event{
			Type:   hooks.AfterCreate,
			Schema: "articles",
		})
		require.NoError(t, err)
	}

	for _, payload := range pub.allPayloads {
		var p hooksBusPayloadJSON
		json.Unmarshal(payload, &p)
		assert.True(t, last == "" || p.Timestamp >= last,
			"timestamps must be non-decreasing: %s >= %s", p.Timestamp, last)
		last = p.Timestamp
	}
}

func TestNewBusHook_TwoHooks_DifferentProducers(t *testing.T) {
	// Two separate NewBusHook() calls must produce different ProducerIDs.
	pub1 := &fakePublisher{}
	pub2 := &fakePublisher{}
	hook1 := hooks.NewBusHook(pub1)
	hook2 := hooks.NewBusHook(pub2)

	hook1(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "a"})
	hook2(context.Background(), hooks.Event{Type: hooks.AfterCreate, Schema: "b"})

	var p1, p2 hooksBusPayloadJSON
	json.Unmarshal(pub1.lastPayload, &p1)
	json.Unmarshal(pub2.lastPayload, &p2)

	assert.NotEqual(t, p1.ProducerID, p2.ProducerID,
		"different hook instances must have different producer IDs")
}

func TestNewBusHook_Concurrent_UniqueMessageIDs(t *testing.T) {
	// Concurrent publishes must not produce duplicate MessageIDs.
	pub := &fakePublisher{}
	hook := hooks.NewBusHook(pub)

	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hook(context.Background(), hooks.Event{
				Type:   hooks.AfterCreate,
				Schema: "articles",
			})
		}()
	}
	wg.Wait()

	messageIDs := make(map[string]bool)
	pub.mu.Lock()
	for _, payload := range pub.allPayloads {
		var p hooksBusPayloadJSON
		json.Unmarshal(payload, &p)
		assert.False(t, messageIDs[p.MessageID],
			"concurrent publishes must not duplicate message_id")
		messageIDs[p.MessageID] = true
	}
	pub.mu.Unlock()
	assert.Len(t, messageIDs, n, "all %d concurrent messages must have unique message_id", n)
}

// fakePublisher implements bus.Publisher
type fakePublisher struct {
	mu          sync.Mutex
	called      int
	lastTopic   string
	lastPayload []byte
	allPayloads [][]byte
	err         error
}

func (f *fakePublisher) Publish(_ context.Context, topic string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.lastTopic = topic
	f.lastPayload = payload
	f.allPayloads = append(f.allPayloads, payload)
	return f.err
}

func (f *fakePublisher) Close() error { return nil }

// Three handlers land in the right order by chance often enough to hide a
// random order. With eight, one arrangement in 40320 is right.
func TestRunSystem_OrderHoldsAcrossManyHandlers(t *testing.T) {
	r := hooks.NewRegistry()

	const n = 8
	want := make([]string, 0, n)
	var order []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("handler-%d", i)
		want = append(want, name)
		r.RegisterSystem("license.changed", func(context.Context, map[string]any) error {
			order = append(order, name)
			return nil
		})
	}

	if err := r.RunSystem(context.Background(), "license.changed", nil); err != nil {
		t.Fatalf("RunSystem: %v", err)
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("ran %v; want %v", order, want)
	}
}

// The runtime registers its own license.changed handler after every plugin has
// started, and relies on running last: it re-reads each plugin's route table,
// which the plugins' own handlers have just rebuilt. That only holds if system
// handlers fire in registration order.
func TestRunSystem_FiresInRegistrationOrder(t *testing.T) {
	r := hooks.NewRegistry()

	var order []string
	for _, name := range []string{"plugin-a", "plugin-b", "runtime"} {
		n := name
		r.RegisterSystem("license.changed", func(context.Context, map[string]any) error {
			order = append(order, n)
			return nil
		})
	}

	if err := r.RunSystem(context.Background(), "license.changed", nil); err != nil {
		t.Fatalf("RunSystem: %v", err)
	}

	want := []string{"plugin-a", "plugin-b", "runtime"}
	if len(order) != len(want) {
		t.Fatalf("ran %v; want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("ran %v; want %v", order, want)
		}
	}
}
