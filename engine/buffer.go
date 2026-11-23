package engine

import (
	"context"
	"sync"
)

type weightedSemaphore struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int64
	used     int64
	peak     int64
}

func newWeightedSemaphore(capacity int64) *weightedSemaphore {
	semaphore := &weightedSemaphore{capacity: capacity}
	semaphore.cond = sync.NewCond(&semaphore.mu)
	return semaphore
}

func (s *weightedSemaphore) Acquire(ctx context.Context, amount int64) error {
	if amount > s.capacity {
		return context.Canceled
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.used+amount > s.capacity {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.used += amount
	if s.used > s.peak {
		s.peak = s.used
	}
	return nil
}

func (s *weightedSemaphore) Release(amount int64) {
	s.mu.Lock()
	s.used -= amount
	if s.used < 0 {
		s.used = 0
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *weightedSemaphore) Peak() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}
