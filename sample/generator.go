package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"virtualstage/pipeline"
)

const (
	DefaultFrameCount = 24
	DefaultWindowSize = 6
	DefaultWidth      = 320
	DefaultHeight     = 180
)

type Options struct {
	OutDir     string
	Seed       int64
	FrameCount int
	WindowSize int
}

type Result struct {
	OutDir     string            `json:"out_dir"`
	Seed       int64             `json:"seed"`
	Manifest   string            `json:"manifest"`
	Truth      string            `json:"truth"`
	Sources    []string          `json:"sources"`
	FrameCount int               `json:"frame_count"`
	WindowSize int               `json:"window_size"`
	FileHashes map[string]string `json:"file_hashes"`
}

type Manifest struct {
	Format       string   `json:"format"`
	Name         string   `json:"name"`
	Seed         int64    `json:"seed"`
	Sources      []string `json:"sources"`
	Instructions string   `json:"instructions"`
	Truth        string   `json:"truth"`
	WindowSize   int      `json:"window_size"`
}

type Truth struct {
	Format        string            `json:"format"`
	Seed          int64             `json:"seed"`
	FrameCount    int               `json:"frame_count"`
	WindowSize    int               `json:"window_size"`
	TimeBase      pipeline.TimeBase `json:"time_base"`
	VisibilityEnd map[string]int    `json:"visibility_end"`
	Frames        []TruthFrame      `json:"frames"`
}

type TruthFrame struct {
	Frame         int                    `json:"frame"`
	Window        int                    `json:"window"`
	Timestamp     pipeline.Rational      `json:"timestamp"`
	Samples       []pipeline.PixelSample `json:"samples"`
	VisibilityEnd map[string]int         `json:"visibility_end"`
	FrameHash     string                 `json:"frame_hash"`
}

type deterministic struct {
	state uint64
}

func newDeterministic(seed int64) *deterministic {
	return &deterministic{state: uint64(seed) + 0x9e3779b97f4a7c15}
}

