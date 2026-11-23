package ocr_test

import (
	"math"
	"sync"
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/imaging"
	"ocrworkbench/internal/ocr"
	"ocrworkbench/internal/synth"
)

func TestGeneratedSamplesDetectionAndRecognition(t *testing.T) {
	dir := t.TempDir()
	manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	for _, meta := range manifest.Samples {
		t.Run(meta.ID, func(t *testing.T) {
			sample, appErr := imaging.LoadSample(dir, meta.ID)
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
			var candidates []domain.Candidate
			for _, tile := range tiles {
				tileCandidates, appErr := detector.ScanTile(tile, params)
				if appErr != nil {
					t.Fatalf("scan tile %d at %+v: %v (%s)", tile.Index, tile, appErr, appErr.Cause)
				}
				candidates = append(candidates, tileCandidates...)
			}
			lines, appErr := ocr.MergeCandidates(candidates, params)
			if appErr != nil {
				t.Fatalf("merge: %v", appErr)
			}
			if len(lines) != len(sample.Truth.Regions) {
				t.Fatalf("line count = %d, truth = %d", len(lines), len(sample.Truth.Regions))
			}
			recognizer, appErr := ocr.NewRecognizer(fontlib.Version, params.Split)
			if appErr != nil {
				t.Fatalf("recognizer: %v", appErr)
			}
			matches := 0
			for i, line := range lines {
				result, appErr := recognizer.RecognizeLine(sample.Gray, line, params.ConfidenceThreshold)
				if appErr != nil {
					t.Fatalf("recognize %d: %v", i, appErr)
				}
				t.Logf("line %d text=%q conf=%.4f angle=%.2f", i, result.Text, result.Confidence, result.DeskewDegrees)
				for _, char := range result.Characters {
					if char.RunnerUp != "" && char.Confidence-char.RunnerUpScore < 0.08 {
						t.Logf("  char %d %q %.4f runner=%q %.4f", char.Index, char.Rune, char.Confidence, char.RunnerUp, char.RunnerUpScore)
					}
				}
				if i < len(sample.Truth.Regions) && result.Text == sample.Truth.Regions[i].Text {
					matches++
				}
			}
			if matches < len(lines) {
				t.Fatalf("recognized %d/%d lines exactly", matches, len(lines))
			}
		})
	}
}

func TestTiledAndWholeDetectionAgree(t *testing.T) {
	dir := t.TempDir()
	manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	for _, meta := range manifest.Samples {
		sample, appErr := imaging.LoadSample(dir, meta.ID)
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
		var tiledCandidates []domain.Candidate
		for _, tile := range tiles {
			part, appErr := detector.ScanTile(tile, params)
			if appErr != nil {
				t.Fatalf("scan: %v", appErr)
			}
			tiledCandidates = append(tiledCandidates, part...)
		}
		wholeCandidates, appErr := detector.ScanWhole(params)
		if appErr != nil {
			t.Fatalf("whole scan: %v", appErr)
		}
		domain.SortCandidates(tiledCandidates)
		domain.SortCandidates(wholeCandidates)
		if len(tiledCandidates) != len(wholeCandidates) {
			t.Fatalf("candidate count tiled=%d whole=%d", len(tiledCandidates), len(wholeCandidates))
		}
		for i := range tiledCandidates {
			if tiledCandidates[i].BBox != wholeCandidates[i].BBox {
				t.Fatalf("candidate %d mismatch: %+v vs %+v", i, tiledCandidates[i].BBox, wholeCandidates[i].BBox)
			}
		}
		tiledLines, appErr := ocr.MergeCandidates(tiledCandidates, params)
		if appErr != nil {
			t.Fatalf("merge tiled: %v", appErr)
		}
		wholeLines, appErr := ocr.MergeCandidates(wholeCandidates, params)
		if appErr != nil {
			t.Fatalf("merge whole: %v", appErr)
		}
		if len(tiledLines) != len(wholeLines) {
			t.Fatalf("line count tiled=%d whole=%d", len(tiledLines), len(wholeLines))
		}
		for i := range tiledLines {
			if tiledLines[i].BBox != wholeLines[i].BBox {
				t.Fatalf("line %d bbox tiled=%+v whole=%+v", i, tiledLines[i].BBox, wholeLines[i].BBox)
			}
			if math.Abs(tiledLines[i].AvgStrokeWidth-wholeLines[i].AvgStrokeWidth) > 1e-9 {
				t.Fatalf("line %d stroke mismatch", i)
			}
		}
	}
}

