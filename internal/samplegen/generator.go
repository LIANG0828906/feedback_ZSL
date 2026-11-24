package samplegen

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"integritykernel/internal/core"
	"integritykernel/internal/loader"
	"integritykernel/internal/watermark"
)

type Options struct {
	OutputDir string
	Seed      int64
	Frames    int
	Force     bool
}

func DefaultOptions(dir string) Options {
	return Options{OutputDir: dir, Seed: 0x42A8691E, Frames: 24, Force: true}
}

func Generate(opts Options) (*core.Manifest, error) {
	if opts.Frames <= 0 {
		opts.Frames = 24
	}
	if opts.Seed == 0 {
		opts.Seed = 0x42A8691E
	}
	if opts.OutputDir == "" {
		return nil, core.NewError(core.CodeMeta, "sample output directory is required")
	}
	if !opts.Force {
		if _, err := os.Stat(filepath.Join(opts.OutputDir, "truth.json")); err == nil {
			seq, err := loader.Load(opts.OutputDir)
			if err != nil {
				return nil, err
			}
			return &seq.Manifest, nil
		}
	}

	width, height := 192, 108
	if err := os.MkdirAll(filepath.Join(opts.OutputDir, "frames"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opts.OutputDir, "baseline"), 0o755); err != nil {
		return nil, err
	}

	params := core.DefaultParams()
	region := core.Rect{X: 104, Y: 8, Width: 80, Height: 24}
	patch := core.Rect{X: 96, Y: 0, Width: 88, Height: 44}
	replacementStart, replacementEnd := 13, 16
	if replacementEnd >= opts.Frames {
		replacementStart = opts.Frames / 2
		replacementEnd = replacementStart + 3
		if replacementEnd >= opts.Frames {
			replacementEnd = opts.Frames - 1
		}
	}
	sourceFrame := 5
	if sourceFrame >= opts.Frames {
		sourceFrame = 0
	}
	if sourceFrame >= replacementStart {
		sourceFrame = 0
	}

	primitives := []core.Primitive{
		{ID: "rect-a", Kind: "rectangle", BBox: core.Rect{X: 18, Y: 26, Width: 46, Height: 30}, Orientation: "horizontal/vertical"},
		{ID: "circle-b", Kind: "circle", BBox: core.Rect{X: 108, Y: 42, Width: 48, Height: 48}, Orientation: "radial"},
		{ID: "triangle-c", Kind: "triangle", BBox: core.Rect{X: 70, Y: 34, Width: 66, Height: 58}, Orientation: "diagonal"},
		{ID: "bar-d", Kind: "rectangle", BBox: core.Rect{X: 18, Y: 76, Width: 58, Height: 10}, Orientation: "horizontal"},
	}

	baselines := make([]*image.NRGBA, opts.Frames)
	actuals := make([]*image.NRGBA, opts.Frames)
	metas := make([]core.FrameMeta, opts.Frames)
	mask := make([]int, opts.Frames)

	for i := 0; i < opts.Frames; i++ {
		frameRNG := rand.New(rand.NewSource(opts.Seed + int64(i)*1009))
		base := renderBaseFrame(i, width, height, region, primitives, params, frameRNG)
		baselines[i] = base
		actual := cloneNRGBA(base)

		if i >= replacementStart && i <= replacementEnd {
			mask[i] = 1
			applySplice(actual, baselines[sourceFrame], patch, frameRNG)
		}
		actuals[i] = actual

		framePath := filepath.ToSlash(filepath.Join("frames", frameName(i)))
		baselineName := filepath.ToSlash(filepath.Join("baseline", frameName(i)))
		if err := writePNG(filepath.Join(opts.OutputDir, framePath), actual); err != nil {
			return nil, err
		}
		if err := writePNG(filepath.Join(opts.OutputDir, baselineName), base); err != nil {
			return nil, err
		}
		metas[i] = core.FrameMeta{
			Index: i, File: framePath, BaselineFile: baselineName,
			TimestampMillis: int64(1700000000000 + i*40),
		}
	}

	manifest := core.Manifest{
		SchemaVersion:   core.SchemaVersion,
		Seed:            opts.Seed,
		Width:           width,
		Height:          height,
		Channels:        4,
		FrameCount:      opts.Frames,
		Frames:          metas,
		WatermarkRegion: region,
		Primitives:      primitives,
		Replacement: core.ReplacementTruth{
			StartFrame:  replacementStart,
			EndFrame:    replacementEnd,
			FrameMask:   mask,
			Patch:       patch,
			SourceFrame: sourceFrame,
			SourceRect:  &patch,
			Method:      "aligned_donor_copy_with_foreign_grain",
		},
		BaselineDir:    "baseline",
		BaselineHashes: map[int]string{},
		FrameHashes:    map[int]string{},
	}
	if err := writeJSON(filepath.Join(opts.OutputDir, "truth.json"), manifest); err != nil {
		return nil, err
	}

	seq, err := loader.Load(opts.OutputDir)
	if err != nil {
		return nil, err
	}
	for i, frame := range seq.Frames {
		manifest.FrameHashes[i] = frame.Hash
		baselineBytes, err := os.ReadFile(filepath.Join(opts.OutputDir, metas[i].BaselineFile))
		if err != nil {
			return nil, err
		}
		manifest.BaselineHashes[i] = core.SHA256Hex(baselineBytes)
	}
	if err := writeJSON(filepath.Join(opts.OutputDir, "truth.json"), manifest); err != nil {
		return nil, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(opts.OutputDir, "truth.json"))
	if err != nil {
		return nil, err
	}
	truthMaterial := struct {
		BaselineHashes map[int]string `json:"baseline_hashes"`
		Replacement    core.Interval  `json:"replacement"`
		Patch          core.Rect      `json:"patch"`
	}{
		BaselineHashes: manifest.BaselineHashes,
		Replacement:    core.Interval{StartFrame: replacementStart, EndFrame: replacementEnd},
		Patch:          patch,
	}
	fp := core.TruthFingerprint{
		SchemaVersion:    core.SchemaVersion,
		ManifestSHA256:   core.SHA256Hex(manifestBytes),
		TruthSHA256:      core.HashCanonical(truthMaterial),
		ReplacementStart: replacementStart,
		ReplacementEnd:   replacementEnd,
		FrameCount:       opts.Frames,
		AlgorithmVersion: core.AlgorithmVersion,
	}
	if err := writeJSON(filepath.Join(opts.OutputDir, "truth.fingerprint.json"), fp); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func frameName(i int) string {
	return "frame_" + pad4(i) + ".png"
}

func pad4(i int) string {
	s := ""
	if i < 10 {
		s = "000"
	} else if i < 100 {
		s = "00"
	} else if i < 1000 {
		s = "0"
	}
	return s + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

func renderBaseFrame(index, width, height int, region core.Rect, primitives []core.Primitive, params core.Params, rng *rand.Rand) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	top := [3]float64{24, 38, 76}
	bottom := [3]float64{116, 169, 211}
	for y := 0; y < height; y++ {
		t := float64(y) / float64(height-1)
		for x := 0; x < width; x++ {
			u := float64(x) / float64(width-1)
			r := top[0]*(1-t) + bottom[0]*t + 10*u
			g := top[1]*(1-t) + bottom[1]*t + 6*math.Sin(u*math.Pi)
			b := top[2]*(1-t) + bottom[2]*t
			img.SetNRGBA(x, y, color.NRGBA{R: clampByte(r), G: clampByte(g), B: clampByte(b), A: 255})
		}
	}

	fillRect(img, primitives[0].BBox, color.NRGBA{R: 236, G: 132, B: 58, A: 255})
	fillCircle(img, 132, 66, 24, color.NRGBA{R: 54, G: 196, B: 164, A: 255})
	fillTriangle(img, 70, 92, 103, 34, 136, 92, color.NRGBA{R: 154, G: 92, B: 224, A: 255})
	fillRect(img, primitives[3].BBox, color.NRGBA{R: 244, G: 205, B: 72, A: 255})

	watermark.Draw(img, index, region, params)

	globalBrightness := rng.Intn(9) - 4
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := img.PixOffset(x, y)
			grain := rng.Intn(5) - 2
			delta := globalBrightness + grain
			img.Pix[i+0] = clampByte(float64(img.Pix[i+0]) + float64(delta))
			img.Pix[i+1] = clampByte(float64(img.Pix[i+1]) + float64(delta))
			img.Pix[i+2] = clampByte(float64(img.Pix[i+2]) + float64(delta))
			img.Pix[i+3] = 255
		}
	}
	return img
}

