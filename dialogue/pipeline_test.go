package dialogue

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStateMachineRejectsIllegalTransitionWithoutMutation(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	machine := NewStateMachine(project)
	if err := machine.Transition("seg-01", StatusCommitted); CodeOf(err) != CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if status, _ := machine.Status("seg-01"); status != StatusPending {
		t.Fatalf("status mutated to %s", status)
	}
	if progress := machine.Progress(); progress.Committed != 0 || progress.Total != 4 {
		t.Fatalf("progress mutated: %+v", progress)
	}
	if err := machine.Transition("seg-01", StatusMeasuring); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition("seg-01", StatusCalibrating); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition("seg-01", StatusCommitted); err != nil {
		t.Fatal(err)
	}
	if progress := machine.Progress(); progress.Committed != 1 {
		t.Fatalf("progress = %+v", progress)
	}
	if err := machine.Transition("seg-01", StatusPending); CodeOf(err) != CodeState {
		t.Fatalf("committed state accepted transition: %v", err)
	}
}

func TestIncrementalInvalidationAndCheckpointRestart(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := NewCheckpointStore(filepath.Join(dir, "checkpoint.json"))
	pipeline, err := NewPipeline(project, store)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pipeline.Build()
	if err != nil {
		t.Fatal(err)
	}
	if first.Progress.Committed != 4 || len(first.CommitOrder) != 4 {
		t.Fatalf("initial progress %+v order %v", first.Progress, first.CommitOrder)
	}
	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("checkpoint not written: %v", err)
	}
	seg01Before := first.Segments[0].Envelope.Digest()

	plan, err := pipeline.ApplyEdit(Edit{Kind: EditSegmentGain, SegmentID: "seg-02", Value: 9900})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Invalidated) != 1 || plan.Invalidated[0] != segmentNodeID("seg-02", "calibration") {
		t.Fatalf("gain edit invalidated %v", plan.Invalidated)
	}
	if len(plan.SegmentIDs) != 1 || plan.SegmentIDs[0] != "seg-02" {
		t.Fatalf("gain edit segments %v", plan.SegmentIDs)
	}
	second, err := pipeline.Build()
	if err != nil {
		t.Fatal(err)
	}
	if second.Segments[0].Envelope.Digest() != seg01Before {
		t.Fatal("unaffected segment was recomputed")
	}
	if second.Segments[1].Calibration.SessionGain != 9900 {
		t.Fatalf("edited gain not applied: %d", second.Segments[1].Calibration.SessionGain)
	}

	plan, err = pipeline.ApplyEdit(Edit{Kind: EditTargetLoudness, Value: 5600})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Invalidated) != 4 {
		t.Fatalf("target edit invalidated %d nodes: %v", len(plan.Invalidated), plan.Invalidated)
	}
	if _, err := pipeline.ApplyEdit(Edit{Kind: EditFrameRate, FrameLength: 400, Hop: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Build(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPipeline(pipeline.Project(), store)
	if err != nil {
		t.Fatal(err)
	}
	restartedResult, err := restarted.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(restartedResult.CheckpointInvalid) != 0 {
		t.Fatalf("fresh checkpoint invalid: %v", restartedResult.CheckpointInvalid)
	}
	if restartedResult.Progress.Committed != 4 {
		t.Fatalf("restart progress %+v", restartedResult.Progress)
	}
}

func TestCheckpointHashMismatchInvalidatesWholeSegment(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	store := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	pipeline, err := NewPipeline(project, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Build(); err != nil {
		t.Fatal(err)
	}
	modified := project.Clone()
	for sceneIndex := range modified.Scenes {
		for segmentIndex := range modified.Scenes[sceneIndex].Segments {
			if modified.Scenes[sceneIndex].Segments[segmentIndex].ID == "seg-03" {
				modified.Scenes[sceneIndex].Segments[segmentIndex].SessionGain += 10
			}
		}
	}
	restarted, err := NewPipeline(modified, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := restarted.Build()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range result.CheckpointInvalid {
		if id == "seg-03" {
			found = true
		}
	}
	if !found {
		t.Fatalf("seg-03 not invalidated: %v", result.CheckpointInvalid)
	}
}

func TestCheckpointVersionMismatchInvalidatesWholeSegment(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	store := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	pipeline, err := NewPipeline(project, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Build(); err != nil {
		t.Fatal(err)
	}
	record, ok := store.Record("seg-02")
	if !ok {
		t.Fatal("missing checkpoint record")
	}
	record.AlgorithmVersion = "other-algorithm"
	if _, err := store.Commit(project.ID, record); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPipeline(project, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := restarted.Build()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range result.CheckpointInvalid {
		if id == "seg-02" {
			found = true
		}
	}
	if !found {
		t.Fatalf("version mismatch did not invalidate: %v", result.CheckpointInvalid)
	}
}

func TestUnreachableLoudnessReturnsELOUD(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := NewPipeline(project, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Build(); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.ApplyEdit(Edit{Kind: EditTargetLoudness, Value: 50000}); err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.Build()
	if CodeOf(err) != CodeLoud {
		t.Fatalf("expected E_LOUD, got %v", err)
	}
	kernelError, ok := err.(*Error)
	if !ok || kernelError.TargetLoudness != 50000 || len(kernelError.LimitedIDs) != 4 ||
		kernelError.MinReachable == 0 || kernelError.MaxReachable == 0 {
		t.Fatalf("incomplete E_LOUD context: %+v", kernelError)
	}
}

func TestThreePathsAndDiffLocation(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatal(err)
	}
	edits := []Edit{
		{Kind: EditSegmentGain, SegmentID: "seg-02", Value: 9900},
		{Kind: EditTargetLoudness, Value: 5700},
		{Kind: EditSegmentGain, SegmentID: "seg-04", Value: 9000},
	}
	report, err := VerifyThreePaths(project, edits, t.TempDir())
	if err != nil {
		t.Fatalf("VerifyThreePaths: %v", err)
	}
	if !report.Equal || report.Realtime.FrameCount == 0 || report.Resume.FrameCount != report.Realtime.FrameCount ||
		report.Full.FrameCount != report.Realtime.FrameCount {
		t.Fatalf("bad report %+v", report)
	}

	pipeline, err := NewPipeline(project, nil)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pipeline.Build()
	if err != nil {
		t.Fatal(err)
	}
	right := cloneBuildResult(left)
	right.Segments[1].Envelope.Frames[2].Envelope++
	diffErr := CompareBuildResults(left, right)
	if CodeOf(diffErr) != CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", diffErr)
	}
	kernelError := diffErr.(*Error)
	if kernelError.SegmentID != "seg-02" || kernelError.FrameIndex == nil || *kernelError.FrameIndex != 2 || kernelError.Field != "envelope" {
		t.Fatalf("wrong diff location: %+v", kernelError)
	}
}

func TestCheckpointConcurrentCommitsRemainParseable(t *testing.T) {
	store := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _ = store.Commit("dlg", CheckpointRecord{SegmentID: "seg-" + string(rune('a'+index))})
		}(i)
	}
	wg.Wait()
	reloaded := NewCheckpointStore(store.Path())
	if err := reloaded.Load("dlg"); err != nil {
		t.Fatal(err)
	}
	records := reloaded.Records()
	if len(records) != 8 {
		t.Fatalf("got %d records", len(records))
	}
	seen := map[int]bool{}
	for _, record := range records {
		if record.Sequence <= 0 || seen[record.Sequence] {
			t.Fatalf("bad sequence %d", record.Sequence)
		}
		seen[record.Sequence] = true
	}
}
