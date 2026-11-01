package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A writer that joins a transaction announces its write only once the write
// is durable, and a writer outside one announces it at once.
func TestAfterCommit_WaitsForTheCommit(t *testing.T) {
	var ran []string
	AfterCommit(context.Background(), func(context.Context) { ran = append(ran, "outside") })
	assert.Equal(t, []string{"outside"}, ran)

	txCtx, run := WithCommitHooks(context.Background())
	AfterCommit(txCtx, func(context.Context) { ran = append(ran, "first") })
	AfterCommit(txCtx, func(context.Context) { ran = append(ran, "second") })
	assert.Equal(t, []string{"outside"}, ran, "a hook ran before the commit")
	run(context.Background())
	assert.Equal(t, []string{"outside", "first", "second"}, ran)
	run(context.Background())
	assert.Len(t, ran, 3, "a hook ran twice")

	rolledBack, _ := WithCommitHooks(context.Background())
	AfterCommit(rolledBack, func(context.Context) { ran = append(ran, "rolled back") })
	assert.Len(t, ran, 3, "a hook of a transaction that never committed ran")
}
