package sample

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gameintegrity/internal/frameio"
	"gameintegrity/internal/model"
)

type Spec struct {
	ID             string
	Description    string
	Width          int
	Height         int
	Channels       int
	FrameCount     int
	BlockSize      int
	Replacement    model.Interval
	SegmentSeconds float64
	Tolerance      int
	Injection      string
	InjectionFrame int
}

type Info struct {
	ID             string         `json:"id"`
	Description    string         `json:"description"`
	Width          int            `json:"width"`
	Height         int            `json:"height"`
	Channels       int            `json:"channels"`
	FrameCount     int            `json:"frame_count"`
	BlockSize      int            `json:"block_size"`
	Replacement    model.Interval `json:"replacement"`
	SegmentSeconds float64        `json:"segment_seconds"`
	Tolerance      int            `json:"tolerance_frames"`
	Anomaly        string         `json:"anomaly,omitempty"`
}

type Catalog struct {
	Seed    int64  `json:"seed"`
	Samples []Info `json:"samples"`
}

type CatalogView struct {
	Seed    int64  `json:"seed"`
	Samples []Info `json:"samples"`
}

func Specs() []Spec {
	return []Spec{
		{ID: "tiny", Description: "small deterministic sanity sample", Width: 80, Height: 64, Channels: 3, FrameCount: 16, BlockSize: 8, Replacement: model.Interval{Start: 6, End: 8}, SegmentSeconds: 1, Tolerance: 1},
		{ID: "small", Description: "short low-resolution recording", Width: 96, Height: 192, Channels: 3, FrameCount: 36, BlockSize: 8, Replacement: model.Interval{Start: 18, End: 24}, SegmentSeconds: 1, Tolerance: 1},
		{ID: "medium", Description: "medium recording with nine one-second segments", Width: 240, Height: 192, Channels: 3, FrameCount: 90, BlockSize: 16, Replacement: model.Interval{Start: 36, End: 46}, SegmentSeconds: 1, Tolerance: 1},
		{ID: "large", Description: "large recording used for adaptive sampling", Width: 480, Height: 360, Channels: 3, FrameCount: 160, BlockSize: 24, Replacement: model.Interval{Start: 71, End: 86}, SegmentSeconds: 1, Tolerance: 1},
		{ID: "small-e-order", Description: "injected non-monotonic timestamp", Width: 96, Height: 192, Channels: 3, FrameCount: 36, BlockSize: 8, Replacement: model.Interval{Start: 18, End: 24}, SegmentSeconds: 1, Tolerance: 1, Injection: "order", InjectionFrame: 8},
		{ID: "small-e-mark", Description: "injected missing frame watermark", Width: 96, Height: 192, Channels: 3, FrameCount: 36, BlockSize: 8, Replacement: model.Interval{Start: 18, End: 24}, SegmentSeconds: 1, Tolerance: 1, Injection: "mark", InjectionFrame: 6},
		{ID: "small-e-frame", Description: "injected corrupt frame", Width: 96, Height: 192, Channels: 3, FrameCount: 36, BlockSize: 8, Replacement: model.Interval{Start: 18, End: 24}, SegmentSeconds: 1, Tolerance: 1, Injection: "frame", InjectionFrame: 10},
		{ID: "small-e-size", Description: "injected frame header size mutation", Width: 96, Height: 192, Channels: 3, FrameCount: 36, BlockSize: 8, Replacement: model.Interval{Start: 18, End: 24}, SegmentSeconds: 1, Tolerance: 1, Injection: "size", InjectionFrame: 0},
	}
}

