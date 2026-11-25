package acoustic_test

import (
	"context"
	"errors"
	"testing"

	acoustic "levelacoustic"
)

func TestStateMachineLegalAndIllegalTransitions(t *testing.T) {
	machine, err := acoustic.NewStateMachine([]int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(1, acoustic.RoomAnalyzingChain); err != nil {
		t.Fatal(err)
	}
	err = machine.Transition(1, acoustic.RoomCommitted)
	if err == nil {
		t.Fatal("expected illegal transition error")
	}
	domain := domainError(t, err)
	if domain.Code != acoustic.CodeState {
		t.Fatalf("expected E_STATE, got %v", domain)
	}
	status, _ := machine.State(1)
	if status.State != acoustic.RoomAnalyzingChain {
		t.Fatalf("state changed after illegal transition: %s", status.State)
	}
	if err := machine.Transition(1, acoustic.RoomSolvingReflection); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(1, acoustic.RoomCommitted); err != nil {
		t.Fatal(err)
	}
	if progress := machine.Progress(); progress.Committed != 1 || progress.Total != 2 {
		t.Fatalf("unexpected progress: %+v", progress)
	}
	err = machine.Transition(1, acoustic.RoomPending)
	if err == nil || acoustic.ErrorCode(err) != acoustic.CodeState {
		t.Fatalf("committed room should be frozen, got %v", err)
	}
	if status, _ := machine.State(1); status.State != acoustic.RoomCommitted {
		t.Fatalf("committed room state changed: %s", status.State)
	}
}

func TestSessionFailureIsolationAndRetry(t *testing.T) {
	project := sampleProject(t)
	failRoomTwo := true
	session, err := acoustic.NewSession(project, acoustic.SessionOptions{
		WorkerOptions: acoustic.WorkerOptions{MaxWorkers: 3, PerWorkerBuffer: 2},
		FailureInjector: func(roomID int) error {
			if failRoomTwo && roomID == 2 {
				failRoomTwo = false
				return errors.New("injected room failure")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := session.Run(context.Background())
	if err == nil {
		t.Fatal("expected injected failure")
	}
	if partial == nil {
		t.Fatal("expected partial run result")
	}
	if partial.Progress.Committed != 1 || partial.Progress.Total != 3 {
		t.Fatalf("unexpected progress after failure: %+v", partial.Progress)
	}
	if len(partial.CommitSequence) != 1 || partial.CommitSequence[0] != 1 {
		t.Fatalf("unexpected commits after failure: %v", partial.CommitSequence)
	}
	states := stateMap(session.States())
	if states[1] != acoustic.RoomCommitted {
		t.Fatalf("room 1 should remain committed: %s", states[1])
	}
	if states[2] != acoustic.RoomFailed {
		t.Fatalf("room 2 should be failed: %s", states[2])
	}
	if states[3] != acoustic.RoomPending {
		t.Fatalf("room 3 should not be polluted: %s", states[3])
	}
	if partial.Failure == nil || partial.Failure.RoomID != 2 {
		t.Fatalf("missing failure detail: %+v", partial.Failure)
	}
	if err := session.ResetRoomFailure(2); err != nil {
		t.Fatal(err)
	}
	final, err := session.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if final.Progress.Committed != 3 || len(final.CommitSequence) != 3 {
		t.Fatalf("retry did not complete: progress=%+v commits=%v", final.Progress, final.CommitSequence)
	}
	comparison := acoustic.CompareTruth(project, final.ListenerSolutions)
	if !comparison.Passed {
		t.Fatalf("retry result does not match truth: %+v", comparison.Mismatches)
	}
	workerStats := session.WorkerStats()
	if len(workerStats) != 3 {
		t.Fatalf("unexpected worker stats count: %d", len(workerStats))
	}
	for _, stats := range workerStats {
		if stats.ConfiguredWorkers > acoustic.AbsoluteMaxWorkers || stats.PerWorkerBuffer > acoustic.AbsolutePerWorkerBuffer {
			t.Fatalf("worker bounds exceeded: %+v", stats)
		}
		if stats.ObservedMaxWorkers > stats.ConfiguredWorkers {
			t.Fatalf("observed workers exceeded configured maximum: %+v", stats)
		}
		if stats.Tasks != 6 {
			t.Fatalf("expected listener/source/reflector tasks, got %+v", stats)
		}
	}
	snapshot, ok := session.Snapshot(1)
	if !ok {
		t.Fatal("missing committed snapshot")
	}
	originalAbsorption := snapshot.Reflectors[0].Absorption
	snapshot.Reflectors[0].Absorption = 0.99
	guard, ok := session.Snapshot(1)
	if !ok {
		t.Fatal("missing frozen snapshot")
	}
	if guard.Reflectors[0].Absorption != originalAbsorption {
		t.Fatal("mutating a returned snapshot changed the frozen session state")
	}
	project.Reflectors[0].Absorption = 0.99
	guard, _ = session.Snapshot(1)
	if guard.Reflectors[0].Absorption != originalAbsorption {
		t.Fatal("mutating the input project changed the frozen session state")
	}
}

func stateMap(statuses []acoustic.RoomStatus) map[int]acoustic.RoomState {
	result := make(map[int]acoustic.RoomState, len(statuses))
	for _, status := range statuses {
		result[status.RoomID] = status.State
	}
	return result
}
