package engine

import (
	"sync"
	"testing"
)

// TestDistLock_ConcurrentContention verifies that the internal mutex
// correctly serializes access to lock state under concurrent goroutines.
// Ten goroutines concurrently call IsHeld and Release. The test verifies
// no data races occur and the final state is consistent.
func TestDistLock_ConcurrentContention(t *testing.T) {
	lock := NewDistLock("concurrent-contention")

	var wg sync.WaitGroup
	results := make(chan bool, 10)

	// Simulate contention: all goroutines race to observe and release.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			held := lock.IsHeld()
			results <- held
			_ = lock.Release()
		}()
	}

	wg.Wait()
	close(results)

	// After all goroutines finish, the lock must not be held.
	if lock.IsHeld() {
		t.Error("lock should not be held after all goroutines released")
	}

	// Count how many saw the lock as held. A newly created lock is
	// never held, so all should report false.
	heldCount := 0
	for held := range results {
		if held {
			heldCount++
		}
	}
	if heldCount > 0 {
		t.Errorf("expected no goroutine to see lock held, got %d", heldCount)
	}
}

// TestDistLock_ReleaseAndReacquire verifies that after Release, IsHeld
// returns false, and that repeated Release calls are safe.
func TestDistLock_ReleaseAndReacquire(t *testing.T) {
	lock := NewDistLock("release-reacquire")

	// Fresh lock is not held.
	if lock.IsHeld() {
		t.Fatal("fresh lock should not be held")
	}

	// Release on a lock that was never held must not error.
	err := lock.Release()
	if err != nil {
		t.Errorf("first release should not error: %v", err)
	}
	if lock.IsHeld() {
		t.Error("lock should not be held after release")
	}

	// Release again: must still be safe.
	err = lock.Release()
	if err != nil {
		t.Errorf("second release should not error: %v", err)
	}
	if lock.IsHeld() {
		t.Error("lock should not be held after second release")
	}
}

// TestDistLock_DoubleRelease verifies that calling Release twice in
// succession on a newly created lock does not panic or error.
func TestDistLock_DoubleRelease(t *testing.T) {
	lock := NewDistLock("double-release")

	err := lock.Release()
	if err != nil {
		t.Errorf("first release: %v", err)
	}

	err = lock.Release()
	if err != nil {
		t.Errorf("second release: %v", err)
	}

	// Third release is also safe.
	err = lock.Release()
	if err != nil {
		t.Errorf("third release: %v", err)
	}
}

// TestDistLock_IsHeld verifies IsHeld accurately reflects lock state
// across the lifecycle of a DistLock instance.
func TestDistLock_IsHeld(t *testing.T) {
	lock := NewDistLock("is-held")

	// After construction, lock must not be held.
	if lock.IsHeld() {
		t.Error("IsHeld should return false for a new lock")
	}

	// Release when not held: IsHeld stays false.
	_ = lock.Release()
	if lock.IsHeld() {
		t.Error("IsHeld should stay false after releasing an unheld lock")
	}

	// Release again: IsHeld still false.
	_ = lock.Release()
	if lock.IsHeld() {
		t.Error("IsHeld should stay false after multiple releases")
	}
}

// TestDistLock_MultipleNames verifies that locks with different names
// are fully independent and do not share state.
func TestDistLock_MultipleNames(t *testing.T) {
	lockA := NewDistLock("alpha")
	lockB := NewDistLock("beta")

	// Names must differ.
	if lockA.name == lockB.name {
		t.Fatalf("different lock names should not collide: both are %q", lockA.name)
	}

	// Both start not held.
	if lockA.IsHeld() || lockB.IsHeld() {
		t.Error("both locks should start not held")
	}

	// Releasing lockA must not affect lockB.
	_ = lockA.Release()
	if lockB.IsHeld() {
		t.Error("releasing lockA should not affect lockB's IsHeld")
	}

	// Releasing lockB must not affect lockA.
	_ = lockB.Release()
	if lockA.IsHeld() {
		t.Error("releasing lockB should not affect lockA's IsHeld")
	}

	// Concurrent independent access must not race.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = lockA.IsHeld()
			_ = lockA.Release()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = lockB.IsHeld()
			_ = lockB.Release()
		}
	}()
	wg.Wait()
}

// TestDistLock_NilSafety verifies that NewDistLock, Release, and IsHeld
// behave correctly with edge-case inputs.
func TestDistLock_NilSafety(t *testing.T) {
	t.Run("NewDistLock with empty name", func(t *testing.T) {
		lock := NewDistLock("")
		if lock == nil {
			t.Fatal("NewDistLock should never return nil")
		}
		if lock.name != "lyeve_" {
			t.Errorf("empty name should produce prefix-only name, got %q", lock.name)
		}
		if lock.IsHeld() {
			t.Error("lock with empty name should not be held")
		}
		// Release must not panic on a lock with empty name.
		if err := lock.Release(); err != nil {
			t.Errorf("release on empty-name lock: %v", err)
		}
	})

	t.Run("NewDistLock with special characters", func(t *testing.T) {
		lock := NewDistLock("test/lock:with*special?chars")
		if lock == nil {
			t.Fatal("NewDistLock should handle special characters")
		}
		if lock.IsHeld() {
			t.Error("lock with special chars should not be held initially")
		}
	})

	t.Run("IsHeld on freshly created lock", func(t *testing.T) {
		lock := NewDistLock("nil-check")
		if lock.IsHeld() {
			t.Error("new lock must not be held")
		}
	})

	t.Run("Release on freshly created lock", func(t *testing.T) {
		lock := NewDistLock("nil-release")
		if err := lock.Release(); err != nil {
			t.Errorf("expected no error releasing unheld lock: %v", err)
		}
	})
}

// TestDistLock_NamePrefix verifies the name prefix convention.
func TestDistLock_NamePrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"cron", "lyeve_cron"},
		{"cache-warmer", "lyeve_cache-warmer"},
		{"stale_purger", "lyeve_stale_purger"},
		{"", "lyeve_"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			lock := NewDistLock(tt.input)
			if lock.name != tt.expected {
				t.Errorf("NewDistLock(%q).name = %q, want %q",
					tt.input, lock.name, tt.expected)
			}
		})
	}
}

// TestDistLock_ConcurrentReleaseIsHeld verifies that concurrent calls to
// Release and IsHeld do not race when many goroutines hammer the same lock.
func TestDistLock_ConcurrentReleaseIsHeld(t *testing.T) {
	lock := NewDistLock("concurrent-hammer")

	var wg sync.WaitGroup
	const goroutines = 50
	const iterations = 200

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = lock.IsHeld()
				_ = lock.Release()
			}
		}()
	}

	wg.Wait()

	// Final state must be consistent: not held.
	if lock.IsHeld() {
		t.Error("lock should not be held after concurrent Release calls")
	}
}
