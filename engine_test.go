package loudness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func mustGeneratedV3(t *testing.T) *GeneratedCase {
	t.Helper()
	generated, err := GenerateCase(20261127, SchemaV3)
	if err != nil {
		t.Fatalf("GenerateCase: %v", err)
	}
	return generated
}

func copyCheckpointDir(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThreePathsAreFrameIdentical(t *testing.T) {
	generated := mustGeneratedV3(t)
	baselineDir := t.TempDir()
	baselineEngine, err := NewEngine(generated.Project, EngineOptions{CheckpointDir: baselineDir})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := baselineEngine.Run(context.Background(), RunFull)
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}
	if len(baseline.Clips) != len(generated.Project.Clips) {
		t.Fatalf("got %d clips", len(baseline.Clips))
	}
	resumeDir := t.TempDir()
	copyCheckpointDir(t, baselineDir, resumeDir)
	_ = os.Remove(filepath.Join(resumeDir, "clip-000003.json"))

	changes := []Change{
		{Kind: ChangeGain, ClipID: 2, GainQ12: 5120},
		{Kind: ChangeTarget, TargetLoudnessDBQ8: generated.Project.TargetLoudnessDBQ8 - 256},
	}
	incrementalEngine, err := NewEngine(generated.Project, EngineOptions{CheckpointDir: baselineDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incrementalEngine.ApplyChanges(changes); err != nil {
		t.Fatalf("apply incremental changes: %v", err)
	}
	incremental, err := incrementalEngine.Run(context.Background(), RunIncremental)
	if err != nil {
		t.Fatalf("incremental run: %v", err)
	}

	fullEngine, err := NewEngine(generated.Project, EngineOptions{CheckpointDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fullEngine.ApplyChanges(changes); err != nil {
		t.Fatalf("apply full changes: %v", err)
	}
	full, err := fullEngine.Run(context.Background(), RunFull)
	if err != nil {
		t.Fatalf("full run: %v", err)
	}

	resumeEngine, err := NewEngine(generated.Project, EngineOptions{CheckpointDir: resumeDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumeEngine.ApplyChanges(changes); err != nil {
		t.Fatalf("apply resume changes: %v", err)
	}
	resumed, err := resumeEngine.Run(context.Background(), RunResume)
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}

	if mismatch := CompareBuildResults(incremental, full); mismatch != nil {
		t.Fatalf("incremental vs full: %v", mismatch)
	}
	if mismatch := CompareBuildResults(incremental, resumed); mismatch != nil {
		t.Fatalf("incremental vs resume: %v", mismatch)
	}
	if len(incremental.MeasurementReused) == 0 {
		t.Fatal("incremental path did not reuse any persisted measurement")
	}
	if len(incremental.MeasurementReused) != len(generated.Project.Clips)-1 {
		t.Fatalf("incremental reused %d measurements, want %d", len(incremental.MeasurementReused), len(generated.Project.Clips)-1)
	}
	if len(incremental.ComputedClips) != 1 || incremental.ComputedClips[0] != 2 {
		t.Fatalf("incremental computed clips = %v, want [2]", incremental.ComputedClips)
	}
	if len(incremental.Invalidations) != len(generated.Project.Clips)+1 {
		t.Fatalf("invalidation count = %d, want %d", len(incremental.Invalidations), len(generated.Project.Clips)+1)
	}
	if len(incremental.Checkpoints) != len(generated.Project.Clips) {
		t.Fatalf("checkpoint count = %d", len(incremental.Checkpoints))
	}
	for i, checkpoint := range incremental.Checkpoints {
		if checkpoint.CommitIndex != i {
			t.Fatalf("checkpoint commit index %d = %d", i, checkpoint.CommitIndex)
		}
	}
	for _, state := range incremental.State {
		if state.State != StateCommitted {
			t.Fatalf("clip %d state = %s", state.ClipID, state.State)
		}
		if state.Frozen == nil {
			t.Fatalf("clip %d has no frozen commit", state.ClipID)
		}
	}
}

func TestTargetEditReusesMeasurementAndCompareLocatesFrame(t *testing.T) {
	generated := mustGeneratedV3(t)
	dir := t.TempDir()
	baselineEngine, _ := NewEngine(generated.Project, EngineOptions{CheckpointDir: dir})
	baseline, err := baselineEngine.Run(context.Background(), RunFull)
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := NewEngine(generated.Project, EngineOptions{CheckpointDir: dir})
	invalidations, err := engine.ApplyChanges([]Change{
		{Kind: ChangeTarget, TargetLoudnessDBQ8: generated.Project.TargetLoudnessDBQ8 - 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalidation := range invalidations {
		if invalidation.Phase != PhaseCalibration {
			t.Fatalf("target invalidation phase = %s", invalidation.Phase)
		}
	}
	targetChanged, err := engine.Run(context.Background(), RunIncremental)
	if err != nil {
		t.Fatal(err)
	}
	if len(targetChanged.MeasurementReused) != len(generated.Project.Clips) {
		t.Fatalf("target edit reused %d measurements", len(targetChanged.MeasurementReused))
	}
	if len(targetChanged.CalibrationReused) != 0 {
		t.Fatalf("target edit reused %d calibration results", len(targetChanged.CalibrationReused))
	}
	if mismatch := CompareBuildResults(baseline, targetChanged); mismatch == nil {
		t.Fatal("target edit should differ from baseline")
	}
	cloned := *targetChanged
	cloned.Clips = make([]ClipResult, len(targetChanged.Clips))
	for i := range targetChanged.Clips {
		cloned.Clips[i] = cloneClipResult(targetChanged.Clips[i])
	}
	cloned.Clips[3].Frames[2].CalibratedDBQ8++
	mismatch := CompareBuildResults(targetChanged, &cloned)
	if mismatch == nil || mismatch.ClipID != generated.Project.Clips[3].ID || mismatch.FrameIndex != 2 || mismatch.Field != "calibratedDbQ8" {
		t.Fatalf("mismatch locator: %v", mismatch)
	}
}

func TestEngineLoudnessErrorAndStateFreeze(t *testing.T) {
	generated := mustGeneratedV3(t)
	engine, _ := NewEngine(generated.Project, EngineOptions{CheckpointDir: t.TempDir()})
	if _, err := engine.ApplyChanges([]Change{{Kind: ChangeTarget, TargetLoudnessDBQ8: 0}}); err != nil {
		t.Fatal(err)
	}
	_, err := engine.Run(context.Background(), RunFull)
	if !HasCode(err, ELoud) {
		t.Fatalf("expected E_LOUD, got %v", err)
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.Target != 0 || domainErr.Reachable >= 0 || len(domainErr.LimitedClips) == 0 {
		t.Fatalf("engine E_LOUD diagnostics: %+v", domainErr)
	}
	engine, _ = NewEngine(generated.Project, EngineOptions{CheckpointDir: t.TempDir()})
	result, err := engine.Run(context.Background(), RunFull)
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range result.State {
		if snapshot.State != StateCommitted || snapshot.Frozen == nil {
			t.Fatalf("state not committed with frozen result: %+v", snapshot)
		}
	}
}

func TestFrameRateEditInvalidatesAllMeasurements(t *testing.T) {
	generated := mustGeneratedV3(t)
	dir := t.TempDir()
	engine, _ := NewEngine(generated.Project, EngineOptions{CheckpointDir: dir})
	if _, err := engine.Run(context.Background(), RunFull); err != nil {
		t.Fatal(err)
	}
	engine, _ = NewEngine(generated.Project, EngineOptions{CheckpointDir: dir})
	invalidations, err := engine.ApplyChanges([]Change{
		{Kind: ChangeFrameRate, FrameLength: 192, FrameShift: 96},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalidation := range invalidations {
		if invalidation.Phase != PhaseEnvelope {
			t.Fatalf("frame-rate phase = %s", invalidation.Phase)
		}
	}
	result, err := engine.Run(context.Background(), RunIncremental)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MeasurementReused) != 0 {
		t.Fatalf("frame-rate edit reused %d stale measurements", len(result.MeasurementReused))
	}
	if len(result.ComputedClips) != len(generated.Project.Clips) {
		t.Fatalf("frame-rate edit computed %d clips", len(result.ComputedClips))
	}
	for _, clip := range result.Clips {
		for _, frame := range clip.Frames {
			if frame.EndSample-frame.StartSample > 192 {
				t.Fatalf("frame interval length exceeds new frame length: %d", frame.EndSample-frame.StartSample)
			}
		}
	}
}
