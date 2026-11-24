package workbench

import (
	"context"
	"database/sql"
	"errors"
	"image"
	"path/filepath"
	"strings"
	"testing"

	"yuvench/errs"
	"yuvench/filters"
	"yuvench/yuv"
)

type fixture struct {
	image *yuv.Image
	path  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	manifest, err := yuv.GenerateSamples(dir, 20261124)
	if err != nil {
		t.Fatal(err)
	}
	image, err := yuv.LoadImage(manifest, "game_tile_64x48_8bit_rgb")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{image: image, path: manifest}
}

func changedParams() filters.Params {
	params := filters.DefaultParams()
	params.YSharpen.Amount = 0.60
	params.UGain.Gain = 1.05
	return params
}

func runOnce(t *testing.T, dbPath string, image *yuv.Image, params filters.Params, options RunOptions) *RunResult {
	t.Helper()
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	result, err := engine.Run(context.Background(), image, params, filters.DefaultLimits(), options)
	engine.Close()
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIncrementalSelectivityAndPixelConsistency(t *testing.T) {
	fixture := newFixture(t)
	store, err := OpenStore(filepath.Join(t.TempDir(), "incremental.db"))
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	if _, err := engine.Run(context.Background(), fixture.image, filters.DefaultParams(), filters.DefaultLimits(), RunOptions{Mode: "full"}); err != nil {
		t.Fatal(err)
	}
	incremental, err := engine.Run(context.Background(), fixture.image, changedParams(), filters.DefaultLimits(), RunOptions{Mode: "incremental"})
	engine.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental.Invalidations) != 4 {
		t.Fatalf("invalidated %d stages, want 4: %+v", len(incremental.Invalidations), incremental.Invalidations)
	}
	if incremental.Stats.Recomputed != 4 || incremental.Stats.CacheHits != 4 {
		t.Fatalf("recomputed=%d hits=%d", incremental.Stats.Recomputed, incremental.Stats.CacheHits)
	}
	full := runOnce(t, filepath.Join(t.TempDir(), "full.db"), fixture.image, changedParams(), RunOptions{Mode: "full"})
	if err := CompareRuns(incremental, full, errs.CodeDiff); err != nil {
		t.Fatalf("incremental/full mismatch: %v", err)
	}
	if incremental.Final.Width != fixture.image.Meta.Width || incremental.Final.Height != fixture.image.Meta.Height {
		t.Fatal("final dimensions changed")
	}
}

func TestPauseRestartResumeConsistency(t *testing.T) {
	fixture := newFixture(t)
	dbPath := filepath.Join(t.TempDir(), "resume.db")
	paused := runOnce(t, dbPath, fixture.image, changedParams(), RunOptions{Mode: "full", PauseAfterCommits: 3})
	if !paused.Paused || len(paused.CommitSequence) != 3 || paused.Final != nil {
		t.Fatalf("unexpected pause result: paused=%v commits=%d final=%v", paused.Paused, len(paused.CommitSequence), paused.Final)
	}
	resumed := runOnce(t, dbPath, fixture.image, changedParams(), RunOptions{Mode: "resume"})
	if resumed.Stats.CacheHits != 3 || resumed.Stats.Recomputed != 5 {
		t.Fatalf("resume recomputed=%d hits=%d", resumed.Stats.Recomputed, resumed.Stats.CacheHits)
	}
	full := runOnce(t, filepath.Join(t.TempDir(), "full.db"), fixture.image, changedParams(), RunOptions{Mode: "full"})
	if err := CompareRuns(resumed, full, errs.CodePath); err != nil {
		t.Fatalf("resume/full mismatch: %v", err)
	}
}

