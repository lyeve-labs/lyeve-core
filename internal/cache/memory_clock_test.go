package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMemoryCache_ClockInjection(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	mockClock := func() time.Time { return now }

	c := NewMemory[string, int](100, 30*time.Second)
	c.clock = mockClock

	// Set with per-entry TTL.
	err := c.Set(context.Background(), "k", 42, 10*time.Second)
	assert.NoError(t, err)

	// Immediately present.
	v, ok := c.Get(context.Background(), "k")
	assert.True(t, ok)
	assert.Equal(t, 42, v)

	// Advance clock past TTL.
	now = now.Add(15 * time.Second)

	// Entry should be expired.
	v, ok = c.Get(context.Background(), "k")
	assert.False(t, ok, "entry should be expired after clock advance")
	assert.Equal(t, 0, v)

	// Accessing the expired entry should also remove it.
	_, ok = c.Get(context.Background(), "k")
	assert.False(t, ok)
}

func TestMemoryCache_ClockInjection_DefaultTTL(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	mockClock := func() time.Time { return now }

	c := NewMemory[string, int](100, 5*time.Second)
	c.clock = mockClock

	// Set with ttl=0: uses defaultTTL.
	err := c.Set(context.Background(), "k", 99, 0)
	assert.NoError(t, err)

	v, ok := c.Get(context.Background(), "k")
	assert.True(t, ok)
	assert.Equal(t, 99, v)

	// Advance past defaultTTL.
	now = now.Add(10 * time.Second)

	_, ok = c.Get(context.Background(), "k")
	assert.False(t, ok, "entry should be expired after clock advance past defaultTTL")
}

func TestMemoryCache_ClockInjection_NoExpiry(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	mockClock := func() time.Time { return now }

	c := NewMemory[string, int](100, 0) // defaultTTL=0, entry TTL=0
	c.clock = mockClock

	err := c.Set(context.Background(), "forever", 42, 0)
	assert.NoError(t, err)

	// Advance clock arbitrarily far.
	now = now.Add(365 * 24 * time.Hour)

	v, ok := c.Get(context.Background(), "forever")
	assert.True(t, ok, "entry with zero TTL should never expire")
	assert.Equal(t, 42, v)
}
