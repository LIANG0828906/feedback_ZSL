package sample

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"virtualstudio/compose"
	"virtualstudio/formats"
	"virtualstudio/model"
)

const (
	Width        = 96
	Height       = 64
	FrameCount   = 24
	WindowFrames = 4
	FrameRate    = 25
	Seed         = int64(42)
)

type Manifest struct {
	Seed            int64             `json:"seed"`
	Width           int               `json:"width"`
	Height          int               `json:"height"`
	FrameCount      int               `json:"frame_count"`
	WindowFrames    int               `json:"window_frames"`
	FrameDurationNS int64             `json:"frame_duration_ns"`
	Files           map[string]string `json:"files"`
	SceneHashes     map[string]string `json:"scene_hashes"`
}

type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	state := uint64(seed)
	if state == 0 {
		state = 0x9e3779b97f4a7c15
	}
	return &rng{state: state}
}

func (r *rng) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *rng) float() float64 {
	return float64(r.next()>>11) / float64(uint64(1)<<53)
}

func (r *rng) byte() byte {
	return byte(r.next() & 0xff)
}

func Generate(outDir string, seed int64) (*Manifest, error) {
	scene, err := CanonicalScene(seed)
	if err != nil {
		return nil, err
	}
	commands := BuildCommands()
	truth, err := BuildTruth(scene, commands)
	if err != nil {
		return nil, err
	}
	manifest := &Manifest{
		Seed:            seed,
		Width:           Width,
		Height:          Height,
		FrameCount:      FrameCount,
		WindowFrames:    WindowFrames,
		FrameDurationNS: scene.FrameDurationNS,
		Files:           map[string]string{},
		SceneHashes:     map[string]string{},
	}
	for _, target := range formats.SupportedVersions {
		result, err := formats.ExportScene(scene, target)
		if err != nil {
			return nil, err
		}
		rel := filepath.Join(target, "scene.json")
		path := filepath.Join(outDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, model.WrapError(model.CodeIO, path, "create sample directory", err)
		}
		if err := os.WriteFile(path, result.Descriptor, 0o644); err != nil {
			return nil, model.WrapError(model.CodeIO, path, "write scene", err)
		}
		manifest.Files[target] = filepath.ToSlash(rel)
		manifest.SceneHashes[target] = model.HashBytes(result.Descriptor)
	}
	commandsBytes, err := json.MarshalIndent(commands, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "commands", "marshal commands", err)
	}
	commandsBytes = append(commandsBytes, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "commands.json"), commandsBytes, 0o644); err != nil {
		return nil, model.WrapError(model.CodeIO, "commands.json", "write commands", err)
	}
	truthBytes, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "truth", "marshal truth", err)
	}
	truthBytes = append(truthBytes, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "truth.json"), truthBytes, 0o644); err != nil {
		return nil, model.WrapError(model.CodeIO, "truth.json", "write truth", err)
	}
	manifest.Files["commands"] = "commands.json"
	manifest.Files["truth"] = "truth.json"
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.CodeIO, "manifest", "marshal manifest", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), append(manifestBytes, '\n'), 0o644); err != nil {
		return nil, model.WrapError(model.CodeIO, "manifest.json", "write manifest", err)
	}
	return manifest, nil
}

func CanonicalScene(seed int64) (*model.Scene, error) {
	scene := &model.Scene{
		SourceVersion:   "v1",
		Width:           Width,
		Height:          Height,
		Channels:        4,
		FrameCount:      FrameCount,
		FrameDurationNS: 40_000_000,
		OrderExpr:       "z-index",
	}
	bg := makeLayer("bg", "Background Plate", backgroundPixels(seed), true, 1.0)
	prop := makeLayer("prop", "Hero Prop", propPixels(seed), true, 0.92)
	character := makeLayer("character", "Character", characterPixels(seed), true, 0.88)
	scene.Layers = []model.Layer{bg, prop, character}
	if err := model.ValidateScene(scene); err != nil {
		return nil, err
	}
	return scene, nil
}

func makeLayer(id, name string, straight []byte, visible bool, opacity float64) model.Layer {
	stabilized := stabilizePixels(straight)
	return model.Layer{
		ID:              id,
		Name:            name,
		Visible:         visible,
		Opacity:         opacity,
		SourceAlphaMode: model.AlphaStraight,
		Pixels:          stabilized,
	}
}

func backgroundPixels(seed int64) []byte {
	r := newRNG(seed ^ 0x11111111)
	out := make([]byte, Width*Height*4)
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			off := (y*Width + x) * 4
			grain := float64(r.byte()) * 0.10
			out[off] = byte(clampByte(float64(16+x*120/Width) + grain))
			out[off+1] = byte(clampByte(float64(28+y*105/Height) + grain))
			out[off+2] = byte(clampByte(float64(60+(x+y)*45/(Width+Height)) + grain*0.5))
			out[off+3] = 255
		}
	}
	// Deterministic slate markers for easy visual diagnostics.
	for y := 8; y < 13; y++ {
		for x := 8; x < 13; x++ {
			off := (y*Width + x) * 4
			out[off], out[off+1], out[off+2], out[off+3] = 230, 230, 230, 255
		}
	}
	return out
}

