package engine

import (
	"context"
	"sync"
)

// semaphore is a counting semaphore whose limit can change while it is held.
//
// A buffered channel cannot do this, because its capacity is fixed at make.
// Tuning the pool at runtime would mean swapping the channel under every
// goroutine blocked on it, which is exactly the kind of swap that loses a
// slot. A mutex and a FIFO of waiters resize in one lock.
//
// Shrinking never evicts a holder. The limit drops, the holders finish at
// their own pace, and no waiter is admitted until held is back under the
// new limit. Growing admits waiters in arrival order immediately.
type semaphore struct {
	mu      sync.Mutex
	limit   int
	held    int
	waiters []chan struct{}
}

func newSemaphore(limit int) *semaphore {
	if limit < 1 {
		limit = 1
	}
	return &semaphore{limit: limit}
}

// Acquire takes a slot, waiting until one is free or ctx is done. A waiter
// that is granted a slot at the same instant its context ends gives the
// slot back and reports the context error, so a slot never leaks to a
// caller that has stopped listening.
func (s *semaphore) Acquire(ctx context.Context) error {
	s.mu.Lock()
	if len(s.waiters) == 0 && s.held < s.limit {
		s.held++
		s.mu.Unlock()
		return nil
	}
	if ctx.Err() != nil {
		s.mu.Unlock()
		return ctx.Err()
	}
	ready := make(chan struct{})
	s.waiters = append(s.waiters, ready)
	s.mu.Unlock()

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		select {
		case <-ready:
			// Granted after the context ended: hand the slot on rather
			// than return it to a caller who will not use it.
			s.held--
			s.admit()
			s.mu.Unlock()
		default:
			for i, w := range s.waiters {
				if w == ready {
					s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
		}
		return ctx.Err()
	}
}

// TryAcquire takes a slot only when one is free right now.
func (s *semaphore) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.waiters) == 0 && s.held < s.limit {
		s.held++
		return true
	}
	return false
}

// Release gives a slot back and admits the oldest waiter when the limit
// allows one.
func (s *semaphore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held > 0 {
		s.held--
	}
	s.admit()
}

// Resize sets a new limit and admits waiters up to it. A limit below one
// is raised to one: a semaphore nobody can acquire is a deadlock, not a
// setting.
func (s *semaphore) Resize(limit int) {
	if limit < 1 {
		limit = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit = limit
	s.admit()
}

// admit grants slots to waiters in arrival order while the limit allows.
// Callers hold mu.
func (s *semaphore) admit() {
	for len(s.waiters) > 0 && s.held < s.limit {
		ready := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.held++
		close(ready)
	}
}

// Limit returns the current cap.
func (s *semaphore) Limit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

// Held returns how many slots are taken. It can exceed Limit briefly after
// a shrink, while holders admitted under the old limit finish.
func (s *semaphore) Held() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// Waiting returns how many callers are blocked in Acquire.
func (s *semaphore) Waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters)
}