func Generate(out string, seed int64) (Catalog, error) {
	if err := os.RemoveAll(out); err != nil {
		return Catalog{}, fmt.Errorf("clear output directory: %w", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return Catalog{}, fmt.Errorf("create output directory: %w", err)
	}
	cat := Catalog{Seed: seed}
	for _, spec := range Specs() {
		if err := WriteSample(out, spec, seed); err != nil {
			return Catalog{}, fmt.Errorf("generate %s: %w", spec.ID, err)
		}
		cat.Samples = append(cat.Samples, Info{
			ID: spec.ID, Description: spec.Description,
			Width: spec.Width, Height: spec.Height, Channels: spec.Channels,
			FrameCount: spec.FrameCount, BlockSize: spec.BlockSize,
			Replacement: spec.Replacement, SegmentSeconds: spec.SegmentSeconds,
			Tolerance: spec.Tolerance, Anomaly: spec.Injection,
		})
	}
	sort.Slice(cat.Samples, func(i, j int) bool { return cat.Samples[i].ID < cat.Samples[j].ID })
	if err := writeJSON(filepath.Join(out, "catalog.json"), cat); err != nil {
		return Catalog{}, err
	}
	return cat, nil
}

func WriteSample(root string, spec Spec, seed int64) error {
	dir := filepath.Join(root, spec.ID)
	if err := os.MkdirAll(filepath.Join(dir, "frames"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "baseline"), 0o755); err != nil {
		return err
	}
	manifest := model.Manifest{
		FormatVersion:  model.FormatVersion,
		SampleID:       spec.ID,
		Seed:           seed,
		Width:          spec.Width,
		Height:         spec.Height,
		Channels:       spec.Channels,
		FrameCount:     spec.FrameCount,
		FrameDuration:  100000,
		TimestampsUS:   make([]int64, spec.FrameCount),
		SegmentSeconds: spec.SegmentSeconds,
		BlockSize:      spec.BlockSize,
		SamplingBudget: 24,
		Tolerance:      spec.Tolerance,
		FramePattern:   "frames/frame-%06d.rgbx",
		BaselineDir:    "baseline",
	}
	truth := model.Truth{
		Width: spec.Width, Height: spec.Height, Channels: spec.Channels,
		FrameCount:    spec.FrameCount,
		Replacements:  []model.Interval{spec.Replacement},
		Tolerance:     spec.Tolerance,
		BaselineBytes: int64(spec.Width * spec.Height * spec.Channels),
		Baseline:      make([]string, spec.FrameCount),
	}
	injectedBlock := model.BlockAtPixel(spec.Width/4, spec.Height/16, spec.Width, spec.Height, spec.BlockSize)
	if spec.Injection != "" {
		manifest.Injections = []model.Injection{{
			Type: spec.Injection, Frame: spec.InjectionFrame, Block: injectedBlock,
		}}
	}
	for i := 0; i < spec.FrameCount; i++ {
		manifest.TimestampsUS[i] = int64(i) * manifest.FrameDuration
		baseline, boxes := renderBaseline(spec.Width, spec.Height, i, seed, spec.BlockSize)
		actual := append([]byte(nil), baseline...)
		if i >= spec.Replacement.Start && i <= spec.Replacement.End {
			actual = renderReplacement(spec.Width, spec.Height, i, seed)
		}
		clean := append([]byte(nil), baseline...)
		drawWatermark(clean, spec.Width, spec.Height, spec.Channels, i, seed)
		drawWatermark(actual, spec.Width, spec.Height, spec.Channels, i, seed)
		baselinePath := filepath.Join(dir, "baseline", fmt.Sprintf("frame-%06d.rgb", i))
		if err := os.WriteFile(baselinePath, clean, 0o644); err != nil {
			return err
		}
		truth.Baseline[i] = filepath.ToSlash(filepath.Join("baseline", fmt.Sprintf("frame-%06d.rgb", i)))
		frameBytes, err := frameio.EncodeFrame(spec.Width, spec.Height, spec.Channels, i, 0, actual)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "frames", fmt.Sprintf("frame-%06d.rgbx", i)), frameBytes, 0o644); err != nil {
			return err
		}
		truth.Shapes = append(truth.Shapes, model.FrameShapes{Frame: i, Boxes: boxes})
		wm := watermarkFor(spec.Width, spec.Height, i, seed)
		truth.Watermarks = append(truth.Watermarks, wm)
	}
	if spec.Injection == "order" {
		manifest.TimestampsUS[spec.InjectionFrame] = manifest.TimestampsUS[spec.InjectionFrame-1]
	}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "truth.json"), truth); err != nil {
		return err
	}
	switch spec.Injection {
	case "mark":
		return eraseWatermark(dir, spec, seed, spec.InjectionFrame)
	case "frame":
		return setBadFrame(dir, spec, spec.InjectionFrame)
	case "size":
		return mutateFrameWidth(dir, spec, spec.InjectionFrame)
	default:
		return nil
	}
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func watermarkFor(width, height, frame int, seed int64) model.Watermark {
	x := 2
	y := 2
	if width > 80 {
		x = 2 + int((uint64(seed)+uint64(frame))%3)
	}
	return model.Watermark{
		Frame: frame, X: x, Y: y,
		Width: frameio.WatermarkWidth, Height: frameio.WatermarkHeight,
		Value: frame,
	}
}

