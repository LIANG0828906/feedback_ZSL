package pool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutorCompletesEveryTaskWithSmallBuffer(t *testing.T) {
	ex, err := New(2, 4)
	if err != nil {
		t.Fatal(err)
	}
	var completed atomic.Int64
	var tasks []Task
	for i := 0; i < 100; i++ {
		tasks = append(tasks, Task{ID: "t", Pixels: 4, Run: func(context.Context) error {
			time.Sleep(time.Millisecond)
			completed.Add(1)
			return nil
		}})
	}
	if err := ex.Run(context.Background(), tasks); err != nil {
		t.Fatal(err)
	}
	if completed.Load() != 100 {
		t.Fatalf("completed %d tasks, want 100", completed.Load())
	}
	if got := ex.Stats().PeakWorkers; got > 2 {
		t.Fatalf("peak workers %d exceeds cap 2", got)
	}
}

func TestExecutorRejectsOversizeTask(t *testing.T) {
	ex, _ := New(1, 4)
	err := ex.Run(context.Background(), []Task{{Pixels: 5, Run: func(context.Context) error { return nil }}})
	if err == nil {
		t.Fatal("expected buffer error")
	}
}

func TestSplitPixels(t *testing.T) {
	chunks := SplitPixels(23, 7)
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	if chunks[0] != [2]int{0, 7} || chunks[3] != [2]int{21, 23} {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}
