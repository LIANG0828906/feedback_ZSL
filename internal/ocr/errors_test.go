package ocr_test

import (
	"image"
	"testing"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/imaging"
	"ocrworkbench/internal/ocr"
	"ocrworkbench/internal/synth"
)

func TestTemplateAndEmptyLineErrors(t *testing.T) {
	params := domain.DefaultParams(fontlib.Version)
	if _, appErr := ocr.NewRecognizer("", params.Split); appErr == nil || appErr.Code != domain.CodeTmpl {
		t.Fatalf("expected E_TMPL for missing version, got %v", appErr)
	}
	if _, appErr := ocr.NewRecognizer("missing-version", params.Split); appErr == nil || appErr.Code != domain.CodeTmpl {
		t.Fatalf("expected E_TMPL for unknown version, got %v", appErr)
	}
	recognizer, appErr := ocr.NewRecognizer(fontlib.Version, params.Split)
	if appErr != nil {
		t.Fatalf("recognizer: %v", appErr)
	}
	blank := image.NewGray(image.Rect(0, 0, 100, 32))
	for i := range blank.Pix {
		blank.Pix[i] = 255
	}
	line := domain.DetectedLine{
		ReadingIndex:   0,
		BBox:           domain.Rect{X: 0, Y: 0, W: 100, H: 32},
		AvgStrokeWidth: 4,
	}
	result, appErr := recognizer.RecognizeLine(blank, line, 0.5)
	if appErr != nil {
		t.Fatalf("empty recognition: %v", appErr)
	}
	if !result.Empty || result.Text != "" || result.Confidence != 0 {
		t.Fatalf("empty line not explicit: %+v", result)
	}
}

func TestCandidateLowerBoundReturnsDetWithTileCoordinates(t *testing.T) {
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
	params.MinCandidates = 10000
	detector := ocr.NewDetector(sample.Gray)
	band, appErr := params.EffectiveBand(detector.AvgStroke)
	if appErr != nil {
		t.Fatalf("band: %v", appErr)
	}
	tiles, appErr := imaging.BuildTiles(sample.Width, sample.Height, params.TileSize, band)
	if appErr != nil {
		t.Fatalf("tiles: %v", appErr)
	}
	_, appErr = detector.ScanTile(tiles[0], params)
	if appErr == nil || appErr.Code != domain.CodeDet || appErr.TileIndex != tiles[0].Index {
		t.Fatalf("expected E_DET with tile, got %v", appErr)
	}
	if appErr.TileX != tiles[0].X || appErr.TileY != tiles[0].Y {
		t.Fatalf("E_DET tile coords missing: %+v", appErr)
	}
}

func TestInsufficientBandReturnsBandError(t *testing.T) {
	dir := t.TempDir()
	manifest, appErr := synth.Generate(dir, domain.DefaultBaseSeed)
	if appErr != nil {
		t.Fatalf("generate: %v", appErr)
	}
	sample, appErr := imaging.LoadSample(dir, "tilted_low_1024x640")
	if appErr != nil {
		t.Fatalf("load: %v", appErr)
	}
	_ = manifest
	params := domain.DefaultParams(fontlib.Version)
	detector := ocr.NewDetector(sample.Gray)
	tiles, appErr := imaging.BuildTiles(sample.Width, sample.Height, 256, 3)
	if appErr != nil {
		t.Fatalf("tiles: %v", appErr)
	}
	found := false
	for _, tile := range tiles {
		_, scanErr := detector.ScanTile(tile, params)
		if scanErr != nil {
			if scanErr.Code != domain.CodeBand {
				t.Fatalf("unexpected scan error: %v", scanErr)
			}
			if scanErr.TileIndex != tile.Index || scanErr.BreakX == 0 && scanErr.BreakY == 0 {
				t.Fatalf("E_BAND lacks tile/break context: %+v", scanErr)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected an E_BAND failure with a 3px band")
	}
}

func TestReadingOrderConflict(t *testing.T) {
	lines := []domain.DetectedLine{
		{ReadingIndex: 0, BBox: domain.Rect{X: 0, Y: 0, W: 10, H: 10}},
		{ReadingIndex: 0, BBox: domain.Rect{X: 20, Y: 0, W: 10, H: 10}},
	}
	appErr := ocr.ValidateReadingOrder(lines)
	if appErr == nil || appErr.Code != domain.CodeRead {
		t.Fatalf("expected E_READ, got %v", appErr)
	}
}

func TestSimilarityIsMonotonic(t *testing.T) {
	library, appErr := fontlib.Load(fontlib.Version, 24, 32)
	if appErr != nil {
		t.Fatalf("load templates: %v", appErr)
	}
	template, ok := library.TemplateFor('O')
	if !ok {
		t.Fatal("O template missing")
	}
	exact := append([]float64(nil), template.Mask...)
	near := append([]float64(nil), template.Mask...)
	near[0] = 1 - near[0]
	far := append([]float64(nil), exact...)
	for i := 0; i < len(far)/6; i++ {
		far[i] = 1 - far[i]
	}
	exactScore := fontlib.Similarity(exact, template.Width, template.Height, template.Mask, template.Width, template.Height)
	nearScore := fontlib.Similarity(near, template.Width, template.Height, template.Mask, template.Width, template.Height)
	farScore := fontlib.Similarity(far, template.Width, template.Height, template.Mask, template.Width, template.Height)
	if !(exactScore > nearScore && nearScore > farScore) {
		t.Fatalf("similarity not monotonic: exact=%.4f near=%.4f far=%.4f", exactScore, nearScore, farScore)
	}
	for _, score := range []float64{exactScore, nearScore, farScore} {
		if score < 0 || score > 1 {
			t.Fatalf("score outside [0,1]: %v", score)
		}
	}
}
