package compliance

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mock erasers

type mockEraser struct {
	mu       sync.Mutex
	callLog  []string // each call records the identifier
	affected int64
	err      error
}

func (m *mockEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callLog = append(m.callLog, identifier)
	return m.affected, m.err
}

// Registry tests

func TestSubjectEraserRegistry_RegisterAndLen(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	tests := []struct {
		name       string
		erasers    []SubjectEraser
		wantLength int
	}{
		{
			name:       "empty registry",
			erasers:    nil,
			wantLength: 0,
		},
		{
			name:       "single eraser",
			erasers:    []SubjectEraser{&mockEraser{affected: 3}},
			wantLength: 1,
		},
		{
			name:       "multiple erasers",
			erasers:    []SubjectEraser{&mockEraser{affected: 1}, &mockEraser{affected: 2}, &mockEraser{affected: 3}},
			wantLength: 3,
		},
		{
			name:       "nil eraser is silently ignored",
			erasers:    []SubjectEraser{nil, &mockEraser{affected: 5}, nil},
			wantLength: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResetSubjectErasers()

			for _, e := range tt.erasers {
				RegisterSubjectEraser(e)
			}

			if got := globalEraserRegistry.Len(); got != tt.wantLength {
				t.Errorf("Len() = %d; want %d", got, tt.wantLength)
			}
		})
	}
}

func TestSubjectEraserRegistry_Erasers_Snapshot(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	e1 := &mockEraser{affected: 1}
	e2 := &mockEraser{affected: 2}
	e3 := &mockEraser{affected: 3}

	RegisterSubjectEraser(e1)
	RegisterSubjectEraser(e2)
	RegisterSubjectEraser(e3)

	// Get a snapshot.
	snapshot := globalEraserRegistry.Erasers()
	if len(snapshot) != 3 {
		t.Fatalf("expected 3 erasers in snapshot, got %d", len(snapshot))
	}

	// Register another after the snapshot.
	RegisterSubjectEraser(&mockEraser{affected: 4})

	// Snapshot should be unchanged (defensive copy).
	if len(snapshot) != 3 {
		t.Fatalf("snapshot length changed from 3 to %d (not a defensive copy)", len(snapshot))
	}

	// Registry should now have 4.
	if len(globalEraserRegistry.Erasers()) != 4 {
		t.Errorf("registry should have 4 erasers after adding, got %d", len(globalEraserRegistry.Erasers()))
	}
}

func TestSubjectEraserRegistry_ConcurrentAccess(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	var wg sync.WaitGroup
	const goroutines = 20

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RegisterSubjectEraser(&mockEraser{affected: 1})
		}()
	}
	wg.Wait()

	if got := globalEraserRegistry.Len(); got != goroutines {
		t.Errorf("expected %d erasers after concurrent registration, got %d", goroutines, got)
	}
}

// Dispatcher tests

