package vision_test

import (
	"errors"
	"image"
	"image/color"
	"math"
	"reflect"
	"testing"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/loader"
	"ocrworkbench/internal/sample"
	"ocrworkbench/internal/vision"
)

func TestDeterministicSamplesMatchTruthAndTileConsistency(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	params := core.DefaultDetectorParams()
	segment := core.DefaultSegmentParams()
	recognizer := vision.Recognizer{Params: segment}
	for _, truth := range manifest.Images {
		t.Run(truth.ID, func(t *testing.T) {
			loaded, err := loader.Load(dir, truth)
			if err != nil {
				t.Fatal(err)
			}
			gray := vision.ToGray(loaded.Image)
			whole, err := vision.DetectWhole(gray, params)
			if err != nil {
				t.Fatal(err)
			}
			wholeAgain, err := vision.DetectWhole(vision.ToGray(loaded.Image), params)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(whole, wholeAgain) {
				t.Fatal("same image and parameters produced different candidates")
			}
			var candidates []core.TextLine
			for _, tile := range vision.PlanTiles(loaded.Width, loaded.Height, params) {
				lines, err := vision.DetectTile(gray, tile, params)
				if err != nil {
					t.Fatal(err)
				}
				candidates = append(candidates, lines...)
			}
			tiled, err := vision.MergeTileCandidates(candidates, params)
			if err != nil {
				t.Fatal(err)
			}
			if absInt(len(whole)-len(truth.Regions)) > truth.DetectionTolerance {
				t.Fatalf("whole-image line count %d, truth %d", len(whole), len(truth.Regions))
			}
			if absInt(len(tiled)-len(truth.Regions)) > truth.DetectionTolerance {
				t.Fatalf("tiled line count %d, truth %d", len(tiled), len(truth.Regions))
			}
			if err := vision.ValidateTileConsistency(whole, tiled, truth.DetectionTolerance, truth.MinDetectionIoU); err != nil {
				t.Fatal(err)
			}
			checkDetectedText(t, recognizer, loaded.Image, whole, truth)
			checkDetectedText(t, recognizer, loaded.Image, tiled, truth)
		})
	}
}

func checkDetectedText(t *testing.T, recognizer vision.Recognizer, img image.Image, lines []core.TextLine, truth core.ImageTruth) {
	t.Helper()
	for i, line := range lines {
		result, err := recognizer.Recognize(img, line)
		if err != nil {
			t.Fatal(err)
		}
		if result.Text != truth.Regions[i].Text {
			t.Fatalf("line %d recognized %q, truth %q", i, result.Text, truth.Regions[i].Text)
		}
		if result.IsEmpty {
			t.Fatalf("line %d was incorrectly marked empty", i)
		}
		if result.Confidence < 0 || result.Confidence > 1 {
			t.Fatalf("line %d confidence %.6f outside [0,1]", i, result.Confidence)
		}
		for j, char := range result.Characters {
			if char.Confidence < 0 || char.Confidence > 1 {
				t.Fatalf("line %d char %d confidence %.6f outside [0,1]", i, j, char.Confidence)
			}
		}
		iou := core.BoxIoU(line.Box, truth.Regions[i].Box)
		if iou < truth.MinDetectionIoU {
			t.Fatalf("line %d IoU %.4f below %.4f", i, iou, truth.MinDetectionIoU)
		}
		if math.Abs(truth.Regions[i].AngleDegrees) > 0.01 {
			truthArea := core.BoxArea(truth.Regions[i].Box)
			detectedArea := core.BoxArea(line.Box)
			deviation := math.Abs(detectedArea-truthArea) / truthArea
			if deviation > 0.35 {
				t.Fatalf("tilted line %d external area deviation %.3f too large", i, deviation)
			}
		}
	}
}

func TestSegmentationErrorsEmptyLineAndConfidenceMonotonicity(t *testing.T) {
	params := core.DefaultSegmentParams()
	params.MaxCharWidth = 1
	err := core.ValidateSegmentParams(params)
	var typed *core.Error
	if !errors.As(err, &typed) || typed.Code != core.CodePara || typed.Field != "max_char_width" || typed.Range != "[4,1024]" {
		t.Fatalf("unexpected segmentation error: %+v", typed)
	}

	blank := image.NewRGBA(image.Rect(0, 0, 160, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 160; x++ {
			blank.SetRGBA(x, y, color.RGBA{R: 24, G: 27, B: 31, A: 255})
		}
	}
	line := core.TextLine{
		ID:             0,
		Box:            core.AxisBox(core.Rect{X: 10, Y: 10, Width: 100, Height: 30}),
		StrokeWidth:    3,
		ComponentCount: 0,
		SourceTiles:    []int{0},
	}
	recognizer := vision.Recognizer{Params: core.DefaultSegmentParams()}
	result, err := recognizer.Recognize(blank, line)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsEmpty || result.Text != "" || result.Confidence != 0 || result.EmptyReason == "" {
		t.Fatalf("blank line was not explicitly retained as empty: %+v", result)
	}

	lowMargin := vision.ConfidenceFromScores(0.9, 0.8, 2.5)
	highMargin := vision.ConfidenceFromScores(0.9, 0.2, 2.5)
	if !(highMargin > lowMargin && lowMargin >= 0 && highMargin <= 1) {
		t.Fatalf("confidence is not monotonic in template margin: %.4f %.4f", lowMargin, highMargin)
	}
}

func TestBandDeficiencyReturnsTileAndBoundary(t *testing.T) {
	params := core.DefaultDetectorParams()
	params.Band = core.RequiredBand(params) - 1
	err := core.ValidateDetectorParams(params)
	var typed *core.Error
	if !errors.As(err, &typed) {
		t.Fatal(err)
	}
	if typed.Code != core.CodeBand || typed.Boundary == "" {
		t.Fatalf("unexpected band error: %+v", typed)
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