func propPixels(seed int64) []byte {
	r := newRNG(seed ^ 0x22222222)
	out := make([]byte, Width*Height*4)
	cx, cy := 31.0, 33.0
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			off := (y*Width + x) * 4
			dx := (float64(x) - cx) / 14.0
			dy := (float64(y) - cy) / 10.0
			d := math.Hypot(dx, dy)
			if d > 1.08 {
				continue
			}
			a := uint8(0)
			switch {
			case d < 0.82:
				a = 240
			case d < 0.98:
				a = 160
			default:
				a = 56
			}
			tone := uint8(clampByte(80 + 80*(1-d) + float64(r.byte()%20)))
			out[off] = tone
			out[off+1] = uint8(clampByte(float64(tone) * 0.55))
			out[off+2] = uint8(clampByte(float64(tone) * 0.25))
			out[off+3] = a
		}
	}
	return out
}

func characterPixels(seed int64) []byte {
	r := newRNG(seed ^ 0x33333333)
	out := make([]byte, Width*Height*4)
	cx, cy := 62.0, 31.0
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			off := (y*Width + x) * 4
			dx := (float64(x) - cx) / 11.0
			dy := (float64(y) - cy) / 18.0
			d := math.Hypot(dx, dy)
			// A slim silhouette with a head and lower body.
			body := d <= 1.0
			headDx := (float64(x) - cx) / 7.0
			headDy := (float64(y) - 14.0) / 7.0
			head := math.Hypot(headDx, headDy) <= 1.0
			if !body && !head {
				continue
			}
			a := uint8(255)
			if d > 0.9 || (head && math.Hypot(headDx, headDy) > 0.88) {
				a = 120
			}
			if x%7 == 0 {
				a = uint8((int(a)*13 + 6) / 14)
			}
			shade := uint8(clampByte(150 + float64(r.byte()%28)))
			out[off] = uint8(clampByte(float64(shade) * 0.72))
			out[off+1] = shade
			out[off+2] = uint8(clampByte(float64(shade) * 0.95))
			out[off+3] = a
		}
	}
	return out
}

func stabilizePixels(src []byte) []byte {
	out := append([]byte(nil), src...)
	for i := 0; i+3 < len(out); i += 4 {
		a := int(out[i+3])
		if a == 0 {
			out[i], out[i+1], out[i+2] = 0, 0, 0
			continue
		}
		for c := 0; c < 3; c++ {
			out[i+c] = stableStraight(out[i+c], a)
		}
	}
	return out
}

func stableStraight(c byte, a int) byte {
	p := (int(c)*a + 127) / 255
	back := (p*255 + a/2) / a
	if back == int(c) {
		return c
	}
	best := 0
	bestDist := 1 << 30
	for candidate := 0; candidate < 256; candidate++ {
		p := (candidate*a + 127) / 255
		back := (p*255 + a/2) / a
		if back != candidate {
			continue
		}
		dist := candidate - int(c)
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist {
			best, bestDist = candidate, dist
		}
	}
	return byte(best)
}

func clampByte(v float64) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return int(math.Round(v))
}

func BuildCommands() *model.CommandSet {
	commands := &model.CommandSet{Version: "commands/v1", WindowFrames: WindowFrames}
	for frame := 0; frame < FrameCount; frame++ {
		visible := map[string]bool{
			"bg":        true,
			"prop":      frame >= 6 && frame <= 19,
			"character": frame >= 4 && frame <= 15,
		}
		opacity := map[string]float64{}
		if visible["prop"] {
			opacity["prop"] = 0.94
		}
		if visible["character"] {
			opacity["character"] = 0.97
		}
		commands.Frames = append(commands.Frames, model.FrameCommand{Frame: frame, Visible: visible, Opacity: opacity})
	}
	return commands
}

