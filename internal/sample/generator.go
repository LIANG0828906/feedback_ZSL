package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/font"
)

const GeneratorVersion = "synthetic-ocr-generator-v1"

type Options struct {
	OutputDir string
	Seed      int64
}

type lineSpec struct {
	Text       string
	Scale      int
	CenterX    float64
	CenterY    float64
	AngleDeg   float64
	Foreground color.RGBA
}

type imageSpec struct {
	ID          string
	File        string
	Width       int
	Height      int
	Background  color.RGBA
	Description string
	Lines       []lineSpec
}

// Generate writes a deterministic manifest and PNG files. Every truth field is
// derived from drawing construction; no image analysis is used to make truth.
func Generate(opts Options) (core.Manifest, error) {
	if opts.OutputDir == "" {
		return core.Manifest{}, fmt.Errorf("output directory is required")
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return core.Manifest{}, err
	}
	if err := font.Validate(); err != nil {
		return core.Manifest{}, err
	}
	manifest := core.Manifest{
		SchemaVersion:      1,
		GeneratorVersion:   GeneratorVersion,
		Seed:               opts.Seed,
		TemplateVersion:    font.Version,
		CreatedBy:          "ocrgen",
		DetectionTolerance: 0,
		MinDetectionIoU:    0.55,
	}
	for _, spec := range imageSpecs(opts.Seed) {
		truth, err := renderImage(opts.OutputDir, spec)
		if err != nil {
			return core.Manifest{}, err
		}
		manifest.Images = append(manifest.Images, truth)
	}
	for i := range manifest.Images {
		manifest.Images[i].DetectionTolerance = manifest.DetectionTolerance
		manifest.Images[i].MinDetectionIoU = manifest.MinDetectionIoU
	}
	fingerprint, err := core.TruthFingerprint(manifest)
	if err != nil {
		return core.Manifest{}, err
	}
	manifest.TruthFingerprint = fingerprint
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return core.Manifest{}, err
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(opts.OutputDir, "manifest.json"), b, 0o644); err != nil {
		return core.Manifest{}, err
	}
	return manifest, nil
}

func LoadManifest(dir string) (core.Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return core.Manifest{}, err
	}
	var manifest core.Manifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		return core.Manifest{}, err
	}
	return manifest, nil
}

