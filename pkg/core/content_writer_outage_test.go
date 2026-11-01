package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A before_create subscriber that cannot read its own store has not refused
// the create, so the writer answers an outage and not a refusal.
func TestPublishBeforeCreate_SubscriberOutageIsNotARefusal(t *testing.T) {
	pub := &recordingPublisher{err: fmt.Errorf("read the guard's rows: %w", ErrServiceUnavailable)}
	err := PublishBeforeCreate(context.Background(), pub, "flow", "posts", uuid.New(), map[string]any{"title": "new"})
	require.ErrorIs(t, err, ErrServiceUnavailable)
	assert.NotErrorIs(t, err, ErrCreateRefused)
	assert.NotErrorIs(t, err, ErrValidation)

	pub.err = assert.AnError
	err = PublishBeforeCreate(context.Background(), pub, "flow", "posts", uuid.New(), nil)
	require.ErrorIs(t, err, ErrCreateRefused)
}