func BuildTruth(scene *model.Scene, commands *model.CommandSet) (*model.Truth, error) {
	samples := compose.DefaultSamples(scene.Width, scene.Height)
	frames, err := compose.RenderAll(scene, commands, compose.RenderOptions{Samples: samples})
	if err != nil {
		return nil, err
	}
	truth := &model.Truth{
		Format:          "truth/v1",
		Version:         "truth/v1",
		FrameCount:      scene.FrameCount,
		WindowFrames:    commands.WindowFrames,
		SamplePoints:    samples,
		LayerVisibility: make(map[string]model.VisibilityEndpoint),
	}
	for _, layer := range scene.Layers {
		ep := model.VisibilityEndpoint{FirstVisible: -1, LastVisible: -1}
		for frame := 0; frame < scene.FrameCount; frame++ {
			eff := model.EffectiveLayerState(scene, commandAt(commands, frame), layer)
			if eff.Visible && eff.Opacity > 0 {
				if !ep.EverVisible {
					ep.FirstVisible = frame
					ep.EverVisible = true
				}
				ep.LastVisible = frame
			}
		}
		truth.LayerVisibility[layer.ID] = ep
	}
	for _, frame := range frames {
		tf := model.TruthFrame{
			Frame:       frame.Index,
			TimeNS:      frame.TimeNS,
			Visible:     map[string]bool{},
			Samples:     append([]model.PixelSample(nil), frame.Stats.Samples...),
			ContentHash: frame.Hash,
		}
		for _, id := range frame.Stats.VisibleLayers {
			tf.Visible[id] = true
		}
		truth.Frames = append(truth.Frames, tf)
	}
	return truth, nil
}

func commandAt(commands *model.CommandSet, frame int) *model.FrameCommand {
	if commands == nil || frame < 0 || frame >= len(commands.Frames) {
		return nil
	}
	return &commands.Frames[frame]
}

func LoadTruth(path string) (*model.Truth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.WrapError(model.CodeIO, path, "read truth", err)
	}
	var truth model.Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, model.WrapError(model.CodeDecode, path, "invalid truth JSON", err)
	}
	if truth.Version == "" {
		return nil, model.NewError(model.CodeVersion, "truth.version", "required field is missing")
	}
	return &truth, nil
}

func TruthMismatch(scene *model.Scene, commands *model.CommandSet, truth *model.Truth) (*model.FirstMismatch, error) {
	if truth == nil {
		return nil, model.NewError(model.CodeVersion, "truth", "truth is nil")
	}
	if truth.FrameCount != scene.FrameCount {
		return &model.FirstMismatch{Field: "truth.frame_count", Expected: fmt.Sprint(truth.FrameCount), Actual: fmt.Sprint(scene.FrameCount)}, nil
	}
	frames, err := compose.RenderAll(scene, commands, compose.RenderOptions{Samples: truth.SamplePoints})
	if err != nil {
		return nil, err
	}
	windowFrames := truth.WindowFrames
	if windowFrames <= 0 {
		windowFrames = commands.WindowFrames
	}
	for i, got := range frames {
		expected := truth.Frames[i]
		if got.TimeNS != expected.TimeNS {
			return &model.FirstMismatch{WindowIndex: i / windowFrames, Frame: i, X: -1, Y: -1, Field: "time_ns", Expected: fmt.Sprint(expected.TimeNS), Actual: fmt.Sprint(got.TimeNS)}, nil
		}
		if expected.ContentHash != "" && expected.ContentHash != got.Hash {
			return &model.FirstMismatch{WindowIndex: i / windowFrames, Frame: i, X: -1, Y: -1, Field: "content_hash", Expected: expected.ContentHash, Actual: got.Hash}, nil
		}
		if len(expected.Samples) != len(got.Stats.Samples) {
			return &model.FirstMismatch{WindowIndex: i / windowFrames, Frame: i, Field: "sample_count", Expected: fmt.Sprint(len(expected.Samples)), Actual: fmt.Sprint(len(got.Stats.Samples))}, nil
		}
		for s := range expected.Samples {
			if expected.Samples[s].RGBA != got.Stats.Samples[s].RGBA {
				return &model.FirstMismatch{
					WindowIndex: i / windowFrames,
					Frame:       i,
					X:           expected.Samples[s].X,
					Y:           expected.Samples[s].Y,
					Field:       "sample.rgba",
					Expected:    fmt.Sprint(expected.Samples[s].RGBA),
					Actual:      fmt.Sprint(got.Stats.Samples[s].RGBA),
				}, nil
			}
		}
	}
	for id, ep := range truth.LayerVisibility {
		layer, _, ok := scene.LayerByID(id)
		if !ok {
			return &model.FirstMismatch{Field: "visibility." + id, Expected: "layer present", Actual: "missing"}, nil
		}
		first, last := -1, -1
		for frame := 0; frame < scene.FrameCount; frame++ {
			eff := model.EffectiveLayerState(scene, commandAt(commands, frame), *layer)
			if eff.Visible && eff.Opacity > 0 {
				if first < 0 {
					first = frame
				}
				last = frame
			}
		}
		if first != ep.FirstVisible || last != ep.LastVisible {
			return &model.FirstMismatch{Frame: first, X: -1, Y: -1, Field: "visibility." + id, Expected: fmt.Sprintf("%d..%d", ep.FirstVisible, ep.LastVisible), Actual: fmt.Sprintf("%d..%d", first, last)}, nil
		}
	}
	return nil, nil
}

func DefaultDataDir(root string) string {
	return filepath.Join(root, "samples")
}
