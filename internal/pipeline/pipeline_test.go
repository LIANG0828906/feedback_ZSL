package pipeline_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"mocapworkbench/internal/checkpoint"
	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/pipeline"
	"mocapworkbench/internal/retarget"
	"mocapworkbench/internal/sample"
)

func TestCheckpointResumeAndInvalidation(t *testing.T) {
	clip, target := fixture(t)
	cfg := model.DefaultConfig()
	db := filepath.Join(t.TempDir(), "checkpoint.db")
	partial, err := pipeline.Run(clip, target, cfg, pipeline.Options{
		StorePath: db, StopAfter: 17, ForceFull: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(partial.Frames) != 17 || partial.SnapshotAction != "interrupted" {
		t.Fatalf("expected normal interrupted prefix, got frames=%d action=%s", len(partial.Frames), partial.SnapshotAction)
	}
	resumed, err := pipeline.Run(clip, target, cfg, pipeline.Options{StorePath: db})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SnapshotAction != "resumed" || resumed.ResumedFrames != 17 {
		t.Fatalf("expected resume from 17, got action=%s resumed=%d reason=%s", resumed.SnapshotAction, resumed.ResumedFrames, resumed.SnapshotReason)
	}
	if diff := pipeline.FirstDiff(partial.Frames, resumed.Frames[:17], target); diff != nil {
		t.Fatalf("prefix changed after resume: %#v", diff)
	}
	full, err := retarget.Run(clip, target, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if diff := pipeline.FirstDiff(full.Frames, resumed.Frames, target); diff != nil {
		t.Fatalf("resume differs from full recompute: %#v", diff)
	}
}

func TestCorruptSnapshotIsDiscardedWholesale(t *testing.T) {
	clip, target := fixture(t)
	cfg := model.DefaultConfig()
	db := filepath.Join(t.TempDir(), "corrupt.db")
	if _, err := pipeline.Run(clip, target, cfg, pipeline.Options{StorePath: db, StopAfter: 12}); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE frames SET content_hash='corrupt' WHERE frame=5`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	recomputed, err := pipeline.Run(clip, target, cfg, pipeline.Options{StorePath: db})
	if err != nil {
		t.Fatal(err)
	}
	if recomputed.SnapshotAction != "recomputed" || recomputed.ResumedFrames != 0 {
		t.Fatalf("corrupt snapshot must be discarded, got action=%s resumed=%d reason=%s", recomputed.SnapshotAction, recomputed.ResumedFrames, recomputed.SnapshotReason)
	}
}

func TestParameterVersionMismatchDiscardsSnapshot(t *testing.T) {
	clip, target := fixture(t)
	cfg := model.DefaultConfig()
	db := filepath.Join(t.TempDir(), "version.db")
	if _, err := pipeline.Run(clip, target, cfg, pipeline.Options{StorePath: db, StopAfter: 9}); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE snapshot_meta SET parameter_version='old-version' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	recomputed, err := pipeline.Run(clip, target, cfg, pipeline.Options{StorePath: db})
	if err != nil {
		t.Fatal(err)
	}
	if recomputed.SnapshotAction != "recomputed" || recomputed.ResumedFrames != 0 {
		t.Fatalf("version mismatch must invalidate all frames, got %s resumed=%d", recomputed.SnapshotAction, recomputed.ResumedFrames)
	}
}

func TestFirstDiffLocatesField(t *testing.T) {
	_, target := fixture(t)
	a := []model.FrameResult{{
		Frame: 0, Rotations: []model.Quat{{W: 1}},
		RootTranslation: model.Vec3{}, Contacts: []string{"foot_l"},
		Stats: model.FrameStats{Iterations: 1},
	}}
	b := []model.FrameResult{{
		Frame: 0, Rotations: []model.Quat{{X: 0.1, W: 0.995}},
		RootTranslation: model.Vec3{}, Contacts: []string{"foot_l"},
		Stats: model.FrameStats{Iterations: 1},
	}}
	diff := pipeline.FirstDiff(a, b, target)
	if diff == nil || diff.Frame != 0 || diff.Joint != target.Joints[0].Name || diff.Field == "" {
		t.Fatalf("first diff did not identify frame/joint/field: %#v", diff)
	}
}

func TestOutOfOrderCommitRejected(t *testing.T) {
	clip, target := fixture(t)
	cfg := model.DefaultConfig()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "ordered.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := checkpoint.SnapshotMeta{
		ClipID: clip.ID, TargetID: target.ID,
		ParameterVersion: retarget.ParameterVersion(clip, target, cfg),
		AlgorithmVersion: model.AlgorithmVersion, PlanJSON: "{}",
	}
	if _, err := store.Prepare(meta); err != nil {
		t.Fatal(err)
	}
	err = store.Append(model.FrameResult{Frame: 1, State: "committed"}, meta)
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.ErrState {
		t.Fatalf("out-of-order append must return E_STATE, got %v", err)
	}
}

func fixture(t *testing.T) (model.Clip, model.TargetSkeleton) {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 4208691); err != nil {
		t.Fatal(err)
	}
	clip, err := loader.LoadClip(filepath.Join(dir, "clips", "clip_short.json"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := loader.LoadTarget(filepath.Join(dir, "targets", "target_tall_29.json"))
	if err != nil {
		t.Fatal(err)
	}
	return clip, target
}