func TestRunSubjectErasure_Success(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	RegisterSubjectEraser(&mockEraser{affected: 5})
	RegisterSubjectEraser(&mockEraser{affected: 3})

	total, err := RunSubjectErasure(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 8 {
		t.Errorf("total rows = %d; want 8", total)
	}
}

func TestRunSubjectErasure_NoErasers(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	total, err := RunSubjectErasure(context.Background(), "user@example.com")
	if err != nil {
		t.Fatalf("unexpected error when no erasers registered: %v", err)
	}
	if total != 0 {
		t.Errorf("total rows = %d; want 0", total)
	}
}

func TestRunSubjectErasure_PartialFailure(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	RegisterSubjectEraser(&mockEraser{affected: 5})
	RegisterSubjectEraser(&mockEraser{affected: 0, err: errors.New("db connection failed")})
	RegisterSubjectEraser(&mockEraser{affected: 2})

	total, err := RunSubjectErasure(context.Background(), "user@example.com")

	// Expect partial error.
	if err == nil {
		t.Fatal("expected an error from the failing eraser")
	}
	if err.Error() != "db connection failed" {
		t.Errorf("expected 'db connection failed', got %q", err.Error())
	}

	// Total should include successful erasers only.
	if total != 7 {
		t.Errorf("total rows = %d; want 7 (5 from first, 2 from last)", total)
	}
}

func TestRunSubjectErasure_AllFail(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	RegisterSubjectEraser(&mockEraser{affected: 0, err: errors.New("timeout")})
	RegisterSubjectEraser(&mockEraser{affected: 0, err: errors.New("permission denied")})

	total, err := RunSubjectErasure(context.Background(), "user@example.com")
	if err == nil {
		t.Fatal("expected an error")
	}
	if total != 0 {
		t.Errorf("total rows = %d; want 0", total)
	}
}

func TestRunSubjectErasure_ContextCanceled(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	RegisterSubjectEraser(&mockEraser{affected: 5})

	total, err := RunSubjectErasure(ctx, "user@example.com")
	if total != 0 {
		t.Errorf("total rows = %d; want 0 (context canceled)", total)
	}
	if err == nil {
		t.Fatal("expected context.Canceled error when context is canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// cancelingEraser erases, then cancels, standing in for the client that gives
// up while the fan-out is still working through the registry.
type cancelingEraser struct {
	affected int64
	cancel   context.CancelFunc
}

func (c *cancelingEraser) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	c.cancel()
	return c.affected, nil
}

// An abandoned fan-out has already committed whatever the erasers before the
// cancellation deleted. Reporting zero there says the erasure did nothing while
// the subject is in fact partly erased, which is the one answer an operator
// must not be given.
func TestRunSubjectErasure_CanceledPartwayReportsTheRowsAlreadyErased(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := &mockEraser{affected: 4}
	tail := &mockEraser{affected: 9}
	RegisterSubjectEraser(first)
	RegisterSubjectEraser(&cancelingEraser{affected: 6, cancel: cancel})
	RegisterSubjectEraser(tail)

	total, err := RunSubjectErasure(ctx, "user@example.com")

	require.Error(t, err, "an abandoned erasure must report why it stopped")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(10), total, "the rows the erasers that did run deleted must be reported")
	assert.Empty(t, tail.callLog, "the fan-out must stop rather than run on after cancellation")
}

// Purger registry tests

func TestPurgerRegistry_RegisterAndSnapshot(t *testing.T) {
	ResetPurgers()
	defer ResetPurgers()

	if got := len(globalPurgerRegistry.Purgers()); got != 0 {
		t.Fatalf("expected 0 purgers after reset, got %d", got)
	}

	// Verify nil guard.
	RegisterPurger(nil)
	if got := len(globalPurgerRegistry.Purgers()); got != 0 {
		t.Errorf("expected 0 after registering nil purger, got %d", got)
	}
}

// Table-driven dispatcher test

func TestRunSubjectErasure_TableDriven(t *testing.T) {
	tableErr := errors.New("eraser failed")

	tests := []struct {
		name       string
		erasers    []SubjectEraser
		wantTotal  int64
		wantErrMsg string // empty = no error expected
	}{
		{
			name:      "no erasers",
			erasers:   nil,
			wantTotal: 0,
		},
		{
			name: "single eraser returns rows",
			erasers: []SubjectEraser{
				&mockEraser{affected: 10},
			},
			wantTotal: 10,
		},
		{
			name: "multiple erasers all succeed",
			erasers: []SubjectEraser{
				&mockEraser{affected: 1},
				&mockEraser{affected: 2},
				&mockEraser{affected: 3},
			},
			wantTotal: 6,
		},
		{
			name: "eraser returns zero rows",
			erasers: []SubjectEraser{
				&mockEraser{affected: 0},
				&mockEraser{affected: 5},
			},
			wantTotal: 5,
		},
		{
			name: "first eraser fails",
			erasers: []SubjectEraser{
				&mockEraser{affected: 0, err: tableErr},
				&mockEraser{affected: 3},
			},
			wantTotal:  3,
			wantErrMsg: "eraser failed",
		},
		{
			name: "middle eraser fails, others succeed",
			erasers: []SubjectEraser{
				&mockEraser{affected: 1},
				&mockEraser{affected: 0, err: tableErr},
				&mockEraser{affected: 50},
			},
			wantTotal:  51,
			wantErrMsg: "eraser failed",
		},
		{
			name: "all erasers fail - first error returned",
			erasers: []SubjectEraser{
				&mockEraser{affected: 0, err: errors.New("timeout")},
				&mockEraser{affected: 0, err: errors.New("boom")},
			},
			wantTotal:  0,
			wantErrMsg: "timeout", // first error
		},
		{
			name: "nil eraser registered (should be skipped)",
			erasers: []SubjectEraser{
				&mockEraser{affected: 5},
				nil, // will be silently ignored by RegisterSubjectEraser
				&mockEraser{affected: 3},
			},
			wantTotal: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResetSubjectErasers()
			defer ResetSubjectErasers()

			for _, e := range tt.erasers {
				RegisterSubjectEraser(e)
			}

			total, err := RunSubjectErasure(context.Background(), "test-identifier")
			if total != tt.wantTotal {
				t.Errorf("total = %d; want %d", total, tt.wantTotal)
			}
			if tt.wantErrMsg == "" && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErrMsg != "" {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErrMsg)
				} else if err.Error() != tt.wantErrMsg {
					t.Errorf("error = %q; want %q", err.Error(), tt.wantErrMsg)
				}
			}
		})
	}
}

