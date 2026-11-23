package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/synth"
)

func main() {
	sampleDir := flag.String("samples", "samples", "deterministic sample directory")
	seed := flag.Int64("seed", domain.DefaultBaseSeed, "sample generator seed")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	manifest, appErr := synth.Ensure(*sampleDir, *seed)
	if appErr != nil {
		fail(appErr)
	}
	fmt.Printf("OCR WORKBENCH VERIFICATION\n")
	fmt.Printf("algorithm=%s template=%s seed=%d samples=%d dir=%s\n\n",
		domain.AlgorithmVersion, manifest.FontVersion, manifest.Seed, len(manifest.Samples), filepath.Clean(*sampleDir))

	params := domain.DefaultParams(fontlib.Version)
	allOK := true
	for _, meta := range manifest.Samples {
		start := time.Now()
		result, appErr := pipeline.Compare(ctx, *sampleDir, *seed, meta.ID, params)
		if appErr != nil {
			fmt.Printf("FAIL %-26s %s %s\n", meta.ID, appErr.Code, appErr.Message)
			allOK = false
			continue
		}
		live := result.Paths[0]
		status := "PASS"
		if !result.Equal {
			status = "DIFF"
			allOK = false
		}
		fmt.Printf("%s %-26s lines=%d chars=%d acc=%.3f iou=%.3f commits=%d time=%s\n",
			status,
			meta.ID,
			live.Stats.DetectedLines,
			live.Stats.CharCount,
			live.Stats.TextAccuracy,
			live.Stats.AvgTruthIoU,
			result.ComparedCommits,
			time.Since(start).Round(time.Millisecond),
		)
		fmt.Printf("     tiles_equal=%t lines_equal=%t commits_equal=%t confidence=%.4f\n",
			result.TilesEqual, result.LinesEqual, result.CommitsEqual, live.Stats.AvgConfidence)
		if result.FirstDiff != nil {
			fmt.Printf("     first_diff code=%s field=%s tile=%d@%d,%d line=%d\n",
				result.FirstDiff.Code, result.FirstDiff.DifferenceKey,
				result.FirstDiff.TileIndex, result.FirstDiff.TileX, result.FirstDiff.TileY,
				result.FirstDiff.LineIndex)
		}

	}
	if !allOK {
		os.Exit(1)
	}
	fmt.Println("\nRESULT: all deterministic samples passed the three-path consistency gate.")
}

func fail(appErr *domain.AppError) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", appErr.Code, appErr.Message)
	os.Exit(2)
}