func TestTileScanOrderDoesNotChangeResults(t *testing.T) {
	dir := t.TempDir()
	manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	sample, appErr := imaging.LoadSample(dir, manifest.Samples[0].ID)
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
	scan := func(order []domain.Tile) []domain.Candidate {
		var candidates []domain.Candidate
		for _, tile := range order {
			part, appErr := detector.ScanTile(tile, params)
			if appErr != nil {
				t.Fatalf("scan tile %d: %v", tile.Index, appErr)
			}
			candidates = append(candidates, part...)
		}
		domain.SortCandidates(candidates)
		return candidates
	}
	forward := scan(tiles)
	reversedTiles := append([]domain.Tile(nil), tiles...)
	for i, j := 0, len(reversedTiles)-1; i < j; i, j = i+1, j-1 {
		reversedTiles[i], reversedTiles[j] = reversedTiles[j], reversedTiles[i]
	}
	reverse := scan(reversedTiles)
	if len(forward) != len(reverse) {
		t.Fatalf("candidate count forward=%d reverse=%d", len(forward), len(reverse))
	}
	for i := range forward {
		if forward[i].ID != reverse[i].ID || forward[i].BBox != reverse[i].BBox || forward[i].PixelHash != reverse[i].PixelHash {
			t.Fatalf("candidate %d differs by scan order", i)
		}
	}
	forwardLines, appErr := ocr.MergeCandidates(forward, params)
	if appErr != nil {
		t.Fatalf("merge forward: %v", appErr)
	}
	reverseLines, appErr := ocr.MergeCandidates(reverse, params)
	if appErr != nil {
		t.Fatalf("merge reverse: %v", appErr)
	}
	if len(forwardLines) != len(reverseLines) {
		t.Fatalf("line count differs")
	}
	for i := range forwardLines {
		if forwardLines[i].BBox != reverseLines[i].BBox || forwardLines[i].ReadingIndex != reverseLines[i].ReadingIndex {
			t.Fatalf("line %d differs by scan order", i)
		}
	}
}

func TestParallelTileScanIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	sample, appErr := imaging.LoadSample(dir, manifest.Samples[0].ID)
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
	parts := make([][]domain.Candidate, len(tiles))
	errs := make([]*domain.AppError, len(tiles))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range tiles {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			parts[index], errs[index] = detector.ScanTile(tiles[index], params)
		}(i)
	}
	wg.Wait()
	var parallel []domain.Candidate
	for i, appErr := range errs {
		if appErr != nil {
			t.Fatalf("tile %d: %v", i, appErr)
		}
		parallel = append(parallel, parts[i]...)
	}
	domain.SortCandidates(parallel)
	var serial []domain.Candidate
	for _, tile := range tiles {
		part, appErr := detector.ScanTile(tile, params)
		if appErr != nil {
			t.Fatalf("serial tile %d: %v", tile.Index, appErr)
		}
		serial = append(serial, part...)
	}
	domain.SortCandidates(serial)
	if len(parallel) != len(serial) {
		t.Fatalf("parallel=%d serial=%d", len(parallel), len(serial))
	}
	for i := range parallel {
		if parallel[i] != serial[i] {
			t.Fatalf("candidate %d differs under parallel scan", i)
		}
	}
}
