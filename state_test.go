package loudness

import "testing"

func TestStateMachineTransitionsFreezeAndProgress(t *testing.T) {
	machine, err := NewStateMachine([]int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if committed, total := machine.Progress(); committed != 0 || total != 2 {
		t.Fatalf("initial progress %d/%d", committed, total)
	}
	err = machine.Transition(0, StateCommitted)
	if !HasCode(err, EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.FromState != StatePendingLoad || domainErr.ToState != StateCommitted || domainErr.ClipTotal != 2 {
		t.Fatalf("state diagnostics: %+v", domainErr)
	}
	snapshot, _ := machine.Snapshot(0)
	if snapshot.State != StatePendingLoad || snapshot.Frozen != nil {
		t.Fatalf("illegal transition mutated state: %+v", snapshot)
	}
	if committed, total := machine.Progress(); committed != 0 || total != 2 {
		t.Fatalf("illegal transition mutated progress %d/%d", committed, total)
	}

	if err := machine.Transition(0, StateMeasuring); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(0, StateCalibrating); err != nil {
		t.Fatal(err)
	}
	result := ClipResult{
		ClipID: 0, GainAdjustmentQ12: 4096,
		Frames: []FrameEnvelope{{ClipID: 0, FrameIndex: 0, CalibratedDBQ8: -7680}},
	}
	if err := machine.Commit(0, result); err != nil {
		t.Fatal(err)
	}
	if committed, total := machine.Progress(); committed != 1 || total != 2 {
		t.Fatalf("progress after commit %d/%d", committed, total)
	}
	result.Frames[0].CalibratedDBQ8 = 123
	frozen, _ := machine.Snapshot(0)
	if frozen.State != StateCommitted || frozen.Frozen == nil || frozen.Frozen.Frames[0].CalibratedDBQ8 != -7680 {
		t.Fatalf("committed values were not frozen: %+v", frozen)
	}

	if err := machine.Invalidate(0, "gain edit"); err != nil {
		t.Fatal(err)
	}
	invalidated, _ := machine.Snapshot(0)
	untouched, _ := machine.Snapshot(1)
	if invalidated.State != StatePendingLoad || invalidated.Frozen != nil {
		t.Fatalf("invalidation failed: %+v", invalidated)
	}
	if untouched.State != StatePendingLoad {
		t.Fatalf("unrelated clip changed: %+v", untouched)
	}
	if committed, total := machine.Progress(); committed != 0 || total != 2 {
		t.Fatalf("progress after invalidation %d/%d", committed, total)
	}
}

func TestStateFailureAndRetry(t *testing.T) {
	machine, _ := NewStateMachine([]int{4})
	if err := machine.Transition(4, StateMeasuring); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(4, StateFailed); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := machine.Snapshot(4)
	if snapshot.State != StateFailed {
		t.Fatalf("state = %s", snapshot.State)
	}
	if err := machine.Transition(4, StateCalibrating); !HasCode(err, EState) {
		t.Fatalf("failed->calibrating should be illegal, got %v", err)
	}
	if err := machine.Invalidate(4, "retry"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = machine.Snapshot(4)
	if snapshot.State != StatePendingLoad {
		t.Fatalf("retry state = %s", snapshot.State)
	}
}