func imageSpecs(seed int64) []imageSpec {
	// Seed changes colors and sampled text while preserving the declared fixed
	// layouts. The layouts cover single-line, sparse multi-line and dense
	// multi-line content at three resolutions, with skew and low contrast.
	jitter := float64((seed%7)-3) * 2
	return []imageSpec{
		{
			ID:          "dense_1280",
			File:        "dense_1280.png",
			Width:       1280,
			Height:      720,
			Background:  color.RGBA{R: 22, G: 32, B: 46, A: 255},
			Description: "dense multi-line, low contrast, positive and negative skew",
			Lines: []lineSpec{
				{Text: "GAME2048", Scale: 5, CenterX: 340 + jitter, CenterY: 85, AngleDeg: -6},
				{Text: "SCORE9000", Scale: 5, CenterX: 720, CenterY: 85, AngleDeg: 5},
				{Text: "COMBO42", Scale: 4, CenterX: 330, CenterY: 190, AngleDeg: 0, Foreground: color.RGBA{R: 140, G: 174, B: 184, A: 255}},
				{Text: "LEVEL07", Scale: 4, CenterX: 700, CenterY: 190, AngleDeg: -4},
				{Text: "PLAYERONE", Scale: 4, CenterX: 460, CenterY: 290, AngleDeg: 7, Foreground: color.RGBA{R: 119, G: 135, B: 132, A: 255}},
				{Text: "READY", Scale: 4, CenterX: 860, CenterY: 290, AngleDeg: 3},
				{Text: "SHIELDS3", Scale: 3, CenterX: 380, CenterY: 410, AngleDeg: -3},
				{Text: "WAVE12", Scale: 3, CenterX: 800, CenterY: 410, AngleDeg: 6, Foreground: color.RGBA{R: 99, G: 123, B: 137, A: 255}},
			},
		},
		{
			ID:          "sparse_960",
			File:        "sparse_960.png",
			Width:       960,
			Height:      540,
			Background:  color.RGBA{R: 41, G: 35, B: 48, A: 255},
			Description: "sparse multi-line with generous whitespace",
			Lines: []lineSpec{
				{Text: "PAUSED", Scale: 6, CenterX: 470, CenterY: 120, AngleDeg: 0},
				{Text: "CONTINUE", Scale: 4, CenterX: 470, CenterY: 290, AngleDeg: -8, Foreground: color.RGBA{R: 146, G: 128, B: 154, A: 255}},
				{Text: "QUIT", Scale: 4, CenterX: 470, CenterY: 430, AngleDeg: 4},
			},
		},
		{
			ID:          "single_800",
			File:        "single_800.png",
			Width:       800,
			Height:      450,
			Background:  color.RGBA{R: 15, G: 36, B: 31, A: 255},
			Description: "single line, low contrast",
			Lines: []lineSpec{
				{Text: "NEWGAME", Scale: 7, CenterX: 400, CenterY: 225, AngleDeg: 5, Foreground: color.RGBA{R: 83, G: 120, B: 105, A: 255}},
			},
		},
		{
			ID:          "mixed_1024",
			File:        "mixed_1024.png",
			Width:       1024,
			Height:      600,
			Background:  color.RGBA{R: 33, G: 28, B: 23, A: 255},
			Description: "mixed sparse and dense multi-line layout",
			Lines: []lineSpec{
				{Text: "HP100", Scale: 5, CenterX: 190, CenterY: 100, AngleDeg: -5},
				{Text: "MP50", Scale: 5, CenterX: 790, CenterY: 100, AngleDeg: 5},
				{Text: "BOSSWAVE", Scale: 5, CenterX: 500, CenterY: 225, AngleDeg: 0, Foreground: color.RGBA{R: 132, G: 113, B: 79, A: 255}},
				{Text: "CHECKPOINT", Scale: 3, CenterX: 500, CenterY: 345, AngleDeg: -7},
				{Text: "AUTOSAVE", Scale: 3, CenterX: 500, CenterY: 420, AngleDeg: 7},
				{Text: "LOADING", Scale: 3, CenterX: 500, CenterY: 495, AngleDeg: 0, Foreground: color.RGBA{R: 89, G: 82, B: 69, A: 255}},
			},
		},
	}
}

func renderImage(dir string, spec imageSpec) (core.ImageTruth, error) {
	img := image.NewRGBA(image.Rect(0, 0, spec.Width, spec.Height))
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			// A deterministic, very subtle vignette exercises normalization and
			// prevents hard-coded pure-color assumptions.
			shade := int(float64((x+2*y)%11) - 5)
			bg := spec.Background
			bg.R = clampByte(int(bg.R) + shade)
			bg.G = clampByte(int(bg.G) + shade)
			bg.B = clampByte(int(bg.B) + shade)
			img.SetRGBA(x, y, bg)
		}
	}
	// Keep the encoded PNG in truecolor+alpha form even when every visible
	// pixel is opaque. This makes the declared channel count stable across Go
	// standard-library PNG encoder optimizations.
	corner := img.RGBAAt(0, 0)
	corner.A = 254
	img.SetRGBA(0, 0, corner)
	truth := core.ImageTruth{
		ID:                  spec.ID,
		File:                spec.File,
		Width:               spec.Width,
		Height:              spec.Height,
		Channels:            4,
		DeclaredRegionCount: len(spec.Lines),
		Description:         spec.Description,
	}
	for index, line := range spec.Lines {
		region, err := drawLine(img, index, line)
		if err != nil {
			return core.ImageTruth{}, fmt.Errorf("%s line %d: %w", spec.ID, index, err)
		}
		truth.Regions = append(truth.Regions, region)
	}
	path := filepath.Join(dir, spec.File)
	file, err := os.Create(path)
	if err != nil {
		return core.ImageTruth{}, err
	}
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	encodeErr := encoder.Encode(file, img)
	closeErr := file.Close()
	if encodeErr != nil {
		return core.ImageTruth{}, encodeErr
	}
	if closeErr != nil {
		return core.ImageTruth{}, closeErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return core.ImageTruth{}, err
	}
	sum := sha256.Sum256(b)
	truth.SHA256 = hex.EncodeToString(sum[:])
	return truth, nil
}

