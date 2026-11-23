package engine_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/checkpoint"
	"maskpipeline/internal/engine"
	"maskpipeline/internal/model"
	"maskpipeline/internal/sample"
	"maskpipeline/internal/verify"
	versionstore "maskpipeline/internal/version"
)

func TestPreviewExactThreePathsAndCheckpointRecovery(t *testing.T) {
	ctx := context.Background()
	smp := generateSample(t)
	versions, initial, err := versionstore.New(
		smp.Manifest.Width, smp.Manifest.Height, smp.Manifest.AlgorithmVersion,
		smp.Manifest.DefaultsWithSeeds(),
	)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	opts := engine.DefaultExecutorOptions()
	opts.MaxParallel = 2
	opts.PerBlockBuffer = 1
	eng, err := engine.New(store, versions, opts)
	if err != nil {
		t.Fatal(err)
	}

	preview, err := eng.Preview(ctx, smp)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Meta.WithinDeadline || preview.Meta.LatencyMS > float64(smp.Manifest.PreviewDeadlineMS) {
		t.Fatalf("preview latency = %+v", preview.Meta)
	}
	if preview.Meta.ParamVersion != initial.Version || preview.Meta.DownsampleFactor != 1 {
		t.Fatalf("preview metadata = %+v", preview.Meta)
	}

	incremental, err := eng.Run(ctx, smp, engine.RunRequest{
		SessionID: "incremental", Mode: checkpoint.ModeIncremental, SnapshotVersion: initial.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Status != "completed" {
		t.Fatalf("incremental status = %s", incremental.Status)
	}
	consistency, err := engine.Compare(preview, incremental, smp.Manifest.Tolerances)
	if err != nil {
		t.Fatalf("preview/exact consistency: %v", err)
	}
	if !consistency.WithinTolerance {
		t.Fatalf("consistency = %+v", consistency)
	}

	resumeReq := engine.RunRequest{
		SessionID: "resume", Mode: checkpoint.ModeResume,
		SnapshotVersion: initial.Version, PauseAfterCommits: 5,
	}
	paused, err := eng.Run(ctx, smp, resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	if !paused.Paused || paused.Status != "paused" || len(paused.CommitSequence) != 5 {
		t.Fatalf("paused run = status=%s paused=%v commits=%d", paused.Status, paused.Paused, len(paused.CommitSequence))
	}
	resumeReq.PauseAfterCommits = 0
	resumed, err := eng.Run(ctx, smp, resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || resumed.Paused {
		t.Fatalf("resumed status = %s", resumed.Status)
	}

	full, err := eng.Run(ctx, smp, engine.RunRequest{
		SessionID: "full", Mode: checkpoint.ModeFull, SnapshotVersion: initial.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertThreePathsEqual(t, incremental, resumed, full)

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"incremental", "incremental"},
		{"resume", "resume"},
		{"full", "full"},
	} {
		report, err := verify.Run(ctx, store, smp.Dir, tc.id)
		if err != nil {
			t.Fatalf("verify %s: %v", tc.name, err)
		}
		if report.CleanupMaskHash != incremental.MaskHash || report.StatsHash != incremental.StatsHash {
			t.Fatalf("verify %s hashes differ: %+v", tc.name, report)
		}
	}

	diverged := *preview
	diverged.Labels = append([]byte(nil), preview.Labels...)
	diverged.Stats = preview.Stats
	diverged.Stats.Regions = append([]model.RegionStats(nil), preview.Stats.Regions...)
	diverged.Stats.Regions[0].Area += 100
	diverged.Stats.Regions[0].Centroid.X += 3
	diverged.Labels[0] ^= 0xff
	report, cmpErr := engine.Compare(&diverged, incremental, smp.Manifest.Tolerances)
	if apperr.Code(cmpErr) != apperr.CodeDiverge {
		t.Fatalf("divergence error = %v", cmpErr)
	}
	if report.FirstRegion == 0 || report.FirstDiffPixel == nil {
		t.Fatalf("divergence report = %+v", report)
	}
	ae, _ := apperr.As(cmpErr)
	for _, key := range []string{"region_count_diff", "max_centroid_delta_px", "max_area_rel_error", "first_region", "first_diff_pixel"} {
		if _, ok := ae.Details[key]; !ok {
			t.Fatalf("divergence details missing %s: %+v", key, ae.Details)
		}
	}

	updated := smp.Manifest.DefaultsWithSeeds()
	updated.StructureElementSize = 3
	snapshot2, err := versions.Update(updated)
	if err != nil {
		t.Fatal(err)
	}
	recomputed, err := eng.Run(ctx, smp, engine.RunRequest{
		SessionID: "incremental", Mode: checkpoint.ModeResume, SnapshotVersion: snapshot2.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recomputed.Invalidated || recomputed.Status != "completed" {
		t.Fatalf("restart mismatch did not invalidate and recompute: %+v", recomputed)
	}
	if _, err := verify.Run(ctx, store, smp.Dir, "incremental"); err != nil {
		t.Fatalf("verify recomputed session: %v", err)
	}

	badOpts := opts
	badOpts.InjectBadTile = 1
	badOpts.InjectBadStage = model.StageGrowth
	badEng, err := engine.New(store, versions, badOpts)
	if err != nil {
		t.Fatal(err)
	}
	badResult, badErr := badEng.Run(ctx, smp, engine.RunRequest{
		SessionID: "bad-block", Mode: checkpoint.ModeFull, SnapshotVersion: initial.Version,
	})
	if apperr.Code(badErr) != apperr.CodeBlock || badResult == nil || badResult.Status != "failed" {
		t.Fatalf("bad block result = %+v, err=%v", badResult, badErr)
	}
	assertBlockIsolation(t, ctx, store)
	retryEng, err := engine.New(store, versions, opts)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := retryEng.Run(ctx, smp, engine.RunRequest{
		SessionID: "bad-block", Mode: checkpoint.ModeResume, SnapshotVersion: initial.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != "completed" {
		t.Fatalf("retry status = %s", retried.Status)
	}
	if _, err := verify.Run(ctx, store, smp.Dir, "bad-block"); err != nil {
		t.Fatalf("verify retried block session: %v", err)
	}

	tinyOpts := opts
	tinyOpts.MaxStageBytes = 4
	tinyEng, err := engine.New(store, versions, tinyOpts)
	if err != nil {
		t.Fatal(err)
	}
	bufferResult, bufferErr := tinyEng.Run(ctx, smp, engine.RunRequest{
		SessionID: "buffer-limit", Mode: checkpoint.ModeFull, SnapshotVersion: initial.Version,
	})
	if apperr.Code(bufferErr) != apperr.CodeBlock || bufferResult == nil || bufferResult.Status != "failed" {
		t.Fatalf("buffer limit result = %+v, err=%v", bufferResult, bufferErr)
	}

	stage, ok, err := store.GetStage(ctx, "full", 1, model.StageCleanup)
	if err != nil || !ok {
		t.Fatalf("get stage: %v", err)
	}
	corrupt := append([]byte(nil), stage.Output...)
	corrupt[0] ^= 0xff
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE tile_stages SET output=? WHERE session_id='full' AND tile_index=1 AND stage_name='cleanup'`,
		corrupt); err != nil {
		t.Fatal(err)
	}
	_, verifyErr := verify.Run(ctx, store, smp.Dir, "full")
	if apperr.Code(verifyErr) != apperr.CodeBlock {
		t.Fatalf("corrupt verify error = %v", verifyErr)
	}
	ve, _ := apperr.As(verifyErr)
	if ve.Details["tile"] != 1 || ve.Details["stage"] != model.StageCleanup ||
		ve.Details["x"] != 64 || ve.Details["y"] != 0 {
		t.Fatalf("corrupt verify location = %+v", ve.Details)
	}
}

func generateSample(t *testing.T) *sample.Sample {
	t.Helper()
	root := t.TempDir()
	generated, err := sample.Generate(sample.GenerateOptions{
		Seed:   42,
		OutDir: root,
		Resolutions: []sample.Resolution{
			{Width: 384, Height: 256, TileSize: 64, DeadlineMS: 250},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	smp, err := sample.Load(filepath.Join(root, generated[0].SampleID))
	if err != nil {
		t.Fatal(err)
	}
	return smp
}

func assertThreePathsEqual(t *testing.T, results ...*engine.RunResult) {
	t.Helper()
	if len(results) == 0 {
		t.Fatal("no paths")
	}
	base := results[0]
	for _, got := range results[1:] {
		if !bytes.Equal(base.Labels, got.Labels) {
			t.Fatal("mask pixels differ across paths")
		}
		if base.MaskHash != got.MaskHash {
			t.Fatal("mask hash differs across paths")
		}
		if base.StatsHash != got.StatsHash || model.StatsHash(base.Stats) != model.StatsHash(got.Stats) {
			t.Fatal("statistics differ across paths")
		}
		if strings.Join(base.CommitSequence, ",") != strings.Join(got.CommitSequence, ",") {
			t.Fatal("stage commit sequence differs across paths")
		}
	}
}

func assertBlockIsolation(t *testing.T, ctx context.Context, store *checkpoint.Store) {
	t.Helper()
	stages, err := store.AllStages(ctx, "bad-block")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[model.State]int{}
	failedTile := -1
	for _, stage := range stages {
		if stage.TileIndex == 0 && stage.State != model.StateCommitted {
			t.Fatalf("tile 0 was polluted: %+v", stage)
		}
		if stage.State == model.StateFailed {
			failedTile = stage.TileIndex
		}
		counts[stage.State]++
	}
	if failedTile != 1 || counts[model.StateFailed] != 1 ||
		counts[model.StateCommitted] != len(stages)-3 || counts[model.StatePending] != 2 {
		t.Fatalf("failed tile or state counts are wrong: failed_tile=%d counts=%+v", failedTile, counts)
	}
	for _, stage := range stages {
		if stage.TileIndex != 1 && stage.State != model.StateCommitted {
			t.Fatalf("unrelated tile %d stage %s was polluted: %s", stage.TileIndex, stage.Stage, stage.State)
		}
	}
}
