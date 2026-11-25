package iconmask

import "testing"

func TestBlockStateMachineProgressAndFreeze(t *testing.T) {
	machine := NewBlockStateMachine(Coord{X: 1, Y: 2})
	mask := NewMask(2, 2)
	mask.Set(0, 0, 1)
	output := StageOutput{Digest: "radius", Mask: &mask}

	if err := machine.Start(StageRadius); err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(StageRadius, output); err != nil {
		t.Fatal(err)
	}
	mask.Set(0, 0, 0)
	snapshot := machine.Snapshot()
	committedMask := snapshot.Rounds[0].Stages[StageRadius].Output.Mask
	if committedMask.At(0, 0) != 1 {
		t.Fatal("committed output must be frozen against caller mutation")
	}
	if stages, rounds := machine.Progress(); stages != 1 || rounds != 0 {
		t.Fatalf("unexpected progress: stages=%d rounds=%d", stages, rounds)
	}

	if err := machine.Start(StageClose); err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(StageClose, StageOutput{Digest: "close"}); err != nil {
		t.Fatal(err)
	}
	if err := machine.Start(StageOpen); err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(StageOpen, StageOutput{Digest: "open"}); err != nil {
		t.Fatal(err)
	}
	if err := machine.Start(StageStats); err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(StageStats, StageOutput{Digest: "stats"}); err != nil {
		t.Fatal(err)
	}
	if stages, rounds := machine.Progress(); stages != 4 || rounds != 1 {
		t.Fatalf("unexpected progress after round: stages=%d rounds=%d", stages, rounds)
	}

	// The next round starts only after the previous stats stage is committed.
	if err := machine.Start(StageRadius); err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(StageRadius, StageOutput{Digest: "radius-2"}); err != nil {
		t.Fatal(err)
	}
	if stages, rounds := machine.Progress(); stages != 5 || rounds != 1 {
		t.Fatalf("unexpected progress in round two: stages=%d rounds=%d", stages, rounds)
	}
}

func TestBlockStateMachineIllegalTransitionPreservesState(t *testing.T) {
	machine := NewBlockStateMachine(Coord{})
	if err := machine.Start(StageClose); err == nil || err.Code != CodeState {
		t.Fatalf("expected E_STATE for out-of-order start, got %v", err)
	}
	if stages, rounds := machine.Progress(); stages != 0 || rounds != 0 {
		t.Fatalf("illegal transition modified progress: %d/%d", stages, rounds)
	}
	if err := machine.Start(StageRadius); err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(StageRadius, StageOutput{Digest: "r"}); err != nil {
		t.Fatal(err)
	}
	before := machine.Snapshot()
	if err := machine.Commit(StageRadius, StageOutput{Digest: "again"}); err == nil || err.Code != CodeState {
		t.Fatalf("expected E_STATE for double commit, got %v", err)
	}
	after := machine.Snapshot()
	if before.CommittedStages != after.CommittedStages || before.CurrentRound != after.CurrentRound ||
		before.Rounds[0].Stages[StageRadius].Output.Digest != after.Rounds[0].Stages[StageRadius].Output.Digest {
		t.Fatal("illegal transition changed state or progress")
	}
	if err := machine.ApplyEdit(StageRadius); err == nil || err.Code != CodeState {
		t.Fatalf("expected frozen stage edit rejection, got %v", err)
	}
}

func TestBlockStateMachineFailureRetry(t *testing.T) {
	machine := NewBlockStateMachine(Coord{})
	if err := machine.Start(StageRadius); err != nil {
		t.Fatal(err)
	}
	if err := machine.Fail(StageRadius, "boom"); err != nil {
		t.Fatal(err)
	}
	if stages, rounds := machine.Progress(); stages != 0 || rounds != 0 {
		t.Fatal("failure must not advance progress")
	}
	if err := machine.Retry(StageRadius); err != nil {
		t.Fatal(err)
	}
	if err := machine.Start(StageRadius); err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(StageRadius, StageOutput{Digest: "ok"}); err != nil {
		t.Fatal(err)
	}
	if stages, _ := machine.Progress(); stages != 1 {
		t.Fatalf("expected one committed stage, got %d", stages)
	}
}

func TestReplayCheckpointPrefix(t *testing.T) {
	checkpoints := []Checkpoint{
		{Block: Coord{X: 1, Y: 1}, Round: 0, Stage: StageRadius, OutputDigest: "r"},
		{Block: Coord{X: 1, Y: 1}, Round: 0, Stage: StageClose, OutputDigest: "c"},
		{Block: Coord{X: 1, Y: 1}, Round: 0, Stage: StageOpen, OutputDigest: "o"},
		{Block: Coord{X: 1, Y: 1}, Round: 0, Stage: StageStats, OutputDigest: "s"},
		{Block: Coord{X: 1, Y: 1}, Round: 1, Stage: StageRadius, OutputDigest: "r2"},
	}
	machines, err := ReplayCheckpointPrefix(checkpoints)
	if err != nil {
		t.Fatal(err)
	}
	machine := machines[Coord{X: 1, Y: 1}]
	if machine == nil {
		t.Fatal("missing replayed machine")
	}
	if stages, rounds := machine.Progress(); stages != 5 || rounds != 1 {
		t.Fatalf("unexpected replayed progress: stages=%d rounds=%d", stages, rounds)
	}
	bad := []Checkpoint{
		{Block: Coord{}, Round: 0, Stage: StageClose, OutputDigest: "c"},
	}
	if _, err := ReplayCheckpointPrefix(bad); err == nil || err.Code != CodeState {
		t.Fatalf("expected E_STATE for illegal prefix, got %v", err)
	}
}