func renderBaseline(width, height, frame int, seed int64, block int) ([]byte, []model.ShapeBox) {
	pixels := make([]byte, width*height*3)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			off := (y*width + x) * 3
			pixels[off] = byte(18 + (x*91/width)%128)
			pixels[off+1] = byte(24 + (y*83/height)%122)
			pixels[off+2] = byte(36 + ((x+y)*57/(width+height))%118)
		}
	}
	drawPerturbations(pixels, width, height, frame, seed)
	boxes := drawShapes(pixels, width, height, frame, seed)
	return pixels, boxes
}

func renderReplacement(width, height, frame int, seed int64) []byte {
	pixels := make([]byte, width*height*3)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			off := (y*width + x) * 3
			n := mix(uint64(seed) ^ uint64(frame+1)*0x9e3779b97f4a7c15 ^ uint64(y)*0xbf58476d1ce4e5b9 ^ uint64(x)*0x94d049bb133111eb)
			amplitude := byte(45 + (int(n>>56) % 150))
			pixels[off] = byte((int(n>>8) + int(amplitude)) & 0xff)
			pixels[off+1] = byte((int(n>>24) + int(amplitude)) & 0xff)
			pixels[off+2] = byte((int(n>>40) + int(amplitude)) & 0xff)
		}
	}
	return pixels
}

func drawPerturbations(pixels []byte, width, height, frame int, seed int64) {
	for p := 0; p < 3; p++ {
		n := mix(uint64(seed) ^ uint64(frame)*0x9e3779b1 ^ uint64(p+1)*0x85ebca6b)
		pw, ph := 5+int(n%7), 4+int((n>>8)%6)
		x := 4 + int((n>>16)%uint64(max(1, width-pw-6)))
		y := height/3 + int((n>>32)%uint64(max(1, height-ph-height/3-4)))
		delta := int((n>>48)%31) - 15
		for yy := y; yy < y+ph && yy < height; yy++ {
			for xx := x; xx < x+pw && xx < width; xx++ {
				off := (yy*width + xx) * 3
				for c := 0; c < 3; c++ {
					v := int(pixels[off+c]) + delta
					if v < 0 {
						v = 0
					}
					if v > 255 {
						v = 255
					}
					pixels[off+c] = byte(v)
				}
			}
		}
	}
}

func drawShapes(pixels []byte, width, height, frame int, seed int64) []model.ShapeBox {
	colors := [][3]int{{245, 35, 95}, {35, 225, 85}, {45, 90, 235}}
	boxW, boxH := 18, 12
	if width < 96 {
		boxW, boxH = 10, 8
	}
	rx := 5 + (frame*2+int(seed&7))%max(1, width/4)
	ry := height/4 + (frame+int(seed&3))%max(1, height/8)
	fillRect(pixels, width, height, rx, ry, boxW, boxH, colors[0])
	boxes := []model.ShapeBox{{ID: "rect", Kind: "rectangle", X: rx, Y: ry, W: boxW, H: boxH, Color: colors[0]}}

	radius := 8
	if width < 96 {
		radius = 5
	}
	cx := 2*width/3 + (frame % 5) - 2
	cy := height/2 + (frame*2)%7 - 3
	fillCircle(pixels, width, height, cx, cy, radius, colors[1])
	boxes = append(boxes, model.ShapeBox{ID: "circle", Kind: "circle", X: cx - radius, Y: cy - radius, W: radius*2 + 1, H: radius*2 + 1, Color: colors[1]})

	tw, th := 16, 12
	if width < 96 {
		tw, th = 8, 7
	}
	tx := width - tw - 5 - (frame % 4)
	ty := height - th - 8 - (frame % 3)
	fillTriangle(pixels, width, height, tx, ty, tw, th, colors[2])
	boxes = append(boxes, model.ShapeBox{ID: "triangle", Kind: "triangle", X: tx, Y: ty, W: tw, H: th, Color: colors[2]})
	return boxes
}

