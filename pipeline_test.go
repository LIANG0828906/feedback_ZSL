package dialogue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryPathMatchesFullBuild(t *testing.T) {
	p, _, _ := sampleProject(t)
	full, pErr := FullBuild(p)
	if pErr != nil {
		t.Fatal(pErr)
	}

	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.json")
	store, err := NewFileCheckpointStore(checkpointPath, p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out, pErr := ProcessClip(p, p.Clips[i], nil)
		if pErr != nil {
			t.Fatal(pErr)
		}
		if err := store.SaveClip(out); err != nil {
			t.Fatal(err)
		}
	}

	engine, report, rErr := RecoverOrBuild(p, checkpointPath)
	if rErr != nil {
		t.Fatal(rErr)
	}
	if len(report.RestoredClips) != 2 || len(report.RebuiltClips) != 2 {
		t.Fatalf("unexpected recovery report: %+v", report)
	}
	cmp := CompareFrames(full, engine.Results())
	if !cmp.Equal {
		t.Fatalf("recovery/full mismatch: %+v", cmp.Mismatches)
	}

	snap, err := LoadSnapshot(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	snap.ProjectHash = "tampered"
	data, _ := json.Marshal(snap)
	if err := os.WriteFile(checkpointPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, report2, rErr := RecoverOrBuild(p, checkpointPath)
	if rErr != nil {
		t.Fatal(rErr)
	}
	if !report2.Discarded || report2.DiscardReason == "" {
		t.Fatalf("tampered snapshot must be discarded wholesale: %+v", report2)
	}
}

func TestIncrementalGainEditMatchesFullRebuild(t *testing.T) {
	p, _, _ := sampleProject(t)
	engine := NewEngine(p)
	if _, pErr := engine.BuildAll(); pErr != nil {
		t.Fatal(pErr)
	}
	oldGain := p.Clips[1].GainBP
	plan, eErr := engine.ApplyClipGainEdit("clip-02", oldGain+200)
	if eErr != nil {
		t.Fatal(eErr)
	}
	incremental, pErr := engine.RebuildPlan(plan)
	if pErr != nil {
		t.Fatal(pErr)
	}
	if len(plan.AffectedClips) != 1 || plan.AffectedClips[0] != "clip-02" {
		t.Fatalf("gain edit should affect one clip: %+v", plan.AffectedClips)
	}
	if len(plan.RebuiltFeatureFrames) != 0 {
		t.Fatalf("gain edit must not rebuild features: %v", plan.RebuiltFeatureFrames)
	}
	if len(plan.RebuiltCalibrationFrames) != 5 {
		t.Fatalf("expected 5 calibration frames rebuilt, got %d", len(plan.RebuiltCalibrationFrames))
	}

	fullEdited, pErr := FullBuild(CloneProject(p))
	if pErr != nil {
		t.Fatal(pErr)
	}
	cmp := CompareFrames(incremental, fullEdited)
	if !cmp.Equal {
		t.Fatalf("incremental/full gain mismatch: %+v", cmp.Mismatches)
	}
}

func TestRecoveryThenIncrementalMatchesFull(t *testing.T) {
	p, _, _ := sampleProject(t)
	checkpointPath := filepath.Join(t.TempDir(), "checkpoint.json")
	store, err := NewFileCheckpointStore(checkpointPath, p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out, pErr := ProcessClip(p, p.Clips[i], nil)
		if pErr != nil {
			t.Fatal(pErr)
		}
		if err := store.SaveClip(out); err != nil {
			t.Fatal(err)
		}
	}
	engine, _, rErr := RecoverOrBuild(p, checkpointPath)
	if rErr != nil {
		t.Fatal(rErr)
	}
	plan, eErr := engine.ApplyClipGainEdit("clip-03", p.Clips[2].GainBP+160)
	if eErr != nil {
		t.Fatal(eErr)
	}
	incremental, pErr := engine.RebuildPlan(plan)
	if pErr != nil {
		t.Fatal(pErr)
	}
	fullAfterEdit, pErr := FullBuild(CloneProject(p))
	if pErr != nil {
		t.Fatal(pErr)
	}
	if cmp := CompareFrames(incremental, fullAfterEdit); !cmp.Equal {
		t.Fatalf("recovery+incremental/full mismatch: %+v", cmp.Mismatches)
	}
}

func TestIncrementalTargetEditMatchesFullRebuild(t *testing.T) {
	p, _, _ := sampleProject(t)
	engine := NewEngine(p)
	if _, pErr := engine.BuildAll(); pErr != nil {
		t.Fatal(pErr)
	}
	plan, eErr := engine.ApplyTargetEdit(78_000)
	if eErr != nil {
		t.Fatal(eErr)
	}
	incremental, pErr := engine.RebuildPlan(plan)
	if pErr != nil {
		t.Fatal(pErr)
	}
	if len(plan.RebuiltFeatureFrames) != 0 {
		t.Fatalf("target edit must retain all features")
	}
	if len(plan.RebuiltCalibrationFrames) != 20 {
		t.Fatalf("expected all 20 calibration frames, got %d", len(plan.RebuiltCalibrationFrames))
	}
	fullEdited, pErr := FullBuild(CloneProject(p))
	if pErr != nil {
		t.Fatal(pErr)
	}
	if cmp := CompareFrames(incremental, fullEdited); !cmp.Equal {
		t.Fatalf("target incremental/full mismatch: %+v", cmp.Mismatches)
	}
}

func TestTargetEditDoesNotInvalidateFrozenClip(t *testing.T) {
	p, _, _ := sampleProject(t)
	engine := NewEngine(p)
	if _, pErr := engine.BuildAll(); pErr != nil {
		t.Fatal(pErr)
	}
	if pErr := engine.FreezeClip("clip-01"); pErr != nil {
		t.Fatal(pErr)
	}
	before, pErr := engine.ClipResults("clip-01")
	if pErr != nil {
		t.Fatal(pErr)
	}
	plan, eErr := engine.ApplyTargetEdit(64_000)
	if eErr != nil {
		t.Fatal(eErr)
	}
	if len(plan.FrozenClips) != 1 || plan.FrozenClips[0] != "clip-01" {
		t.Fatalf("target plan should record frozen clip: %+v", plan.FrozenClips)
	}
	if _, pErr := engine.RebuildPlan(plan); pErr != nil {
		t.Fatal(pErr)
	}
	after, pErr := engine.ClipResults("clip-01")
	if pErr != nil {
		t.Fatal(pErr)
	}
	if cmp := CompareFrames(before, after); !cmp.Equal {
		t.Fatalf("frozen clip changed after target edit: %+v", cmp.Mismatches)
	}
}

func TestIncrementalFPSEditMatchesFullRebuildAndTracesAttribution(t *testing.T) {
	p, _, _ := sampleProject(t)
	fullProject := CloneProject(p)
	fullProject.FrameRate = 30
	full30, pErr := FullBuild(fullProject)
	if pErr != nil {
		t.Fatal(pErr)
	}

	engine := NewEngine(p)
	if _, pErr := engine.BuildAll(); pErr != nil {
		t.Fatal(pErr)
	}
	plan, eErr := engine.ApplyFrameRateEdit(30)
	if eErr != nil {
		t.Fatal(eErr)
	}
	incremental, pErr := engine.RebuildPlan(plan)
	if pErr != nil {
		t.Fatal(pErr)
	}
	if len(plan.FeatureRebuildRanges) == 0 || len(plan.Attribution) == 0 {
		t.Fatalf("fps edit missing precise rebuild/attribution data: %+v", plan)
	}
	if cmp := CompareFrames(incremental, full30); !cmp.Equal {
		t.Fatalf("fps incremental/full mismatch: %+v", cmp.Mismatches)
	}
}
