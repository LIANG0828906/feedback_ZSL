package workpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"iconseg/internal/core"
)

func TestPoolBoundsParallelismAndPreservesOrder(t *testing.T) {
	pool, err := New(2, 8)
	if err != nil {
		t.Fatal(err)
	}
	var active int32
	var maxActive int32
	var completed int32
	tasks := make([]Task, 40)
	for i := range tasks {
		index := i
		tasks[i] = Task{Pixels: 8, Run: func(_ []byte) (any, error) {
			current := atomic.AddInt32(&active, 1)
			for {
				old := atomic.LoadInt32(&maxActive)
				if current <= old || atomic.CompareAndSwapInt32(&maxActive, old, current) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&active, -1)
			atomic.AddInt32(&completed, 1)
			return index, nil
		}}
	}
	results, err := pool.Run(context.Background(), tasks)
	if err != nil {
		t.Fatal(err)
	}
	if completed != int32(len(tasks)) {
		t.Fatalf("completed=%d want=%d", completed, len(tasks))
	}
	if maxActive > 2 {
		t.Fatalf("max parallelism=%d exceeds worker budget", maxActive)
	}
	for i, result := range results {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if result.Value.(int) != i {
			t.Fatalf("result %d = %v", i, result.Value)
		}
	}
}

func TestPoolRejectsTaskLargerThanThreadBuffer(t *testing.T) {
	pool, err := New(2, 8)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Run(context.Background(), []Task{{Pixels: 9, Run: func([]byte) (any, error) {
		return nil, nil
	}}})
	if err == nil {
		t.Fatal("oversized task was accepted")
	}
	app := core.AsAppError(err)
	if app.Code != core.CodePara || app.Field != "per_thread_pixel_buffer" {
		t.Fatalf("error=%+v", app)
	}
}

func TestPoolContextCancellationDoesNotDropSilently(t *testing.T) {
	pool, err := New(1, 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tasks := make([]Task, 10)
	for i := range tasks {
		index := i
		tasks[i] = Task{Pixels: 8, Run: func([]byte) (any, error) {
			if index == 1 {
				cancel()
			}
			time.Sleep(time.Millisecond)
			return index, nil
		}}
	}
	if _, err := pool.Run(ctx, tasks); err == nil {
		t.Fatal("cancellation did not surface an error")
	}
}
