package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// redisLog sends what the Redis client logs through slog. The client writes
// its connection pool failures to stderr as plain text by default, and every
// other line the engine writes is a JSON object that a log pipeline parses.
type redisLog struct{}

func (redisLog) Printf(ctx context.Context, format string, v ...any) {
	msg := strings.TrimPrefix(fmt.Sprintf(format, v...), "redis: ")
	slog.WarnContext(ctx, msg, "component", "redis")
}
