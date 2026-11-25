package lipsync

import (
	"context"
	"testing"
)

func TestThreePathsFrameIdentical(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 2)
	store, path := openTempStore(t)
	options := RunOptions{WindowSize: 6, MaxWorkers: 2, FrameBufferLimit: 2, InjectStageWindow: -1}
	realtime := make([]PathResult, len(set.Projects))
	for i, project := range set.Projects {
		var err error
		realtime[i], err = RunRealtime(ctx, store, project, options)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i, project := range set.Projects {
		resumed, err := RunResume(ctx, store, project, options)
		if err != nil {
			t.Fatal(err)
		}
		full, err := fullPath(project)
		if err != nil {
			t.Fatal(err)
		}
		comparisons := []Comparison{
			CompareFrames(project.DialogueID, "realtime_vs_resume", realtime[i].Frames, resumed.Frames),
			CompareFrames(project.DialogueID, "realtime_vs_full", realtime[i].Frames, full.Frames),
			CompareFrames(project.DialogueID, "resume_vs_full", resumed.Frames, full.Frames),
			CompareFrames(project.DialogueID, "full_vs_truth", full.Frames, set.Truths[i].Frames),
			CompareTransitions(project.DialogueID, "realtime_transitions_vs_truth", realtime[i].Transitions, set.Truths[i].Transitions),
			CompareTransitions(project.DialogueID, "resume_transitions_vs_truth", resumed.Transitions, set.Truths[i].Transitions),
			CompareTransitions(project.DialogueID, "full_transitions_vs_truth", full.Transitions, set.Truths[i].Transitions),
		}
		for _, comparison := range comparisons {
			if !comparison.Consistent {
				t.Fatalf("%s/%s: %v", project.DialogueID, comparison.Name, comparison.Mismatch)
			}
		}
	}
}

func TestIncrementalReuseAndPreciseInvalidation(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 1)
	project := set.Projects[0]
	store, path := openTempStore(t)
	options := RunOptions{WindowSize: 6, MaxWorkers: 2, FrameBufferLimit: 2, InjectStageWindow: -1}
	if _, err := RunRealtime(ctx, store, project, options); err != nil {
		t.Fatal(err)
	}
	baseline, err := BuildTimeline(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	boundary, _, err := editPhonemeBoundary(project)
	if err != nil {
		t.Fatal(err)
	}
	boundaryResult, err := ComputeIncremental(ctx, store, boundary, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !boundaryResult.OutputMatch || boundaryResult.ReusedFrames == 0 ||
		boundaryResult.InvalidatedFrames >= len(boundaryResult.Frames) {
		t.Fatalf("boundary incremental stats = %+v", boundaryResult)
	}
	if len(boundaryResult.Invalidations) == 0 {
		t.Fatal("boundary edit produced no invalidation ranges")
	}

	weighted, err := cloneProject(project)
	if err != nil {
		t.Fatal(err)
	}
	weighted.Subtitle.Weights[2] += 3
	weightResult, err := ComputeIncremental(ctx, store, weighted, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !weightResult.OutputMatch || weightResult.ReusedFrames == 0 ||
		weightResult.InvalidatedFrames >= len(weightResult.Frames) {
		t.Fatalf("weight incremental stats = %+v", weightResult)
	}
	foundWeightReason := false
	for _, invalidation := range weightResult.Invalidations {
		for _, reason := range invalidation.Reasons {
			if reason == "subtitle_weight_changed" {
				foundWeightReason = true
			}
		}
	}
	if !foundWeightReason {
		t.Fatal("weight invalidation did not report subtitle_weight_changed")
	}

	highFPS, err := cloneProject(project)
	if err != nil {
		t.Fatal(err)
	}
	highFPS.FPS = project.FPS * 2
	fpsResult, err := ComputeIncremental(ctx, store, highFPS, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if !fpsResult.OutputMatch || fpsResult.ReusedFrames != 0 || fpsResult.RecomputedFrames != len(fpsResult.Frames) {
		t.Fatalf("fps incremental stats = %+v", fpsResult)
	}
}

func TestBoundedFrameBufferRecordsBlocking(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 1)
	result, err := func() (PathResult, error) {
		store, err := OpenStore(":memory:")
		if err != nil {
			return PathResult{}, err
		}
		defer store.Close()
		return RunRealtime(ctx, store, set.Projects[0], RunOptions{
			WindowSize:        12,
			MaxWorkers:        2,
			FrameBufferLimit:  2,
			InjectStageWindow: -1,
		})
	}()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.QueueEvents) == 0 {
		t.Fatal("bounded frame buffer did not record blocking")
	}
	for _, event := range result.QueueEvents {
		if event.Reason != "frame_buffer_exhausted_blocking" {
			t.Fatalf("unexpected queue reason: %+v", event)
		}
	}
	if comparison := CompareFrames(set.Projects[0].DialogueID, "bounded_vs_truth", result.Frames, set.Truths[0].Frames); !comparison.Consistent {
		t.Fatal(comparison.Mismatch)
	}
}

func TestStageInjectionReturnsLocatedStageError(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 1)
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = RunRealtime(ctx, store, set.Projects[0], RunOptions{
		WindowSize:        6,
		MaxWorkers:        2,
		FrameBufferLimit:  2,
		InjectStageWindow: 1,
		InjectStageReason: "injected_window_failure",
	})
	de, ok := asDomainError(err)
	if ErrorCode(err) != CodeStage || !ok || de.Window == nil || *de.Window != 1 ||
		de.Frame == nil || *de.Frame != 6 || de.Stage != "lip_mapping" {
		t.Fatalf("stage error = %+v", de)
	}
	checkpoints, loadErr := store.LoadCheckpoints(ctx, set.Projects[0].DialogueID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("failed run committed %d checkpoints", len(checkpoints))
	}
}

func TestCompareLocatesFirstFieldMismatch(t *testing.T) {
	set := sampleSetForTest(t, 1)
	base := set.Truths[0].Frames
	modified := append([]FrameResult(nil), base...)
	modified[3].CharEnd++
	comparison := CompareFrames(set.Projects[0].DialogueID, "test", base, modified)
	if comparison.Consistent || comparison.Mismatch == nil {
		t.Fatal("expected a mismatch")
	}
	if comparison.Mismatch.Frame != 3 || comparison.Mismatch.Field != "char_end" ||
		comparison.Mismatch.DialogueID != set.Projects[0].DialogueID {
		t.Fatalf("mismatch = %+v", comparison.Mismatch)
	}
}
