package pipeline

import (
	"context"
	"testing"
	"time"

	"iconflow/errs"
	"iconflow/model"
)

func TestSchedulerRecordsQueueBudgetWait(t *testing.T) {
	scheduler := NewScheduler(Budget{Workers: 1, MaxPixelsPerThread: 1024, MaxQueuedBlocks: 1})
	release := make(chan struct{})
	blocking := func() (*BlockResult, *JobError) {
		<-release
		return &BlockResult{}, nil
	}
	coord := model.BlockCoord{ImageID: "img", Width: 1, Height: 1}
	first := scheduler.Submit(coord, 1, blocking)
	second := scheduler.Submit(coord, 1, func() (*BlockResult, *JobError) { return &BlockResult{}, nil })

	thirdSubmitted := make(chan struct{})
	go func() {
		third := scheduler.Submit(coord, 1, func() (*BlockResult, *JobError) { return &BlockResult{}, nil })
		close(thirdSubmitted)
		_ = third.Wait(context.Background())
	}()
	time.Sleep(25 * time.Millisecond)
	close(release)
	select {
	case <-thirdSubmitted:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked submit was not admitted after release")
	}
	_ = first.Wait(context.Background())
	_ = second.Wait(context.Background())
	scheduler.Close()

	events := scheduler.Events()
	if len(events) == 0 {
		t.Fatal("expected queue budget wait event")
	}
	if events[0].Reason == "" || events[0].WaitedMS < 0 {
		t.Fatalf("invalid queue event: %+v", events[0])
	}
}

func TestSchedulerEnforcesPixelBuffer(t *testing.T) {
	scheduler := NewScheduler(Budget{Workers: 1, MaxPixelsPerThread: 1, MaxQueuedBlocks: 1})
	coord := model.BlockCoord{ImageID: "img", Width: 2, Height: 2}
	handle := scheduler.Submit(coord, 4, func() (*BlockResult, *JobError) {
		t.Fatal("job should not run when pixel buffer budget is exceeded")
		return nil, nil
	})
	result := handle.Wait(context.Background())
	scheduler.Close()
	if result.Error == nil || result.Error.Err == nil || result.Error.Err.Code != errs.EBudget {
		t.Fatalf("expected E_BUDGET, got %+v", result.Error)
	}
}