func applySplice(dst, src *image.NRGBA, patch core.Rect, rng *rand.Rand) {
	for y := patch.Y; y < patch.MaxY(); y++ {
		for x := patch.X; x < patch.MaxX(); x++ {
			si := src.PixOffset(x, y)
			di := dst.PixOffset(x, y)
			if si < 0 || di < 0 || si+3 >= len(src.Pix) || di+3 >= len(dst.Pix) {
				continue
			}
			extraGrain := rng.Intn(45) - 22
			brightness := 12
			dst.Pix[di+0] = clampByte(float64(src.Pix[si+0]) + float64(brightness+extraGrain))
			dst.Pix[di+1] = clampByte(float64(src.Pix[si+1]) + float64(brightness+extraGrain))
			dst.Pix[di+2] = clampByte(float64(src.Pix[si+2]) + float64(brightness+extraGrain))
			dst.Pix[di+3] = 255
		}
	}
}

func fillRect(img *image.NRGBA, r core.Rect, c color.NRGBA) {
	for y := r.Y; y < r.MaxY(); y++ {
		for x := r.X; x < r.MaxX(); x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

func fillCircle(img *image.NRGBA, cx, cy, radius int, c color.NRGBA) {
	r2 := radius * radius
	for y := cy - radius; y <= cy+radius; y++ {
		for x := cx - radius; x <= cx+radius; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r2 {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

func fillTriangle(img *image.NRGBA, x1, y1, x2, y2, x3, y3 int, c color.NRGBA) {
	minX := minInt(x1, minInt(x2, x3))
	maxX := maxInt(x1, maxInt(x2, x3))
	minY := minInt(y1, minInt(y2, y3))
	maxY := maxInt(y1, maxInt(y2, y3))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			if pointInTriangle(float64(x)+0.5, float64(y)+0.5,
				[2]float64{float64(x1), float64(y1)},
				[2]float64{float64(x2), float64(y2)},
				[2]float64{float64(x3), float64(y3)}) {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

func pointInTriangle(px, py float64, a, b, c [2]float64) bool {
	d1 := sign(px, py, a, b)
	d2 := sign(px, py, b, c)
	d3 := sign(px, py, c, a)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func sign(px, py float64, a, b [2]float64) float64 {
	return (px-b[0])*(a[1]-b[1]) - (a[0]-b[0])*(py-b[1])
}

func cloneNRGBA(src *image.NRGBA) *image.NRGBA {
	dst := image.NewNRGBA(src.Rect)
	copy(dst.Pix, src.Pix)
	dst.Stride = src.Stride
	return dst
}

func clampByte(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(math.Round(v))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func writePNG(path string, img *image.NRGBA) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
