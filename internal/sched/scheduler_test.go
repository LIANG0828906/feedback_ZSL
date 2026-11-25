package sched_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"qkernel/internal/sched"
)

func TestBufferBudgetQueuesWithoutDropping(t *testing.T) {
	s := sched.New(2, 2)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	units := []sched.Unit{
		{ID: "u1", BufferWeight: 3, Work: func() error { started <- struct{}{}; <-release; return nil }},
		{ID: "u2", BufferWeight: 3, Work: func() error { started <- struct{}{}; <-release; return nil }},
	}
	done := make(chan []sched.Result, 1)
	go func() { done <- s.Run(units) }()
	<-started
	time.Sleep(40 * time.Millisecond)
	close(release)
	<-started
	results := <-done
	if len(results) != 2 || results[0].Err != nil || results[1].Err != nil {
		t.Fatalf("units were dropped or failed: %+v", results)
	}
	found := false
	for _, e := range s.Events() {
		if e.Reason == "buffer_exhausted" && e.WaitedMS >= 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected buffer_exhausted queue event, got %+v", s.Events())
	}
}

func TestUnitFailureIsolatesAndCanRetry(t *testing.T) {
	var attempts int32
	s := sched.New(2, 1<<20)
	units := []sched.Unit{
		{ID: "ok", Work: func() error { return nil }},
		{ID: "boom", Work: func() error {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return errors.New("transient")
			}
			return nil
		}},
	}
	res := s.Run(units)
	if res[0].Err != nil || res[1].Err == nil {
		t.Fatalf("unexpected first run: %+v", res)
	}
	retry := s.Run([]sched.Unit{units[1]})
	if retry[0].Err != nil {
		t.Fatalf("retry failed: %v", retry[0].Err)
	}
}
