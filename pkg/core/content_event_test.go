package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The REST handler's shape is the contract: a create carries the new row, an
// update the new row with the old one beside it, a delete the removed row and
// nothing else. Every other writer builds its event here, so the cases below
// are what every writer publishes.
func TestContentEvent_ShapePerAction(t *testing.T) {
	before := map[string]any{"id": "r1", "title": "old"}
	after := map[string]any{"id": "r1", "title": "new"}
	ctx := WithTenantID(context.Background(), "acme")

	cases := []struct {
		name    string
		action  EventType
		before  map[string]any
		after   map[string]any
		want    map[string]any
		wantOld map[string]any
	}{
		{"create carries the new row", AfterCreate, nil, after, after, nil},
		{"create ignores a before the caller passed", AfterCreate, before, after, after, nil},
		{"update carries both rows", AfterUpdate, before, after, after, before},
		{"delete carries the removed row as data", AfterDelete, before, nil, before, nil},
		{"before_delete matches after_delete", BeforeDelete, before, nil, before, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := ContentEvent(ctx, "engine", tc.action, "posts", "r1", tc.before, tc.after)
			assert.Equal(t, tc.action, ev.Type)
			assert.Equal(t, "posts", ev.Schema)
			assert.Equal(t, "r1", ev.RecordID)
			assert.Equal(t, "acme", ev.TenantID)
			assert.Equal(t, "engine", ev.Source)
			assert.Equal(t, tc.want, ev.Data)
			assert.Equal(t, tc.wantOld, ev.OldData)
			assert.False(t, ev.Timestamp.IsZero())
		})
	}
}

// A writer that gets its row back without the id (a multi-row insert, a
// RETURNING clause that projects user columns only) still publishes a row a
// subscriber can address, and the caller's map is not the one that changed.
func TestContentEvent_StampsIDWithoutTouchingCallerMap(t *testing.T) {
	row := map[string]any{"title": "x"}
	ev := ContentEvent(context.Background(), "bulk-import", AfterCreate, "posts", "r9", nil, row)
	assert.Equal(t, "r9", ev.Data["id"])
	_, leaked := row["id"]
	assert.False(t, leaked, "the caller's map must not gain a key")

	same := map[string]any{"id": "keep", "title": "x"}
	ev = ContentEvent(context.Background(), "bulk-import", AfterCreate, "posts", "r9", nil, same)
	assert.Equal(t, "keep", ev.Data["id"], "an id the writer produced wins over the argument")
}

type recordingPublisher struct {
	events []Event
	err    error
}

func (r *recordingPublisher) Publish(_ context.Context, e Event) error {
	r.events = append(r.events, e)
	return r.err
}

func TestPublishContentEvent_NilPublisherPublishesNothing(t *testing.T) {
	require.NoError(t, PublishContentEvent(context.Background(), nil, "engine", AfterCreate, "posts", "r1", nil, nil))
}

func TestPublishContentEvent_WrapsPublisherError(t *testing.T) {
	pub := &recordingPublisher{err: assert.AnError}
	err := PublishContentEvent(context.Background(), pub, "engine", AfterDelete, "posts", "r1", map[string]any{"id": "r1"}, nil)
	require.ErrorIs(t, err, assert.AnError)
	require.Len(t, pub.events, 1)
	assert.Equal(t, AfterDelete, pub.events[0].Type)
	assert.Equal(t, "r1", pub.events[0].RecordID)
}