func TestActivePauseSignal(t *testing.T) {
	fixture := newFixture(t)
	dbPath := filepath.Join(t.TempDir(), "pause-signal.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	pause := make(chan struct{})
	close(pause)
	result, err := engine.Run(context.Background(), fixture.image, changedParams(), filters.DefaultLimits(), RunOptions{
		Mode:  "full",
		Pause: pause,
	})
	engine.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Paused || result.Complete || len(result.CommitSequence) != 0 {
		t.Fatalf("active pause was not honored: %+v", result)
	}
}

func TestSnapshotTamperForcesFullInvalidation(t *testing.T) {
	fixture := newFixture(t)
	dbPath := filepath.Join(t.TempDir(), "version.db")
	runOnce(t, dbPath, fixture.image, changedParams(), RunOptions{Mode: "full"})
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE run_meta SET param_version='tampered' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	result := runOnce(t, dbPath, fixture.image, changedParams(), RunOptions{Mode: "incremental"})
	if !strings.HasPrefix(result.SnapshotReason, "snapshot-version-mismatch:") {
		t.Fatalf("snapshot reason=%q", result.SnapshotReason)
	}
	if result.Stats.Recomputed != 8 || result.Stats.CacheHits != 0 {
		t.Fatalf("recomputed=%d hits=%d", result.Stats.Recomputed, result.Stats.CacheHits)
	}
	if len(result.Invalidations) != 8 {
		t.Fatalf("invalidations=%d", len(result.Invalidations))
	}
}

func TestFailureRetryDoesNotPolluteCache(t *testing.T) {
	fixture := newFixture(t)
	dbPath := filepath.Join(t.TempDir(), "retry.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	if _, err := engine.Run(context.Background(), fixture.image, changedParams(), filters.DefaultLimits(), RunOptions{Mode: "full"}); err != nil {
		t.Fatal(err)
	}
	retryParams := changedParams()
	retryParams.YSharpen.Amount = 0.75
	faulted := false
	failedResult, failedErr := engine.Run(context.Background(), fixture.image, retryParams, filters.DefaultLimits(), RunOptions{
		Mode: "incremental",
		Fault: func(node string) error {
			if node == filters.NodeYSharpen && !faulted {
				faulted = true
				return errors.New("injected")
			}
			return nil
		},
	})
	if failedErr == nil || errs.CodeOf(failedErr) != errs.CodeStage || failedResult.Stats.Failed != 1 {
		t.Fatalf("expected E_STAGE, got %v", failedErr)
	}
	retry, err := engine.Run(context.Background(), fixture.image, retryParams, filters.DefaultLimits(), RunOptions{Mode: "incremental"})
	engine.Close()
	if err != nil {
		t.Fatal(err)
	}
	if retry.Stats.Recomputed != 3 || retry.Stats.CacheHits != 5 {
		t.Fatalf("retry recomputed=%d hits=%d", retry.Stats.Recomputed, retry.Stats.CacheHits)
	}
	full := runOnce(t, filepath.Join(t.TempDir(), "full.db"), fixture.image, retryParams, RunOptions{Mode: "full"})
	if err := CompareRuns(retry, full, errs.CodeDiff); err != nil {
		t.Fatalf("retry mismatch: %v", err)
	}
}

func TestCancelPersistsOnlyCommittedStages(t *testing.T) {
	fixture := newFixture(t)
	dbPath := filepath.Join(t.TempDir(), "cancel.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := engine.Run(ctx, fixture.image, changedParams(), filters.DefaultLimits(), RunOptions{Mode: "full"})
	engine.Close()
	if errs.CodeOf(err) != errs.CodeCancel {
		t.Fatalf("expected E_CANCEL, got %v", err)
	}
	if result == nil || len(result.CommitSequence) != 0 {
		t.Fatalf("unexpected cancelled result: %+v", result)
	}
}

func TestHeatmapDeterministicAndValidated(t *testing.T) {
	fixture := newFixture(t)
	full := runOnce(t, filepath.Join(t.TempDir(), "heat.db"), fixture.image, changedParams(), RunOptions{Mode: "full"})
	first, firstStats, err := Heatmap(full.Original, full.Final, 63.75)
	if err != nil {
		t.Fatal(err)
	}
	second, secondStats, err := Heatmap(full.Original, full.Final, 63.75)
	if err != nil {
		t.Fatal(err)
	}
	if firstStats.Summary != secondStats.Summary || !pixelsEqual(first, second) {
		t.Fatal("heatmap is not deterministic")
	}
	_, _, err = Heatmap(full.Original, full.Final, 0)
	if errs.CodeOf(err) != errs.CodeParameter {
		t.Fatalf("expected E_PARA, got %v", err)
	}
}

func TestCompareReportsFirstStageAndPixel(t *testing.T) {
	fixture := newFixture(t)
	first := runOnce(t, filepath.Join(t.TempDir(), "a.db"), fixture.image, filters.DefaultParams(), RunOptions{Mode: "full"})
	second := runOnce(t, filepath.Join(t.TempDir(), "b.db"), fixture.image, changedParams(), RunOptions{Mode: "full"})
	err := CompareRuns(first, second, errs.CodeDiff)
	business, ok := errs.As(err)
	if !ok || business.Code != errs.CodeDiff || business.Node == "" || business.Plane == "" || business.Stage == "" || business.X == nil || business.Y == nil || business.LeftValue == nil || business.RightValue == nil {
		t.Fatalf("incomplete mismatch report: %#v (%v)", business, err)
	}
}

func pixelsEqual(left, right *image.RGBA) bool {
	if left == nil || right == nil || len(left.Pix) != len(right.Pix) {
		return false
	}
	for index := range left.Pix {
		if left.Pix[index] != right.Pix[index] {
			return false
		}
	}
	return true
}
