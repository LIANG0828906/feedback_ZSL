package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/imaging"
	"ocrworkbench/internal/ocr"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/synth"
)

func TestTuningOnlyAffectsUncommittedLines(t *testing.T) {
	sampleDir := t.TempDir()
	if _, appErr := synth.Generate(sampleDir, domain.DefaultBaseSeed); appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	sample, appErr := imaging.LoadSample(sampleDir, "multiline_1280x720")
	if appErr != nil {
		t.Fatalf("load: %v", appErr)
	}
	params := domain.DefaultParams(fontlib.Version)
	detector := ocr.NewDetector(sample.Gray)
	band, appErr := params.EffectiveBand(detector.AvgStroke)
	if appErr != nil {
		t.Fatalf("band: %v", appErr)
	}
	tiles, appErr := imaging.BuildTiles(sample.Width, sample.Height, params.TileSize, band)
	if appErr != nil {
		t.Fatalf("tiles: %v", appErr)
	}
	db, err := store.Open(t.TempDir() + "/tuning.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer db.Close()
	engine := &pipeline.Engine{Store: db, SampleDir: sampleDir, Seed: domain.DefaultBaseSeed}
	control := pipeline.NewControl(params, band, params.StructuralVersion(band))
	control.SetAutoPauseAfter(len(tiles) + 1)
	first, appErr := engine.Run(context.Background(), sample.Meta.ID, params, "live", control, nil)
	if appErr != nil {
		t.Fatalf("first run: %v", appErr)
	}
	if first.Snapshot.Run.Status != "paused" {
		t.Fatalf("expected paused after first line commit, got %s", first.Snapshot.Run.Status)
	}
	committedLines := 0
	for _, line := range first.Snapshot.Lines {
		if line.State == domain.LineCommitted {
			committedLines++
		}
	}
	if committedLines != 1 {
		t.Fatalf("expected exactly one committed line, got %d", committedLines)
	}
	locked := first.Snapshot.Lines[0]
	if locked.Snapshot == nil {
		t.Fatal("committed line has no frozen snapshot")
	}
	lockedJSON, _ := json.Marshal(locked.Snapshot)

	if _, appErr := db.CommitLine(first.RunID, *locked.Snapshot, locked.PayloadHash); appErr == nil || appErr.Code != domain.CodeState {
		t.Fatalf("expected E_STATE on committed line rewrite, got %v", appErr)
	}
	if appErr := db.TransitionLine(first.RunID, 0, domain.LineRecognizing); appErr == nil || appErr.Code != domain.CodeState {
		t.Fatalf("expected E_STATE on committed->recognizing, got %v", appErr)
	}

	tuned := params
	tuned.ConfidenceThreshold = 0.99
	resumeControl := pipeline.NewControl(tuned, band, params.StructuralVersion(band))
	second, appErr := engine.Run(context.Background(), sample.Meta.ID, tuned, "resume", resumeControl, nil)
	if appErr != nil {
		t.Fatalf("resume: %v", appErr)
	}
	if second.Snapshot.Run.Status != "completed" {
		t.Fatalf("resume status = %s", second.Snapshot.Run.Status)
	}
	afterLockedJSON, _ := json.Marshal(second.Snapshot.Lines[0].Snapshot)
	if string(lockedJSON) != string(afterLockedJSON) {
		t.Fatal("committed line snapshot was silently rewritten")
	}
	for _, line := range second.Snapshot.Lines[1:] {
		if line.State != domain.LineCommitted {
			t.Fatalf("line %d state=%s", line.Line.ReadingIndex, line.State)
		}
		if line.Recognition.Accepted {
			t.Fatalf("uncommitted line %d ignored the raised confidence threshold", line.Line.ReadingIndex)
		}
	}
	if !second.Snapshot.Lines[0].Recognition.Accepted {
		t.Fatal("frozen committed line was affected by the threshold change")
	}
}

func TestThreePathConsistencyAllSamples(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full replay matrix in short mode")
	}
	sampleDir := t.TempDir()
	manifest, appErr := synth.Generate(sampleDir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	params := domain.DefaultParams(fontlib.Version)
	for _, sample := range manifest.Samples {
		t.Run(sample.ID, func(t *testing.T) {
			result, appErr := pipeline.Compare(context.Background(), sampleDir, domain.DefaultBaseSeed, sample.ID, params)
			if appErr != nil {
				t.Fatalf("compare: %v", appErr)
			}
			if !result.Equal {
				t.Fatalf("diff: %+v", result.FirstDiff)
			}
			for _, path := range result.Paths {
				if path.Stats.TextAccuracy != 1 {
					t.Fatalf("%s text accuracy=%v", path.Name, path.Stats.TextAccuracy)
				}
				if path.Stats.CountDeviation != 0 {
					t.Fatalf("%s count deviation=%d", path.Name, path.Stats.CountDeviation)
				}
			}
		})
	}
}
