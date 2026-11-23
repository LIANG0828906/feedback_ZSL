package pipeline_test

import (
	"context"
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/imaging"
	"ocrworkbench/internal/ocr"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/synth"
)

func TestUnitFailureIsIsolatedAndRetryable(t *testing.T) {
	sampleDir := t.TempDir()
	if _, appErr := synth.Generate(sampleDir, domain.DefaultBaseSeed); appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	sample, appErr := imaging.LoadSample(sampleDir, "sparse_single_960x540")
	if appErr != nil {
		t.Fatalf("load: %v", appErr)
	}
	params := domain.DefaultParams(fontlib.Version)
	detector := ocr.NewDetector(sample.Gray)
	band, appErr := params.EffectiveBand(detector.AvgStroke)
	if appErr != nil {
		t.Fatalf("band: %v", appErr)
	}
	db, err := store.Open(t.TempDir() + "/failure.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer db.Close()
	engine := &pipeline.Engine{Store: db, SampleDir: sampleDir, Seed: domain.DefaultBaseSeed}
	firstControl := pipeline.NewControl(params, band, params.StructuralVersion(band))
	firstControl.SetAutoPauseAfter(2)
	first, appErr := engine.Run(context.Background(), sample.Meta.ID, params, "live", firstControl, nil)
	if appErr != nil {
		t.Fatalf("first run: %v", appErr)
	}
	if first.Snapshot.Run.Status != "paused" {
		t.Fatalf("first status = %s", first.Snapshot.Run.Status)
	}
	if len(first.Snapshot.Commits) != 2 {
		t.Fatalf("first commits = %d", len(first.Snapshot.Commits))
	}
	frozen := first.Snapshot.Commits

	failingParams := params
	failingParams.MinCandidates = 10000
	failingControl := pipeline.NewControl(failingParams, band, params.StructuralVersion(band))
	failed, appErr := engine.Run(context.Background(), sample.Meta.ID, failingParams, "resume", failingControl, nil)
	if appErr == nil || appErr.Code != domain.CodeDet {
		t.Fatalf("expected E_DET, got %v", appErr)
	}
	if failed.Snapshot.Run.Status != "failed" {
		t.Fatalf("failed status = %s", failed.Snapshot.Run.Status)
	}
	if len(failed.Snapshot.Commits) != len(frozen) {
		t.Fatalf("prior commits changed during failure")
	}
	for i := range frozen {
		if failed.Snapshot.Commits[i].PayloadHash != frozen[i].PayloadHash {
			t.Fatalf("commit %d was polluted by failed unit", i)
		}
	}
	failedTileIndex := -1
	for _, tile := range failed.Snapshot.Tiles {
		if tile.State == domain.TileFailed {
			failedTileIndex = tile.Tile.Index
			break
		}
	}
	if failedTileIndex < 0 {
		t.Fatal("no failed tile recorded")
	}

	retryControl := pipeline.NewControl(params, band, params.StructuralVersion(band))
	final, appErr := engine.Run(context.Background(), sample.Meta.ID, params, "resume", retryControl, nil)
	if appErr != nil {
		t.Fatalf("retry: %v", appErr)
	}
	if final.Snapshot.Run.Status != "completed" {
		t.Fatalf("final status = %s", final.Snapshot.Run.Status)
	}
	if final.Snapshot.Tiles[failedTileIndex].State != domain.TileCommitted {
		t.Fatalf("failed tile %d was not retried", failedTileIndex)
	}
	if len(final.Snapshot.Commits) != len(final.Snapshot.Tiles)+1 {
		t.Fatalf("final commits=%d tiles=%d", len(final.Snapshot.Commits), len(final.Snapshot.Tiles))
	}
}
