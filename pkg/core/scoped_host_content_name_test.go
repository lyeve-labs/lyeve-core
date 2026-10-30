package core

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingContentHost is an inner host that records the name each content
// write and publish arrives under.
type recordingContentHost struct {
	stubHost
	mu        sync.Mutex
	published []Event
	upserts   []string
	projects  []string
}

func (h *recordingContentHost) Publish(_ context.Context, e Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.published = append(h.published, e)
	return nil
}

func (h *recordingContentHost) Subscribe(string, EventType, EventHandler) Subscription {
	return stubSubscription{}
}

func (h *recordingContentHost) On(string, SystemEventHandler) Subscription { return stubSubscription{} }

func (h *recordingContentHost) Hooks() HookBus { return h }

func (h *recordingContentHost) HookPublisher() HookPublisher { return h }

func (h *recordingContentHost) UpsertContentAs(ctx context.Context, plugin, _ string, _ uuid.UUID, _ map[string]any) error {
	h.mu.Lock()
	h.upserts = append(h.upserts, plugin)
	h.mu.Unlock()
	return h.Publish(ctx, Event{Type: BeforeCreate, Plugin: plugin})
}

func (h *recordingContentHost) ProjectContentAs(_ context.Context, plugin, _ string, _ uuid.UUID, _ map[string]any) error {
	if plugin != ContentPluginName {
		return ErrCapDenied
	}
	h.mu.Lock()
	h.projects = append(h.projects, plugin)
	h.mu.Unlock()
	return nil
}

// A subscriber trusts Plugin because no plugin can write it: every publisher
// a plugin is handed names that plugin, the one reached through a type
// assertion of Hooks() included, and so does every content write.
func TestScopedPublisher_StampsThePluginName(t *testing.T) {
	inner := &recordingContentHost{}
	flow := NewScopedHost(inner, "flow", CapAll)
	ctx := context.Background()

	require.NoError(t, flow.HookPublisher().Publish(ctx, Event{Type: BeforeCreate, Plugin: ContentPluginName}))
	pub, ok := flow.Hooks().(HookPublisher)
	require.True(t, ok, "the bus a plugin gets no longer publishes")
	require.NoError(t, pub.Publish(ctx, Event{Type: BeforeCreate, Plugin: ContentPluginName}))
	require.NoError(t, flow.UpsertContent(ctx, "pages", uuid.New(), nil))

	// A plugin that wraps its own host under another name still reaches the
	// engine under its own.
	disguised := NewScopedHost(flow, ContentPluginName, CapAll)
	require.NoError(t, disguised.HookPublisher().Publish(ctx, Event{Type: BeforeCreate}))
	require.NoError(t, disguised.UpsertContent(ctx, "pages", uuid.New(), nil))

	inner.mu.Lock()
	defer inner.mu.Unlock()
	require.Len(t, inner.published, 5)
	for i, e := range inner.published {
		assert.Equal(t, "flow", e.Plugin, "event %d", i)
	}
	assert.Equal(t, []string{"flow", "flow"}, inner.upserts)
}

// The projection skips the before_create subscribers, so only the content
// plugin, which asked them when it created the entry, may call it.
func TestProjectContent_OnlyTheContentPlugin(t *testing.T) {
	inner := &recordingContentHost{}
	ctx := context.Background()

	flow := NewScopedHost(inner, "flow", CapAll)
	assert.ErrorIs(t, flow.ProjectContent(ctx, "pages", uuid.New(), nil), ErrCapDenied)

	disguised := NewScopedHost(flow, ContentPluginName, CapAll)
	assert.ErrorIs(t, disguised.ProjectContent(ctx, "pages", uuid.New(), nil), ErrCapDenied)

	content := NewScopedHost(inner, ContentPluginName, CapAll)
	require.NoError(t, content.ProjectContent(ctx, "pages", uuid.New(), nil))

	inner.mu.Lock()
	defer inner.mu.Unlock()
	assert.Equal(t, []string{ContentPluginName}, inner.projects)
	assert.Empty(t, inner.published, "a projection asked the subscribers")
}