// Fan-out, idempotency, and coverage

func TestSubjectEraser_RegistryFanOutAndIdempotent(t *testing.T) {
	t.Run("multiple_erasers_all_called", func(t *testing.T) {
		ResetSubjectErasers()
		defer ResetSubjectErasers()

		e1 := &mockEraser{affected: 3}
		e2 := &mockEraser{affected: 5}
		e3 := &mockEraser{affected: 2}
		RegisterSubjectEraser(e1)
		RegisterSubjectEraser(e2)
		RegisterSubjectEraser(e3)

		total, err := RunSubjectErasure(context.Background(), "test-identifier")
		require.NoError(t, err)
		assert.Equal(t, int64(10), total, "total should sum all erasers' affected rows")

		e1.mu.Lock()
		assert.Equal(t, []string{"test-identifier"}, e1.callLog, "eraser 1 should have been called once")
		e1.mu.Unlock()

		e2.mu.Lock()
		assert.Equal(t, []string{"test-identifier"}, e2.callLog, "eraser 2 should have been called once")
		e2.mu.Unlock()

		e3.mu.Lock()
		assert.Equal(t, []string{"test-identifier"}, e3.callLog, "eraser 3 should have been called once")
		e3.mu.Unlock()
	})

	t.Run("idempotent_calls_return_same_total", func(t *testing.T) {
		ResetSubjectErasers()
		defer ResetSubjectErasers()

		e1 := &mockEraser{affected: 4}
		e2 := &mockEraser{affected: 6}
		RegisterSubjectEraser(e1)
		RegisterSubjectEraser(e2)

		// First call
		total1, err1 := RunSubjectErasure(context.Background(), "same-id")
		require.NoError(t, err1)

		// Second call with same identifier
		total2, err2 := RunSubjectErasure(context.Background(), "same-id")
		require.NoError(t, err2)

		assert.Equal(t, total1, total2, "running erasure twice with same identifier should return same total")
		assert.Equal(t, int64(10), total1)
		assert.Equal(t, int64(10), total2)
	})

	t.Run("no_erasers_returns_zero_no_error", func(t *testing.T) {
		ResetSubjectErasers()
		defer ResetSubjectErasers()

		total, err := RunSubjectErasure(context.Background(), "some-identifier")
		require.NoError(t, err)
		assert.Equal(t, int64(0), total, "no erasers should return 0 without error")
	})
}

func TestSubjectEraser_PIIHeavyPluginsHaveEraserCoverage(t *testing.T) {
	ResetSubjectErasers()
	defer ResetSubjectErasers()

	// Register 8 mock erasers to simulate PII-heavy plugins.
	for i := 0; i < 8; i++ {
		RegisterSubjectEraser(&mockEraser{affected: int64(i + 1)})
	}

	// Verify Erasers() snapshot returns all 8.
	erasers := globalEraserRegistry.Erasers()
	assert.Len(t, erasers, 8, "should have 8 registered erasers")

	// Verify each is a concrete *mockEraser.
	for i, e := range erasers {
		_, ok := e.(*mockEraser)
		assert.True(t, ok, "eraser[%d] should be a *mockEraser", i)
	}

	// Verify the snapshot is a defensive copy (not the original slice).
	erasers2 := globalEraserRegistry.Erasers()
	assert.Equal(t, len(erasers), len(erasers2))
	// Modifying one should not affect the other.
	erasers[0] = nil
	assert.NotNil(t, erasers2[0], "modifying a returned Erasers() slice should not corrupt another snapshot")
}
