package terrain

import (
	"context"
	"testing"
	"time"
)

func TestMemoryBudgetQueueReason(t *testing.T) {
	meter := NewBudgetMeter(BudgetConfig{MaxParallel: 4, PerWorkerVertexBuffer: 128, MeshMemoryBudgetBytes: 1000})
	first, err := meter.Acquire(context.Background(), 0, 16, 600)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *Lease, 1)
	go func() {
		lease, err := meter.Acquire(context.Background(), 1, 16, 500)
		if err != nil {
			t.Errorf("acquire: %v", err)
			return
		}
		acquired <- lease
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && meter.Occupancy().Queued == 0 {
		time.Sleep(time.Millisecond * 5)
	}
	if meter.Occupancy().Queued == 0 {
		t.Fatal("memory-budget task did not queue")
	}
	first.Complete(100)
	select {
	case lease := <-acquired:
		lease.Complete(100)
	case <-time.After(time.Second):
		t.Fatal("memory-queued task was not admitted")
	}
	found := false
	for _, event := range meter.Events() {
		if event.State == "queued" && event.Reason == "mesh_memory_budget" && event.BlockNo == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing mesh_memory_budget queue event: %+v", meter.Events())
	}
}
