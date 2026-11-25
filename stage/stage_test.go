package stage_test

import (
	"testing"

	"lipsync/domain"
	"lipsync/stage"
)

func TestStateMachineRejectsIllegalTransitionWithoutMutation(t *testing.T) {
	m := stage.New(4, "d04")
	if err := m.Transition(stage.Quantizing, 10); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot()
	err := m.Transition(stage.Committed, 100)
	if domain.ErrorCode(err) != domain.CodeState {
		t.Fatalf("code = %s: %v", domain.ErrorCode(err), err)
	}
	after := m.Snapshot()
	if after != before {
		t.Fatalf("illegal transition mutated state: before=%+v after=%+v", before, after)
	}
	if err := m.Transition(stage.Merging, 30); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(stage.Dispatching, 20); domain.ErrorCode(err) != domain.CodeState {
		t.Fatalf("progress rollback should fail: %v", err)
	}
	if m.Snapshot().Progress != 30 {
		t.Fatalf("progress changed on rejected rollback")
	}
	if err := m.Transition(stage.Dispatching, 60); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(stage.Committed, 100); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().State != stage.Committed {
		t.Fatalf("state = %s", m.Snapshot().State)
	}
}

func TestFailedCanOnlyRestartAtPendingLoad(t *testing.T) {
	m := stage.New(1, "d02")
	if err := m.Transition(stage.Quantizing, 10); err != nil {
		t.Fatal(err)
	}
	if err := m.Fail(20, nil); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().State != stage.Failed {
		t.Fatalf("state = %s", m.Snapshot().State)
	}
	if err := m.Transition(stage.Merging, 40); domain.ErrorCode(err) != domain.CodeState {
		t.Fatalf("failed shard accepted merge transition: %v", err)
	}
	if err := m.Transition(stage.PendingLoad, 0); domain.ErrorCode(err) != domain.CodeState {
		t.Fatalf("progress reset should be rejected: %v", err)
	}
	if err := m.Transition(stage.PendingLoad, 20); err != nil {
		t.Fatal(err)
	}
}
