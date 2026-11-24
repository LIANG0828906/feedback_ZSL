package sample

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"screenverify/internal/frameio"
	"screenverify/internal/model"
)

const (
	FrameWidth    = 96
	FrameHeight   = 64
	FrameChannels = 3
	FrameCount    = 16
	GridX         = 8
	GridY         = 4
)

type Options struct {
	OutputDir string
	Seed      int64
	Frames    int
}

func DefaultOptions(out string) Options {
	return Options{OutputDir: out, Seed: 0x51A7E, Frames: FrameCount}
}

func GenerateDataset(opts Options) (*model.DatasetManifest, error) {
	if opts.Frames <= 0 {
		opts.Frames = FrameCount
	}
	if opts.Seed == 0 {
		opts.Seed = 0x51A7E
	}
	clips := []struct {
		name     string
		scenario string
	}{
		{name: "noise", scenario: "noise_tamper"},
		{name: "watermark", scenario: "watermark_break"},
		{name: "clean", scenario: "clean"},
	}
	manifest := &model.DatasetManifest{Schema: "recverify-dataset-v1", Seed: opts.Seed}
	for _, clip := range clips {
		dir := filepath.Join(opts.OutputDir, clip.name)
		truth, err := GenerateClip(dir, clip.name, clip.scenario, opts.Seed+int64(len(manifest.Clips)*1009), opts.Frames)
		if err != nil {
			return nil, err
		}
		manifest.Clips = append(manifest.Clips, model.ClipInfo{
			Name: clip.name, Dir: filepath.ToSlash(clip.name), Truth: filepath.ToSlash(filepath.Join(clip.name, "truth.json")),
		})
		_ = truth
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(opts.OutputDir, "dataset.json"), b, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func GenerateClip(dir, clipName, scenario string, seed int64, frameCount int) (*model.Truth, error) {
	frameDir := filepath.Join(dir, "frames")
	baseDir := filepath.Join(dir, "baseline")
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}

	wmRect := model.Rect{X: 62, Y: 4, Width: 32, Height: 16}
	prims := defaultPrimitives()
	truth := &model.Truth{
		Schema:           model.SchemaVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		Seed:             seed,
		Clip:             clipName,
		Width:            FrameWidth,
		Height:           FrameHeight,
		Channels:         FrameChannels,
		FrameCount:       frameCount,
		FrameTolerance:   1,
		DefaultBlockSize: 16,
		Replacement:      model.Replacement{Scenario: scenario, StartFrame: -1, EndFrame: -1},
	}

	switch scenario {
	case "noise_tamper":
		truth.Replacement.StartFrame = 8
		truth.Replacement.EndFrame = 10
		truth.Replacement.Region = model.Rect{X: 8, Y: 18, Width: 36, Height: 28}
	case "watermark_break":
		truth.Replacement.StartFrame = 7
		truth.Replacement.EndFrame = 9
		truth.Replacement.Region = wmRect
	}
	truth.Replacement.Mask = make([]bool, frameCount)
	for f := truth.Replacement.StartFrame; f >= 1 && f <= truth.Replacement.EndFrame; f++ {
		truth.Replacement.Mask[f-1] = true
	}

	rng := rand.New(rand.NewSource(seed))
	for frameNo := 1; frameNo <= frameCount; frameNo++ {
		baseline := renderPristineFrame(frameNo, seed, prims, wmRect)
		actual := append([]byte(nil), baseline...)

		ft := model.FrameTruth{
			Frame:           frameNo,
			TimestampNS:     int64(frameNo-1) * 33_333_333,
			File:            fmt.Sprintf("frames/frame_%03d.rmf", frameNo),
			BaselineFile:    fmt.Sprintf("baseline/frame_%03d.rmf", frameNo),
			WatermarkRegion: wmRect,
			Primitives:      prims,
		}

		if truth.Replacement.Mask[frameNo-1] {
			switch scenario {
			case "noise_tamper":
				applyNoiseReplacement(actual, FrameWidth, FrameHeight, FrameChannels, truth.Replacement.Region, frameNo, rng)
			case "watermark_break":
				applyWatermarkAttack(actual, FrameWidth, FrameHeight, FrameChannels, wmRect, frameNo)
			}
		}

		ft.ContentSHA256 = model.HashBytes("actual-frame-v1:", actual)
		ft.BaselineSHA256 = model.HashBytes("baseline-frame-v1:", baseline)
		framePath := filepath.Join(dir, ft.File)
		basePath := filepath.Join(dir, ft.BaselineFile)
		if err := frameio.WriteFile(framePath, FrameWidth, FrameHeight, FrameChannels, actual); err != nil {
			return nil, err
		}
		if err := frameio.WriteFile(basePath, FrameWidth, FrameHeight, FrameChannels, baseline); err != nil {
			return nil, err
		}
		truth.Frames = append(truth.Frames, ft)
	}

	fp, err := model.FingerprintTruth(truth)
	if err != nil {
		return nil, err
	}
	truth.TruthFingerprint = fp
	b, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), b, 0o644); err != nil {
		return nil, err
	}
	return truth, nil
}

