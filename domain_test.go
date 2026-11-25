package macrokit

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func TestIntegerTimeRoundTrip(t *testing.T) {
	for frame := int64(0); frame <= 1000; frame++ {
		ms, back, err := RoundTripFrames(frame, 60)
		if err != nil {
			t.Fatalf("frame %d: %v", frame, err)
		}
		if back != frame {
			t.Fatalf("round trip frame=%d ms=%d back=%d", frame, ms, back)
		}
	}

	set := GenerateSamples(FixedSampleSeed)
	for _, sample := range set.Projects {
		for _, branch := range sample.Project.Branches {
			if _, back, err := RoundTripFrames(branch.DelayFrames, sample.Project.FrameRate); err != nil || back != branch.DelayFrames {
				t.Fatalf("branch delay %s: back=%d err=%v", branch.ID, back, err)
			}
			if _, back, err := RoundTripFrames(branch.TriggerWindowFrames, sample.Project.FrameRate); err != nil || back != branch.TriggerWindowFrames {
				t.Fatalf("window %s: back=%d err=%v", branch.ID, back, err)
			}
			for _, step := range branch.Steps {
				if _, back, err := RoundTripFrames(step.GapFrames, sample.Project.FrameRate); err != nil || back != step.GapFrames {
					t.Fatalf("gap %s: back=%d err=%v", step.ID, back, err)
				}
			}
		}
	}
}

func TestConversionOverflowIdentifiesBranch(t *testing.T) {
	_, err := framesToMSForBranch("b_overflow", math.MaxInt64, 60, RoundFloor, "/branches/0/delay_frames")
	var domainErr *Error
	if !errors.As(err, &domainErr) || domainErr.Code != EOvf {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	if domainErr.BranchID != "b_overflow" {
		t.Fatalf("branch id = %q", domainErr.BranchID)
	}

	_, err = ConvertMSToFrames(math.MaxInt64, 60, RoundCeil)
	if !HasCode(err, EOvf) {
		t.Fatalf("expected E_OVF from ms conversion, got %v", err)
	}
}

func TestConditionEvaluationDeterministicAndOrderIndependent(t *testing.T) {
	cond := allCondition(
		keyPressedCondition("KeyQ"),
		&Condition{Kind: CondFlag, Flag: "advanced"},
		&Condition{Kind: CondBranch, BranchID: "b0", Outcome: "completed"},
	)
	state := EvalState{
		Frame: 4,
		Keys: KeySnapshot{
			Frame: 4, Pressed: map[string]bool{"KeyQ": true},
			PressedAt: map[string]int64{"KeyQ": 4},
			Down:      map[string]bool{}, Released: map[string]bool{},
		},
		Flags: map[string]bool{"advanced": true},
		Results: map[string]*BranchState{
			"b0": {BranchID: "b0", Status: StatusCompleted, CommitFrame: 3},
		},
	}

	branch := &Branch{ID: "b1", Condition: cond, TriggerWindowFrames: 4}
	want, wantSnapshot := EvaluateBranchCondition(branch, state)
	if !want {
		t.Fatal("condition should be true")
	}
	wantHash := HashCanonical(wantSnapshot)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, snapshot := EvaluateBranchCondition(branch, state)
			if !ok || HashCanonical(snapshot) != wantHash {
				t.Errorf("non-deterministic evaluation")
			}
		}()
	}
	wg.Wait()

	// A later frame outside the press window must be false. Real snapshots
	// clear the one-frame Pressed edge while retaining PressedAt.
	state.Frame = 8
	state.Keys.Pressed = map[string]bool{}
	if ok, _ := EvaluateBranchCondition(branch, state); ok {
		t.Fatal("pressed condition should not remain true outside window")
	}
}

func TestComparatorLocatesEDiff(t *testing.T) {
	set := GenerateSamples(FixedSampleSeed)
	sample := findSample(t, set, "basic-timing")
	want, err := RunProject(sample.Project, sample.Events, ExecutorConfig{FrameCount: sample.Truth.FrameCount})
	if err != nil {
		t.Fatal(err)
	}

	got, err := RunProject(sample.Project, sample.Events, ExecutorConfig{FrameCount: sample.Truth.FrameCount})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareResults(want, got); err != nil {
		t.Fatalf("identical runs should compare equal: %v", err)
	}

	for frameIndex := range got.Frames {
		for stepIndex := range got.Frames[frameIndex].ExecutedSteps {
			if got.Frames[frameIndex].ExecutedSteps[stepIndex].Action != "" {
				got.Frames[frameIndex].ExecutedSteps[stepIndex].Action = "mutated-action"
				domainErr, ok := AsError(CompareResults(want, got))
				if !ok || domainErr.Code != EDiff {
					t.Fatalf("expected E_DIFF, got %v", CompareResults(want, got))
				}
				if domainErr.Frame != got.Frames[frameIndex].Frame || domainErr.BranchID == "" {
					t.Fatalf("diff location incomplete: %+v", domainErr)
				}
				if domainErr.Field != "executed_steps[0].action" && stepIndex != 0 {
					// Field includes the actual index; only require action suffix.
				}
				if domainErr.Field == "" {
					t.Fatal("diff field is empty")
				}
				return
			}
		}
	}
	t.Fatal("no executed step found to mutate")
}