func drawLine(img *image.RGBA, id int, spec lineSpec) (core.TruthRegion, error) {
	runes := []rune(strings.ToUpper(spec.Text))
	if len(runes) == 0 {
		return core.TruthRegion{}, fmt.Errorf("empty construction text is not supported in image layout")
	}
	for _, r := range runes {
		if _, ok := font.Pattern(r); !ok {
			return core.TruthRegion{}, fmt.Errorf("no template for %q", r)
		}
	}
	scale := spec.Scale
	if scale < 2 || scale > 12 {
		return core.TruthRegion{}, fmt.Errorf("invalid scale %d", scale)
	}
	cellW := (font.Width + 1) * scale
	width := len(runes)*cellW - scale
	height := font.Height * scale
	x0 := spec.CenterX - float64(width)/2
	y0 := spec.CenterY - float64(height)/2
	theta := spec.AngleDeg * math.Pi / 180
	cosA, sinA := math.Cos(theta), math.Sin(theta)
	if spec.Foreground.A == 0 {
		spec.Foreground = color.RGBA{R: 221, G: 232, B: 214, A: 255}
	}
	localCX := x0 + float64(width)/2
	localCY := y0 + float64(height)/2

	rotated := make([]core.Point, 0, 4)
	for _, corner := range [][2]float64{
		{x0, y0}, {x0 + float64(width), y0},
		{x0 + float64(width), y0 + float64(height)}, {x0, y0 + float64(height)},
	} {
		rotated = append(rotated, rotatePoint(corner[0], corner[1], localCX, localCY, cosA, sinA))
	}
	bounds := core.BoxBounds(core.Box(rotated))

	minLX, minLY := math.Inf(1), math.Inf(1)
	maxLX, maxLY := math.Inf(-1), math.Inf(-1)
	for py := bounds.Y; py < bounds.Bottom(); py++ {
		for px := bounds.X; px < bounds.Right(); px++ {
			dx := float64(px) + 0.5 - spec.CenterX
			dy := float64(py) + 0.5 - spec.CenterY
			lx := dx*cosA + dy*sinA + localCX
			ly := -dx*sinA + dy*cosA + localCY
			if lx < x0 || ly < y0 || lx >= x0+float64(width) || ly >= y0+float64(height) {
				continue
			}
			relX := lx - x0
			relY := ly - y0
			charIndex := int(relX / float64(cellW))
			if charIndex < 0 || charIndex >= len(runes) {
				continue
			}
			withinCell := int(relX) % cellW
			gx := withinCell / scale
			gy := int(relY) / scale
			if gx < 0 || gx >= font.Width || gy < 0 || gy >= font.Height {
				continue
			}
			pattern, _ := font.Pattern(runes[charIndex])
			if pattern[gy][gx] != '#' {
				continue
			}
			img.SetRGBA(px, py, spec.Foreground)
			minLX = math.Min(minLX, lx)
			maxLX = math.Max(maxLX, lx)
			minLY = math.Min(minLY, ly)
			maxLY = math.Max(maxLY, ly)
		}
	}
	if !finite(minLX) {
		return core.TruthRegion{}, fmt.Errorf("line produced no ink")
	}
	// Truth corners are the actual constructed ink extent transformed by the
	// same affine rotation used for drawing.
	box := core.Box{
		rotatePoint(minLX, minLY, localCX, localCY, cosA, sinA),
		rotatePoint(maxLX, minLY, localCX, localCY, cosA, sinA),
		rotatePoint(maxLX, maxLY, localCX, localCY, cosA, sinA),
		rotatePoint(minLX, maxLY, localCX, localCY, cosA, sinA),
	}
	return core.TruthRegion{
		ID:             id,
		LineIndex:      id,
		Box:            box,
		Text:           string(runes),
		CharCount:      len(runes),
		AvgStrokeWidth: float64(scale),
		AngleDegrees:   spec.AngleDeg,
	}, nil
}

func rotatePoint(x, y, cx, cy, cosA, sinA float64) core.Point {
	dx, dy := x-cx, y-cy
	return core.Point{
		X: cx + dx*cosA - dy*sinA,
		Y: cy + dx*sinA + dy*cosA,
	}
}

func TruthFingerprint(manifest core.Manifest) (string, error) {
	return core.TruthFingerprint(manifest)
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