func defaultPrimitives() []model.Primitive {
	return []model.Primitive{
		{Kind: "rect", Label: "health_panel", Color: [3]int{220, 72, 72}, BBox: model.Rect{X: 8, Y: 20, Width: 26, Height: 20}},
		{Kind: "circle", Label: "mini_map", Color: [3]int{72, 190, 120}, BBox: model.Rect{X: 42, Y: 22, Width: 20, Height: 20}},
		{Kind: "triangle", Label: "direction_marker", Color: [3]int{82, 132, 230}, BBox: model.Rect{X: 68, Y: 18, Width: 20, Height: 24}, Points: []model.Point{{X: 78, Y: 18}, {X: 68, Y: 42}, {X: 88, Y: 42}}},
	}
}

func renderPristineFrame(frameNo int, seed int64, prims []model.Primitive, wm model.Rect) []byte {
	w, h, ch := FrameWidth, FrameHeight, FrameChannels
	pix := make([]byte, w*h*ch)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ripple := 5.0 * math.Sin(float64(x+frameNo)*0.22) * math.Cos(float64(y)*0.19)
			n := deterministicNoise(x, y, frameNo, seed)
			r := clampFloatInt(38 + float64(x/3) + float64(y)/7 + ripple + float64(n))
			g := clampFloatInt(42 + float64(y*2/3) + float64(x)/9 + ripple*0.4 + float64(n))
			b := clampFloatInt(78 + float64(x+y)/9 + ripple*0.7 + float64(n))
			setRGB(pix, w, h, ch, x, y, r, g, b)
		}
	}
	for _, p := range prims {
		drawPrimitive(pix, w, h, ch, p)
	}
	drawWatermark(pix, w, h, ch, wm, frameNo, GridX, GridY, nil, 0, 0, false)
	return pix
}

func drawPrimitive(pix []byte, w, h, ch int, p model.Primitive) {
	switch p.Kind {
	case "rect":
		fillRect(pix, w, h, ch, p.BBox, p.Color)
	case "circle":
		cx := float64(p.BBox.X) + float64(p.BBox.Width)/2
		cy := float64(p.BBox.Y) + float64(p.BBox.Height)/2
		rx := float64(p.BBox.Width) / 2
		ry := float64(p.BBox.Height) / 2
		for y := p.BBox.Y; y < p.BBox.Y+p.BBox.Height; y++ {
			for x := p.BBox.X; x < p.BBox.X+p.BBox.Width; x++ {
				v := ((float64(x)+0.5-cx)/rx)*((float64(x)+0.5-cx)/rx) + ((float64(y)+0.5-cy)/ry)*((float64(y)+0.5-cy)/ry)
				if v <= 1 {
					setRGB(pix, w, h, ch, x, y, p.Color[0], p.Color[1], p.Color[2])
				}
			}
		}
	case "triangle":
		for y := p.BBox.Y; y < p.BBox.Y+p.BBox.Height; y++ {
			for x := p.BBox.X; x < p.BBox.X+p.BBox.Width; x++ {
				if inTriangle(float64(x)+0.5, float64(y)+0.5, p.Points[0], p.Points[1], p.Points[2]) {
					setRGB(pix, w, h, ch, x, y, p.Color[0], p.Color[1], p.Color[2])
				}
			}
		}
	}
}

