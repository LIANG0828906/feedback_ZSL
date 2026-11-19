package scheduler

import (
	"context"
	"testing"
	"time"

	"feedback_zsl/internal/priority"
)

var (
	high = priority.Class{Name: "high", Rank: 0}
	low  = priority.Class{Name: "low", Rank: 10}
)

func TestHighPriorityGetsNextSlot(t *testing.T) {
	s := New(Config{MaxInflight: 1, MaxWait: 200 * time.Millisecond, AgingPeriod: time.Second})
	defer s.Close()

	blocker, _, err := s.Acquire(context.Background(), low)
	if err != nil {
		t.Fatal(err)
	}
	lowCh := make(chan *Lease, 3)
	for i := 0; i < 3; i++ {
		go func() {
			lease, _, err := s.Acquire(context.Background(), low)
			if err == nil {
				lowCh <- lease
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	highCh := make(chan *Lease, 1)
	go func() {
		lease, _, err := s.Acquire(context.Background(), high)
		if err == nil {
			highCh <- lease
		}
	}()
	time.Sleep(20 * time.Millisecond)
	blocker.Release()

	select {
	case lease := <-highCh:
		lease.Release()
	case lease := <-lowCh:
		lease.Release()
		t.Fatal("a queued low-priority request received the slot before high priority")
	case <-time.After(time.Second):
		t.Fatal("no request received the slot")
	}
}

func TestAgingPreventsLowPriorityStarvation(t *testing.T) {
	s := New(Config{MaxInflight: 1, MaxWait: 50 * time.Millisecond, AgingPeriod: 5 * time.Millisecond})
	defer s.Close()
	blocker, _, err := s.Acquire(context.Background(), high)
	if err != nil {
		t.Fatal(err)
	}
	lowCh := make(chan *Lease, 1)
	go func() {
		lease, _, err := s.Acquire(context.Background(), priority.Class{Name: "low", Rank: 2})
		if err == nil {
			lowCh <- lease
		}
	}()
	time.Sleep(20 * time.Millisecond)
	highCh := make(chan *Lease, 1)
	go func() {
		lease, _, err := s.Acquire(context.Background(), high)
		if err == nil {
			highCh <- lease
		}
	}()
	time.Sleep(10 * time.Millisecond)
	blocker.Release()

	var first *Lease
	select {
	case first = <-lowCh:
	case first = <-highCh:
	case <-time.After(time.Second):
		t.Fatal("no request received the slot")
	}
	first.Release()
	select {
	case lease := <-lowCh:
		lease.Release()
	case lease := <-highCh:
		lease.Release()
	case <-time.After(time.Second):
		t.Fatal("remaining request was starved")
	}
}

func TestCanceledWaiterIsNotDispatched(t *testing.T) {
	s := New(Config{MaxInflight: 1, MaxWait: 100 * time.Millisecond, AgingPeriod: 10 * time.Millisecond})
	defer s.Close()
	blocker, _, err := s.Acquire(context.Background(), low)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Acquire(ctx, high)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled waiter returned success")
	}
	blocker.Release()
	time.Sleep(20 * time.Millisecond)
	if s.Inflight() != 0 {
		t.Fatalf("canceled request consumed a slot: inflight=%d", s.Inflight())
	}
}
