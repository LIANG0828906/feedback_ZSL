package lipsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func openTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lipsync.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func TestCheckpointResumeFromSQLiteOnly(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 2)
	store, path := openTempStore(t)
	options := RunOptions{WindowSize: 6, MaxWorkers: 2, FrameBufferLimit: 3, InjectStageWindow: -1}
	for _, project := range set.Projects {
		if _, err := RunRealtime(ctx, store, project, options); err != nil {
			t.Fatal(err)
		}
	}
	checkpoints, err := store.LoadCheckpoints(ctx, set.Projects[0].DialogueID)
	if err != nil {
		t.Fatal(err)
	}
	timeline, err := BuildTimeline(set.Projects[0])
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := ValidateCheckpoints(set.Projects[0], checkpoints, len(timeline.Frames), options.WindowSize); !ok {
		t.Fatalf("checkpoint validation failed: %s", reason)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path)
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
		comparison := CompareFrames(project.DialogueID, "resume_vs_full", resumed.Frames, full.Frames)
		if !comparison.Consistent {
			t.Fatalf("%s: %v", project.DialogueID, comparison.Mismatch)
		}
		if resumed.RestoredWindows == 0 || resumed.ReusedFrames != len(full.Frames) || resumed.RecomputedFrames != 0 {
			t.Fatalf("%s resume stats = restored %d reused %d recomputed %d", project.DialogueID, resumed.RestoredWindows, resumed.ReusedFrames, resumed.RecomputedFrames)
		}
		truthComparison := CompareFrames(project.DialogueID, "resume_vs_truth", resumed.Frames, set.Truths[i].Frames)
		if !truthComparison.Consistent {
			t.Fatalf("%s truth: %v", project.DialogueID, truthComparison.Mismatch)
		}
	}
}

func TestPartialCheckpointPrefixResumes(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 1)
	project := set.Projects[0]
	store, path := openTempStore(t)
	options := RunOptions{WindowSize: 6, MaxWorkers: 2, FrameBufferLimit: 3, InjectStageWindow: -1}
	if _, err := RunRealtime(ctx, store, project, options); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE dialogue_id = ? AND window_index >= 2`, project.DialogueID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, err := RunResume(ctx, store, project, options)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RestoredWindows != 2 {
		t.Fatalf("restored windows = %d, want 2", resumed.RestoredWindows)
	}
	full, err := fullPath(project)
	if err != nil {
		t.Fatal(err)
	}
	if comparison := CompareFrames(project.DialogueID, "partial_resume_vs_full", resumed.Frames, full.Frames); !comparison.Consistent {
		t.Fatal(comparison.Mismatch)
	}
	checkpoints, err := store.LoadCheckpoints(ctx, project.DialogueID)
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := ValidateCheckpoints(project, checkpoints, len(full.Frames), options.WindowSize); !ok {
		t.Fatalf("rebuilt checkpoint set invalid: %s", reason)
	}
}

func TestCheckpointMismatchPurgesAndRecomputesAll(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 1)
	project := set.Projects[0]
	store, path := openTempStore(t)
	options := RunOptions{WindowSize: 6, MaxWorkers: 2, FrameBufferLimit: 3, InjectStageWindow: -1}
	if _, err := RunRealtime(ctx, store, project, options); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE checkpoints SET content_hash = 'tampered' WHERE dialogue_id = ? AND window_index = 1`, project.DialogueID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, err := RunResume(ctx, store, project, options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resumed.ResetReason, "hash_or_version_mismatch") {
		t.Fatalf("reset reason = %q", resumed.ResetReason)
	}
	if resumed.ReusedFrames != 0 || resumed.RecomputedFrames != len(resumed.Frames) {
		t.Fatalf("mismatch did not force full recompute: reused=%d recomputed=%d", resumed.ReusedFrames, resumed.RecomputedFrames)
	}
	if comparison := CompareFrames(project.DialogueID, "reset_vs_truth", resumed.Frames, set.Truths[0].Frames); !comparison.Consistent {
		t.Fatal(comparison.Mismatch)
	}
}

func TestFailedCheckpointWriteIsAtomic(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 1)
	project := set.Projects[0]
	store, _ := openTempStore(t)
	defer store.Close()
	options := RunOptions{WindowSize: 6, MaxWorkers: 2, FrameBufferLimit: 3, InjectStageWindow: -1}
	if _, err := RunRealtime(ctx, store, project, options); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadCheckpoints(ctx, project.DialogueID)
	if err != nil {
		t.Fatal(err)
	}
	bad := Checkpoint{
		DialogueID:  project.DialogueID,
		WindowIndex: 0,
		FrameStart:  0,
		FrameEnd:    2,
		Status:      StateCommitted,
		Frames:      []FrameResult{{Frame: 0}, {Frame: 1}},
	}
	if err := store.CommitWindow(ctx, bad); ErrorCode(err) != CodeStage {
		t.Fatalf("bad checkpoint code = %s, want E_STAGE: %v", ErrorCode(err), err)
	}
	after, err := store.LoadCheckpoints(ctx, project.DialogueID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed transaction changed checkpoint count %d -> %d", len(before), len(after))
	}
}

func TestFrameCacheVersionMismatchInvalidatesCheckpointSet(t *testing.T) {
	ctx := context.Background()
	set := sampleSetForTest(t, 1)
	project := set.Projects[0]
	store, path := openTempStore(t)
	options := RunOptions{WindowSize: 6, MaxWorkers: 2, FrameBufferLimit: 3, InjectStageWindow: -1}
	if _, err := RunRealtime(ctx, store, project, options); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE frame_cache SET q_version = 'tampered' WHERE dialogue_id = ? AND rowid = (
		SELECT rowid FROM frame_cache WHERE dialogue_id = ? ORDER BY rowid LIMIT 1
	)`, project.DialogueID, project.DialogueID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, err := RunResume(ctx, store, project, options)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resumed.ResetReason, "frame_cache_hash_or_version_mismatch") {
		t.Fatalf("reset reason = %q", resumed.ResetReason)
	}
	if resumed.RecomputedFrames != len(resumed.Frames) {
		t.Fatalf("frame-cache mismatch did not force full recompute: %+v", resumed)
	}
}

func TestInMemoryStoreForStageInjection(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	set := sampleSetForTest(t, 1)
	_, err = RunRealtime(context.Background(), store, set.Projects[0], RunOptions{
		WindowSize:        6,
		MaxWorkers:        2,
		FrameBufferLimit:  2,
		InjectStageWindow: 0,
		InjectStageReason: "injected_window_failure",
	})
	if ErrorCode(err) != CodeStage {
		t.Fatalf("stage injection code = %s, want E_STAGE: %v", ErrorCode(err), err)
	}
}
