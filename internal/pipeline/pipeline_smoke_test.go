package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"iconbench/internal/checkpoint"
	"iconbench/internal/model"
	"iconbench/internal/sample"
)

func TestBlockPipelineMatchesTruth(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	manifest, err := sample.Generate(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(dataDir, "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pl := &Pipeline{Manifest: manifest, DataDir: dataDir, Store: store, Pool: NewResourcePool(manifest.Budget)}
	for _, rec := range manifest.Images {
		params := model.DefaultParams()
		params.ParamVersion = 1
		res, err := pl.RunImage(context.Background(), rec.ID, params, RunOptions{})
		if err != nil {
			t.Fatalf("%s: %v", rec.ID, err)
		}
		t.Logf("%s regions=%d iou=%.4f mismatch=%d cent=%.3f area=%.3f within=%v seq=%d", rec.ID, len(res.Stats), res.Metrics.IoU, res.Metrics.MismatchPixels, res.Metrics.MaxCentroidErrorPx, res.Metrics.MaxAreaRelativeError, res.Report.WithinTolerance, len(res.Sequence))
		if res.Report.StateError != nil {
			t.Fatalf("%s state error: %+v", rec.ID, res.Report.StateError)
		}
		if !res.Report.WithinTolerance {
			t.Fatalf("%s outside tolerance: cent=%.3f area=%.3f", rec.ID, res.Report.MaxCentroidErrorPx, res.Report.MaxAreaRelativeError)
		}
	}
}
