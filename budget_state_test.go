package bonerig

import (
	"context"
	"testing"
	"time"
)

func TestIntervalStateMachineLegalAndIllegalTransitions(t *testing.T) {
	m := NewIntervalMachine("frames-0000-0004")
	if err := m.Transition(StatePendingRetarget); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateRetargeting); err != nil {
		t.Fatal(err)
	}
	m.SetProgress(2)
	frame := 3
	if err := m.Fail(frame, errorf(CodeQuat, "injected")); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if snap.State != StateFailed || snap.ProgressFrames != 2 || len(snap.FailedFrames) != 1 {
		t.Fatalf("bad failed snapshot: %+v", snap)
	}
	err := m.Transition(StateRetargeting)
	be := asBonerigError(t, err)
	if be.Code != CodeState || be.State != string(StateFailed) {
		t.Fatalf("expected E_STATE from failed->retargeting, got %+v", be)
	}
	if m.GetState() != StateFailed || m.Snapshot().ProgressFrames != 2 {
		t.Fatal("illegal transition changed state/progress")
	}
	if err := m.Transition(StatePendingRetarget); err != nil {
		t.Fatalf("retry should be legal: %v", err)
	}

	committed := NewIntervalMachine("x")
	_ = committed.Transition(StatePendingRetarget)
	_ = committed.Transition(StateRetargeting)
	_ = committed.Transition(StateCommitted)
	err = committed.Transition(StateRetargeting)
	be = asBonerigError(t, err)
	if be.Code != CodeState || committed.GetState() != StateCommitted {
		t.Fatalf("committed mutation not rejected: %+v", be)
	}
}

func TestBoundedJointBufferBlocksAndRecordsWait(t *testing.T) {
	rec := &WaitRecorder{}
	buf := NewBoundedJointBuffer(7, 3, 1, rec)
	ctx := context.Background()
	if err := buf.Push(ctx, JointState{Joint: "a"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- buf.Push(ctx, JointState{Joint: "b"}) }()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("full buffer should block, returned %v", err)
	default:
	}
	first := <-buf.Channel()
	if first.Joint != "a" {
		t.Fatalf("unexpected buffered joint %q", first.Joint)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rec.Count("joint_buffer_full") != 1 {
		t.Fatalf("wait records = %+v", rec.Records())
	}
	if got := rec.Records()[0].WaitedNanoseconds; got <= 0 {
		t.Fatalf("positive wait duration expected, got %d", got)
	}
}

func TestFramePoolParallelismAndFrameQueueWait(t *testing.T) {
	rec := &WaitRecorder{}
	budget := Budget{MaxWorkers: 2, MaxJointBufferPerWorker: 1, MaxPendingFrames: 1}
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	done := make(chan FramePoolMetrics, 1)
	go func() {
		errs, metrics := RunFramePool(context.Background(), budget, []int{0, 1, 2, 3}, rec, func(ctx context.Context, workerID, frame int) *Error {
			started <- struct{}{}
			<-release
			return nil
		})
		for _, err := range errs {
			if err != nil {
				t.Errorf("frame error: %v", err)
			}
		}
		done <- metrics
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(started) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(started) != 2 {
		t.Fatalf("expected two parallel workers, started=%d", len(started))
	}
	close(release)
	metrics := <-done
	if metrics.MaxObservedWorkers != 2 {
		t.Fatalf("max workers = %d", metrics.MaxObservedWorkers)
	}

	rec2 := &WaitRecorder{}
	release2 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		_, _ = RunFramePool(context.Background(), Budget{MaxWorkers: 1, MaxJointBufferPerWorker: 1, MaxPendingFrames: 1}, []int{0, 1, 2}, rec2, func(ctx context.Context, workerID, frame int) *Error {
			<-release2
			return nil
		})
		close(done2)
	}()
	time.Sleep(30 * time.Millisecond)
	close(release2)
	<-done2
	if rec2.Count("frame_queue_full") == 0 {
		t.Fatalf("expected frame queue wait, records=%+v", rec2.Records())
	}
}