func fillRect(pixels []byte, width, height, x, y, w, h int, c [3]int) {
	for yy := max(0, y); yy < min(height, y+h); yy++ {
		for xx := max(0, x); xx < min(width, x+w); xx++ {
			off := (yy*width + xx) * 3
			pixels[off], pixels[off+1], pixels[off+2] = byte(c[0]), byte(c[1]), byte(c[2])
		}
	}
}

func fillCircle(pixels []byte, width, height, cx, cy, radius int, c [3]int) {
	r2 := radius * radius
	for y := cy - radius; y <= cy+radius; y++ {
		for x := cx - radius; x <= cx+radius; x++ {
			dx, dy := x-cx, y-cy
			if x < 0 || y < 0 || x >= width || y >= height || dx*dx+dy*dy > r2 {
				continue
			}
			off := (y*width + x) * 3
			pixels[off], pixels[off+1], pixels[off+2] = byte(c[0]), byte(c[1]), byte(c[2])
		}
	}
}

func fillTriangle(pixels []byte, width, height, x, y, w, h int, c [3]int) {
	for yy := 0; yy < h; yy++ {
		span := (yy * (w - 1) / max(1, h-1))
		x0 := x + (w-span)/2
		for xx := x0; xx < x0+span+1; xx++ {
			if xx < 0 || xx >= width || y+yy < 0 || y+yy >= height {
				continue
			}
			off := ((y+yy)*width + xx) * 3
			pixels[off], pixels[off+1], pixels[off+2] = byte(c[0]), byte(c[1]), byte(c[2])
		}
	}
}

func drawWatermark(pixels []byte, width, height, channels, frame int, seed int64) {
	wm := watermarkFor(width, height, frame, seed)
	border := [3]byte{255, 0, 255}
	color0 := [3]byte{24, 24, 24}
	color1 := [3]byte{231, 231, 231}
	for y := wm.Y; y < wm.Y+wm.Height; y++ {
		for x := wm.X; x < wm.X+wm.Width; x++ {
			setPixel(pixels, width, height, channels, x, y, border)
		}
	}
	for bit := 0; bit < frameio.WatermarkBits; bit++ {
		c := color0
		if frame&(1<<bit) != 0 {
			c = color1
		}
		bx := wm.X + frameio.WatermarkBorder + bit*frameio.WatermarkCell
		by := wm.Y + frameio.WatermarkBorder
		for y := by; y < by+frameio.WatermarkCell; y++ {
			for x := bx; x < bx+frameio.WatermarkCell; x++ {
				setPixel(pixels, width, height, channels, x, y, c)
			}
		}
	}
}

func setPixel(pixels []byte, width, height, channels, x, y int, c [3]byte) {
	if x < 0 || y < 0 || x >= width || y >= height || channels < 3 {
		return
	}
	off := (y*width + x) * channels
	pixels[off], pixels[off+1], pixels[off+2] = c[0], c[1], c[2]
}

func eraseWatermark(dir string, spec Spec, seed int64, frame int) error {
	path := filepath.Join(dir, "frames", fmt.Sprintf("frame-%06d.rgbx", frame))
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	wm := watermarkFor(spec.Width, spec.Height, frame, seed)
	for y := wm.Y; y < wm.Y+wm.Height; y++ {
		for x := wm.X; x < wm.X+wm.Width; x++ {
			off := frameio.HeaderSize + (y*spec.Width+x)*spec.Channels
			if off+2 < len(data) {
				data[off], data[off+1], data[off+2] = 40, 46, 58
			}
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func setBadFrame(dir string, spec Spec, frame int) error {
	path := filepath.Join(dir, "frames", fmt.Sprintf("frame-%06d.rgbx", frame))
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	data[11] |= frameio.FlagBadFrame
	return os.WriteFile(path, data, 0o644)
}

func mutateFrameWidth(dir string, spec Spec, frame int) error {
	path := filepath.Join(dir, "frames", fmt.Sprintf("frame-%06d.rgbx", frame))
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	data[6] = 0
	data[7] = byte(spec.Width + 1)
	return os.WriteFile(path, data, 0o644)
}

func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