func (d *deterministic) next() uint64 {
	d.state += 0x9e3779b97f4a7c15
	z := d.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (d *deterministic) unit() float64 {
	return float64(d.next()>>11) / float64(uint64(1)<<53)
}

func (d *deterministic) quantized(lo, hi float64) float64 {
	value := lo + (hi-lo)*d.unit()
	return math.Round(value*1000) / 1000
}

func Generate(opts Options) (*Result, error) {
	if opts.OutDir == "" {
		return nil, pipelineError("E_PARA", "output directory is required")
	}
	if opts.FrameCount <= 0 {
		opts.FrameCount = DefaultFrameCount
	}
	if opts.WindowSize <= 0 {
		opts.WindowSize = DefaultWindowSize
	}
	if opts.WindowSize > opts.FrameCount {
		return nil, pipelineError("E_PARA", "window size cannot exceed frame count")
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	layersDir := filepath.Join(opts.OutDir, "layers")
	if err := os.MkdirAll(layersDir, 0o755); err != nil {
		return nil, err
	}
	rng := newDeterministic(opts.Seed)
	bgColor := [3]float64{rng.quantized(0.08, 0.16), rng.quantized(0.16, 0.25), rng.quantized(0.28, 0.42)}
	propColor := [3]float64{rng.quantized(0.68, 0.82), rng.quantized(0.25, 0.36), rng.quantized(0.08, 0.16)}
	characterColor := [3]float64{rng.quantized(0.72, 0.88), rng.quantized(0.62, 0.78), rng.quantized(0.48, 0.62)}
	propX := 48 + int(rng.next()%40)
	propY := 32 + int(rng.next()%24)
	charX := 128 + int(rng.next()%30)
	charY := 38 + int(rng.next()%24)
	propAlpha := 0.84
	charAlpha := 0.93

	v1 := map[string]any{
		"format":      pipeline.V1Format,
		"frame_count": opts.FrameCount,
		"scene": map[string]any{
			"name": "Virtual Stage " + fmt.Sprintf("%d", opts.Seed), "width": DefaultWidth, "height": DefaultHeight,
			"channels": 4, "color_space": pipeline.ColorLinear,
			"frame_rate_num": 24, "frame_rate_den": 1, "premultiplied": true,
		},
		"layers": []any{
			map[string]any{
				"id": "background", "name": "Background Plate", "kind": "background",
				"z_index": 0, "enabled": true, "opacity": 1.0,
				"color": bgColor, "color_encoding": pipeline.ColorSRGB, "alpha": 1.0,
				"in_frame": 0, "out_frame": opts.FrameCount,
				"shape": map[string]any{"type": pipeline.ShapeFull},
			},
		},
		"layer_order": []string{"background"},
	}
	v2Color := [3]float64{propColor[0] * propAlpha, propColor[1] * propAlpha, propColor[2] * propAlpha}
	v2 := map[string]any{
		"format": pipeline.V2Format, "spec_version": "2.0", "frame_count": opts.FrameCount,
		"name":                "Virtual Stage " + fmt.Sprintf("%d", opts.Seed),
		"canvas":              map[string]any{"width": DefaultWidth, "height": DefaultHeight, "channels": 4},
		"working_color_space": pipeline.ColorLinear,
		"alpha":               map[string]any{"mode": pipeline.AlphaPremultiplied},
		"time":                map[string]any{"frame_rate": map[string]any{"numerator": 24, "denominator": 1}, "unit": "seconds"},
		"layers": []any{
			map[string]any{
				"uid": "prop", "display_name": "Foreground Prop", "layer_type": "prop",
				"visibility": map[string]any{"visible": true, "start_frame": 0, "until_frame": 18},
				"hierarchy":  map[string]any{"parent": nil, "order": 1},
				"pixel":      map[string]any{"color": v2Color, "opacity": 0.78, "alpha": propAlpha, "encoding": pipeline.ColorSRGB},
				"bounds":     map[string]any{"kind": pipeline.ShapeEllipse, "rect": []float64{float64(propX), float64(propY), 132, 104}},
			},
		},
		"stack": []string{"prop"},
	}
	v3 := map[string]any{
		"format": pipeline.V3Format, "schema_version": 3, "frame_count": opts.FrameCount,
		"name":   "Virtual Stage " + fmt.Sprintf("%d", opts.Seed),
		"canvas": map[string]any{"w": DefaultWidth, "h": DefaultHeight, "channels": 4},
		"color":  map[string]any{"space": pipeline.ColorLinear, "alpha_mode": pipeline.AlphaStraight, "per_layer_alpha": true},
		"timing": map[string]any{"tick_rate": 24000, "ticks_per_frame": 1000, "unit": "ticks"},
		"layers": []any{
			map[string]any{
				"layer_key": "character", "title": "Character Layer", "role": "character", "visible": true,
				"lifecycle": map[string]any{"start": 4, "end": opts.FrameCount},
				"parent":    nil,
				"source": map[string]any{
					"color": characterColor, "encoding": pipeline.ColorSRGB,
					"alpha": charAlpha, "premultiplied": pipeline.AlphaStraight,
				},
				"blend": map[string]any{"opacity": 0.9},
				"mask":  map[string]any{"type": pipeline.ShapeEllipse, "rect": []float64{float64(charX), float64(charY), 100, 108}},
			},
		},
		"hierarchy": map[string]any{
			"roots": []string{"character"}, "children": map[string][]string{}, "render_order": []string{"character"},
		},
	}
	sourceFiles := map[string]any{
		"layers-v1.json": v1,
		"layers-v2.json": v2,
		"layers-v3.json": v3,
	}
	paths := make([]string, 0, 3)
	hashes := map[string]string{}
	for _, name := range []string{"layers-v1.json", "layers-v2.json", "layers-v3.json"} {
		path := filepath.Join(layersDir, name)
		if err := writeJSON(path, sourceFiles[name]); err != nil {
			return nil, err
		}
		paths = append(paths, path)
		hash, _ := hashFile(path)
		hashes[path] = hash
	}

	instructions := map[string]any{
		"format": pipeline.InstructionFormat, "frame_count": opts.FrameCount,
		"frames": []any{
			map[string]any{"frame": 2, "layers": map[string]any{"prop": map[string]any{"opacity": 0.65}}},
			map[string]any{"frame": 9, "layers": map[string]any{"character": map[string]any{"opacity": 0.7}}},
			map[string]any{"frame": 14, "layers": map[string]any{"prop": map[string]any{"visible": false}}},
		},
	}
	insPath := filepath.Join(opts.OutDir, "instructions.json")
	if err := writeJSON(insPath, instructions); err != nil {
		return nil, err
	}
	hashes[insPath], _ = hashFile(insPath)

	relativeSources := []string{
		filepath.ToSlash(filepath.Join("layers", "layers-v1.json")),
		filepath.ToSlash(filepath.Join("layers", "layers-v2.json")),
		filepath.ToSlash(filepath.Join("layers", "layers-v3.json")),
	}
	manifest := Manifest{
		Format: pipeline.BundleFormat, Name: "Virtual Stage " + fmt.Sprintf("%d", opts.Seed), Seed: opts.Seed,
		Sources: relativeSources, Instructions: "instructions.json", Truth: "truth.json", WindowSize: opts.WindowSize,
	}
	manifestPath := filepath.Join(opts.OutDir, "scene.bundle.json")
	if err := writeJSON(manifestPath, manifest); err != nil {
		return nil, err
	}
	hashes[manifestPath], _ = hashFile(manifestPath)

	model, _, err := pipeline.LoadProject(manifestPath)
	if err != nil {
		return nil, err
	}
	truth, err := buildTruth(model, opts)
	if err != nil {
		return nil, err
	}
	truthPath := filepath.Join(opts.OutDir, "truth.json")
	if err := writeJSON(truthPath, truth); err != nil {
		return nil, err
	}
	hashes[truthPath], _ = hashFile(truthPath)

	return &Result{
		OutDir: opts.OutDir, Seed: opts.Seed, Manifest: manifestPath, Truth: truthPath,
		Sources: paths, FrameCount: opts.FrameCount, WindowSize: opts.WindowSize, FileHashes: hashes,
	}, nil
}

func buildTruth(model *pipeline.Model, opts Options) (*Truth, error) {
	visibility := map[string]int{}
	for _, layer := range model.Layers {
		visibility[layer.ID] = layer.VisibleUntilFrame
	}
	samplePoints := chooseSamplePoints(model)
	truth := &Truth{
		Format: "virtual-stage-truth/v1", Seed: opts.Seed, FrameCount: opts.FrameCount,
		WindowSize: opts.WindowSize, TimeBase: model.TimeBase, VisibilityEnd: visibility,
	}
	for frame := 0; frame < model.FrameCount; frame++ {
		composed, err := pipeline.CompositeFrame(model, frame, pipeline.CompositeConfig{MaxFrameBytes: 64 << 20})
		if err != nil {
			return nil, err
		}
		samples := make([]pipeline.PixelSample, 0, len(samplePoints))
		for _, point := range samplePoints {
			sample, err := composed.Sample(point[0], point[1])
			if err != nil {
				return nil, err
			}
			samples = append(samples, sample)
		}
		truth.Frames = append(truth.Frames, TruthFrame{
			Frame: frame, Window: frame / opts.WindowSize,
			Timestamp: composed.Timestamp, Samples: samples,
			VisibilityEnd: visibility, FrameHash: pipeline.FrameHash(composed),
		})
	}
	return truth, nil
}

func chooseSamplePoints(model *pipeline.Model) [][2]int {
	points := [][2]int{{8, 8}, {DefaultWidth - 12, DefaultHeight - 12}}
	for _, layer := range model.Layers {
		if layer.Shape.Type != pipeline.ShapeRect && layer.Shape.Type != pipeline.ShapeEllipse {
			continue
		}
		x := int(layer.Shape.Rect[0] + layer.Shape.Rect[2]/2)
		y := int(layer.Shape.Rect[1] + layer.Shape.Rect[3]/2)
		points = append(points, [2]int{x, y})
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i][1] == points[j][1] {
			return points[i][0] < points[j][0]
		}
		return points[i][1] < points[j][1]
	})
	out := points[:0]
	for _, point := range points {
		if len(out) == 0 || out[len(out)-1] != point {
			out = append(out, point)
		}
	}
	return out
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func hashFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type simpleError struct {
	code    string
	message string
}

func (e *simpleError) Error() string { return e.code + ": " + e.message }

func pipelineError(code, message string) error {
	return &simpleError{code: code, message: message}
}
