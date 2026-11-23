package pipeline

import (
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/store"
)

func TestComparisonReportsFirstDiffLocation(t *testing.T) {
	sample := &domain.LoadedSample{
		Meta:   domain.SampleMeta{Title: "diff fixture"},
		Width:  100,
		Height: 100,
		Truth: domain.TruthFile{
			Regions: []domain.TruthRegion{{
				LineIndex: 0,
				Text:      "A",
				BBox:      domain.Rect{X: 10, Y: 10, W: 30, H: 20},
			}},
		},
	}
	makeSnapshot := func(text string) store.Snapshot {
		line := domain.DetectedLine{
			ReadingIndex: 0,
			BBox:         domain.Rect{X: 10, Y: 10, W: 30, H: 20},
			Quad: [4]domain.Point{
				{X: 10, Y: 10}, {X: 40, Y: 10}, {X: 40, Y: 30}, {X: 10, Y: 30},
			},
		}
		recognition := domain.LineRecognition{
			Text:       text,
			Confidence: 0.9,
			Accepted:   true,
			Characters: []domain.CharResult{{
				Index:      0,
				Rune:       "A",
				Confidence: 0.9,
				MatchScore: 0.9,
			}},
		}
		return store.Snapshot{
			Run: store.RunRecord{
				ID:       1,
				SampleID: "fixture",
				Status:   "completed",
			},
			Tiles: []store.StoredTile{{
				Tile:        domain.Tile{Index: 0, X: 0, Y: 0, W: 50, H: 50, CoreW: 50, CoreH: 50},
				State:       domain.TileCommitted,
				CommitSeq:   1,
				PayloadHash: "tile-hash",
			}},
			Lines: []store.StoredLine{{
				Line:        line,
				State:       domain.LineCommitted,
				Recognition: recognition,
				CommitSeq:   2,
				PayloadHash: "line-hash-" + text,
			}},
			Commits: []domain.CommitEntry{
				{Seq: 1, Kind: "tile", UnitKey: "tile:000000", PayloadHash: "tile-hash"},
				{Seq: 2, Kind: "line", UnitKey: "line:000000", PayloadHash: "line-hash-" + text},
			},
		}
	}
	base := pathRun{
		name:   "live",
		result: &Result{RunID: 1, Sample: sample, Snapshot: makeSnapshot("A")},
	}
	resume := pathRun{
		name:   "resume",
		result: &Result{RunID: 1, Sample: sample, Snapshot: makeSnapshot("A")},
	}
	reset := pathRun{
		name:   "reset",
		result: &Result{RunID: 1, Sample: sample, Snapshot: makeSnapshot("B")},
	}
	result, appErr := compareRuns([]pathRun{base, resume, reset})
	if appErr == nil || appErr.Code != domain.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", appErr)
	}
	if result.Equal {
		t.Fatal("result marked equal")
	}
	if result.FirstDiff == nil {
		t.Fatal("first diff missing")
	}
	if result.FirstDiff.LineIndex != 0 {
		t.Fatalf("line index = %d", result.FirstDiff.LineIndex)
	}
	if result.FirstDiff.TileX != 0 || result.FirstDiff.TileY != 0 {
		t.Fatalf("tile coordinates = %d,%d", result.FirstDiff.TileX, result.FirstDiff.TileY)
	}
	if result.FirstDiff.DifferenceKey != "text" {
		t.Fatalf("difference key = %s", result.FirstDiff.DifferenceKey)
	}
}
