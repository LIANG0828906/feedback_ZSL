package bonerig

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestFrameFailureLeavesIntervalFailedAndRetryCommits(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	dir := t.TempDir()
	storePath := dir + "/checkpoint.json"
	store, err := OpenCheckpointStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{
		Asset: sample.Asset, Budget: Budget{MaxWorkers: 2, MaxJointBufferPerWorker: 4, MaxPendingFrames: 1}, Store: store,
		Hooks: EngineHooks{FrameError: func(frame int) *Error {
			if frame == 8 {
				return errorf(CodeQuat, "injected single frame failure")
			}
			return nil
		}},
	}
	_, rerr := engine.Run(context.Background())
	be := asBonerigError(t, rerr)
	if be.Code != CodeQuat || be.Frame == nil || *be.Frame != 8 {
		t.Fatalf("unexpected run error: %+v", be)
	}
	if _, ok := store.Get("frames-0008-0012"); ok {
		t.Fatal("failed interval must not be committed")
	}
	if _, ok := store.Get("frames-0000-0004"); !ok {
		t.Fatal("committed earlier interval missing")
	}

	store2, oerr := OpenCheckpointStore(storePath)
	if oerr != nil {
		t.Fatal(oerr)
	}
	retry := &Engine{Asset: sample.Asset, Budget: DefaultBudget(), Store: store2}
	report, rerr := retry.Run(context.Background())
	if rerr != nil {
		t.Fatal(rerr)
	}
	var last IntervalRun
	for _, run := range report.Intervals {
		if run.IntervalID == "frames-0008-0012" {
			last = run
		}
	}
	if last.State != StateCommitted || last.Reused {
		t.Fatalf("retried interval should commit: %+v", last)
	}
	sqlBytes, readErr := os.ReadFile(dir + "/checkpoint.sql")
	if readErr != nil {
		t.Fatal(readErr)
	}
	sqlText := string(sqlBytes)
	if !strings.Contains(sqlText, "CREATE TABLE IF NOT EXISTS precision_component") || !strings.Contains(sqlText, "INSERT OR REPLACE") {
		t.Fatal("SQLite-compatible checkpoint SQL missing tables/inserts")
	}
}

func TestThreePathsAndThreadBudgetsAreFrameIdentical(t *testing.T) {
	low, err := RunThreePaths(t.TempDir()+"/low", Budget{MaxWorkers: 1, MaxJointBufferPerWorker: 2, MaxPendingFrames: 1})
	if err != nil {
		t.Fatal(err)
	}
	high, err := RunThreePaths(t.TempDir()+"/high", Budget{MaxWorkers: 4, MaxJointBufferPerWorker: 6, MaxPendingFrames: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range []*ThreePathReport{low, high} {
		if rep.FrameDiff != nil {
			t.Fatal(rep.FrameDiff)
		}
		if rep.SequenceDiff != nil {
			t.Fatal(rep.SequenceDiff)
		}
		if !rep.RecoveryWriteSequenceMatches {
			t.Fatal("recovery write sequence differs")
		}
	}
	if diff := CompareFrames(low.Paths[0].Frames, high.Paths[0].Frames); diff != nil {
		t.Fatal(diff)
	}
}

func TestCompareFramesLocatesFirstDiff(t *testing.T) {
	expected := []FrameResult{{Index: 0, Outputs: map[string]JointOutput{"Root": {Joint: "Root", Rotation: Quat{W: 1, Scale: 1}}}}}
	actual := []FrameResult{{Index: 0, Outputs: map[string]JointOutput{"Root": {Joint: "Root", Rotation: Quat{W: 2, Scale: 1}}}}}
	err := CompareFrames(expected, actual)
	be := asBonerigError(t, err)
	if be.Code != CodeDiff || be.Joint != "Root" || be.Frame == nil || *be.Frame != 0 || be.Field != "rotation.w" {
		t.Fatalf("bad diff locator: %+v", be)
	}
}