func inTriangle(px, py float64, a, b, c model.Point) bool {
	d1 := (px-b.X)*(a.Y-b.Y) - (a.X-b.X)*(py-b.Y)
	d2 := (px-c.X)*(b.Y-c.Y) - (b.X-c.X)*(py-c.Y)
	d3 := (px-a.X)*(c.Y-a.Y) - (c.X-a.X)*(py-a.Y)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func fillRect(pix []byte, w, h, ch int, r model.Rect, color [3]int) {
	for y := r.Y; y < r.Y+r.Height; y++ {
		for x := r.X; x < r.X+r.Width; x++ {
			setRGB(pix, w, h, ch, x, y, color[0], color[1], color[2])
		}
	}
}

func drawWatermark(pix []byte, w, h, ch int, r model.Rect, frameNo, gridX, gridY int, forced *int, shiftX, shiftY int, blank bool) {
	if blank {
		fillRect(pix, w, h, ch, r, [3]int{128, 128, 128})
		return
	}
	value := uint32(frameNo)
	if forced != nil {
		value = uint32(*forced)
	}
	originX := r.X + shiftX
	originY := r.Y + shiftY
	cellW := float64(r.Width) / float64(gridX)
	cellH := float64(r.Height) / float64(gridY)
	for row := 0; row < gridY; row++ {
		for col := 0; col < gridX; col++ {
			idx := row*gridX + col
			bit := 0
			if idx < 32 {
				bit = int((value >> uint(31-idx)) & 1)
			}
			color := [3]int{34, 38, 46}
			if bit == 1 {
				color = [3]int{238, 242, 248}
			}
			x0 := originX + int(math.Round(float64(col)*cellW))
			y0 := originY + int(math.Round(float64(row)*cellH))
			x1 := originX + int(math.Round(float64(col+1)*cellW))
			y1 := originY + int(math.Round(float64(row+1)*cellH))
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					setRGB(pix, w, h, ch, xx, yy, color[0], color[1], color[2])
				}
			}
		}
	}
}

func applyNoiseReplacement(pix []byte, w, h, ch int, r model.Rect, frameNo int, rng *rand.Rand) {
	for y := r.Y; y < r.Y+r.Height; y++ {
		for x := r.X; x < r.X+r.Width; x++ {
			if x < 0 || y < 0 || x >= w || y >= h {
				continue
			}
			n1 := rng.Intn(101) - 50
			n2 := rng.Intn(91) - 45
			pattern := 35 + int(45*math.Sin(float64(x)*0.65)) + int(30*math.Cos(float64(y+frameNo)*0.55))
			rr := clampFloatInt(float64(175 + pattern/3 + n1))
			gg := clampFloatInt(float64(28 + pattern/5 + n2))
			bb := clampFloatInt(float64(70 + pattern/2 + n1/2))
			setRGB(pix, w, h, ch, x, y, rr, gg, bb)
		}
	}
}

func applyWatermarkAttack(pix []byte, w, h, ch int, r model.Rect, frameNo int) {
	switch frameNo {
	case 7:
		drawWatermark(pix, w, h, ch, r, frameNo, GridX, GridY, nil, 0, 0, true)
	case 8:
		wrong := frameNo + 5
		drawWatermark(pix, w, h, ch, r, frameNo, GridX, GridY, &wrong, 0, 0, false)
	case 9:
		drawWatermark(pix, w, h, ch, r, frameNo, GridX, GridY, nil, 2, 1, false)
	}
}

func setRGB(pix []byte, w, h, ch, x, y, r, g, b int) {
	if x < 0 || y < 0 || x >= w || y >= h {
		return
	}
	i := (y*w + x) * ch
	pix[i] = byte(clampByte(r))
	pix[i+1] = byte(clampByte(g))
	pix[i+2] = byte(clampByte(b))
	if ch == 4 {
		pix[i+3] = 255
	}
}

func clampByte(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func clampFloatInt(v float64) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return int(v)
}

func deterministicNoise(x, y, frame int, seed int64) int {
	v := uint64(seed) ^ uint64(x)*1_000_003 ^ uint64(y)*91_763 ^ uint64(frame)*127
	v ^= v >> 23
	v *= 0x2127599bf4325c37
	v ^= v >> 47
	return int(v%5) - 2
}
