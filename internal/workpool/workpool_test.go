package workpool

import (
	"context"
	"testing"
	"time"
)

func TestBufferPoolBlocksInsteadOfDropping(t *testing.T) {
	pool := NewBufferPool(100, 25) // four slots
	if pool.Slots() != 4 {
		t.Fatalf("slots = %d", pool.Slots())
	}
	ctx := context.Background()
	if err := pool.Acquire(ctx, 4); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- pool.Acquire(ctx, 1) }()
	select {
	case err := <-acquired:
		t.Fatalf("acquire did not block: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	if pool.Waits() == 0 {
		t.Fatal("waited acquire was not recorded")
	}
	pool.Release(1)
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked acquire did not resume")
	}
	pool.Release(4)
}
