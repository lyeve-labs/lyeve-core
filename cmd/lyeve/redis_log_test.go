package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// A failed dial to an unreachable Redis is the line the client writes at
// boot, and it has to arrive as one JSON object at warning level.
func TestRedisLog_WritesAStructuredWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	redisLog{}.Printf(context.Background(), "redis: connection pool: failed to dial after %d attempts: %v",
		5, errors.New("dial tcp [::1]:6379: connect: connection refused"))

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line), "not one JSON object: %s", buf.String())
	require.Equal(t, "WARN", line["level"])
	require.Equal(t, "redis", line["component"])
	require.Equal(t, "connection pool: failed to dial after 5 attempts: dial tcp [::1]:6379: connect: connection refused", line["msg"])
}
