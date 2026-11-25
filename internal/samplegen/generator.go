package samplegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"example.com/vectorcore"
	"example.com/vectorcore/internal/sampletruth"
)

const GeneratedBy = "vectorcore deterministic sample generator v1"

type Options struct {
	Seed    int64
	Batches int
	OutDir  string
	Config  vectorcore.Config
}

type Manifest struct {
	Seed            int64                   `json:"seed"`
	GeneratedBy     string                  `json:"generatedBy"`
	Config          sampletruth.ConfigTruth `json:"config"`
	TruthDisclaimer string                  `json:"truthDisclaimer"`
	Batches         []BatchInfo             `json:"batches"`
}

type BatchInfo struct {
	ID        string            `json:"id"`
	Directory string            `json:"directory"`
	Scenes    map[string]string `json:"scenes"`
	Truth     map[string]string `json:"truth"`
}

func Generate(opts Options) (*Manifest, error) {
	if opts.Batches <= 0 {
		opts.Batches = 3
	}
	if opts.OutDir == "" {
		opts.OutDir = "samples"
	}
	opts.Config = normalizeConfig(opts.Config)
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	manifest := &Manifest{
		Seed: opts.Seed, GeneratedBy: GeneratedBy,
		Config:          sampletruth.ConfigFrom(opts.Config),
		TruthDisclaimer: "Truth records are generated from the analytic curve and deterministic raster construction; rendering observations never feed back into truth.",
	}
	for bi := 0; bi < opts.Batches; bi++ {
		batchID := fmt.Sprintf("batch-%02d", bi+1)
		scene := buildScene(batchID, rng, opts.Seed)
		dir := filepath.Join(opts.OutDir, batchID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		info := BatchInfo{ID: batchID, Directory: batchID, Scenes: map[string]string{}, Truth: map[string]string{}}
		reference := map[int][]*vectorcore.TimeResult{}
		var baseline []*vectorcore.TimeResult
		for version := 1; version <= 3; version++ {
			encoded, err := vectorcore.EncodeScene(scene, version)
			if err != nil {
				return nil, fmt.Errorf("%s encode v%d: %w", batchID, version, err)
			}
			sceneName := fmt.Sprintf("scene.v%d.json", version)
			if err := os.WriteFile(filepath.Join(dir, sceneName), append(encoded, '\n'), 0o644); err != nil {
				return nil, err
			}
			info.Scenes[fmt.Sprintf("v%d", version)] = filepath.ToSlash(filepath.Join(batchID, sceneName))
			loaded, err := vectorcore.LoadSceneBytes(encoded)
			if err != nil {
				return nil, fmt.Errorf("%s load v%d: %w", batchID, version, err)
			}
			times, err := renderReference(loaded, opts.Config)
			if err != nil {
				return nil, fmt.Errorf("%s render v%d: %w", batchID, version, err)
			}
			reference[version] = times
			if baseline == nil {
				baseline = times
			} else if !sameTimelinePixels(baseline, times) {
				return nil, fmt.Errorf("%s version %d reference pixels diverge", batchID, version)
			}
		}
		for ti, timeResult := range baseline {
			pixelName := fmt.Sprintf("ref-%02d.rgba", ti)
			if err := os.WriteFile(filepath.Join(dir, pixelName), timeResult.Bitmap.Pixels, 0o644); err != nil {
				return nil, err
			}
		}
		for version := 1; version <= 3; version++ {
			truth := buildTruthFile(batchID, version, opts.Config, reference[version])
			truthName := fmt.Sprintf("truth.v%d.json", version)
			if err := sampletruth.Save(filepath.Join(dir, truthName), truth); err != nil {
				return nil, err
			}
			info.Truth[fmt.Sprintf("v%d", version)] = filepath.ToSlash(filepath.Join(batchID, truthName))
		}
		manifest.Batches = append(manifest.Batches, info)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(opts.OutDir, "manifest.json"), data, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func normalizeConfig(cfg vectorcore.Config) vectorcore.Config {
	if cfg == (vectorcore.Config{}) {
		cfg = vectorcore.DefaultConfig()
		cfg.MaxWorkers = 1
	}
	if cfg.MaxWorkers == 0 {
		cfg.MaxWorkers = 1
	}
	if cfg.MaxPixelBufferBytes == 0 {
		cfg.MaxPixelBufferBytes = 64 << 20
	}
	if cfg.Scale == (vectorcore.Scale{}) {
		cfg.Scale = vectorcore.UnitScale
	}
	return cfg
}

func buildTruthFile(batchID string, version int, cfg vectorcore.Config, times []*vectorcore.TimeResult) *sampletruth.File {
	file := &sampletruth.File{
		SceneID: batchID, SourceVersion: version, PixelEncoding: "rgba8",
		GeneratedBy: GeneratedBy, Config: sampletruth.ConfigFrom(cfg),
	}
	for _, tr := range times {
		sample := sampletruth.Sample{
			TimeIndex: tr.TimeIndex, TimeTicks: tr.TimeTicks, BBox: tr.Viewport.Bounds,
			BitmapWidth: tr.Bitmap.Width, BitmapHeight: tr.Bitmap.Height,
			PixelFile:   fmt.Sprintf("ref-%02d.rgba", tr.TimeIndex),
			PixelSHA256: sampletruth.PixelHash(tr.Bitmap.Pixels),
		}
		for _, p := range tr.Paths {
			pt := sampletruth.PathTruth{
				PathID: p.PathID, AnalyticArea: p.AnalyticArea, BBox: p.Bounds,
				FlattenedPoints: len(p.Flattened.Points),
			}
			for si := range p.Contour.Segments {
				points, err := vectorcore.SampleSegment(p.Contour, si, 4)
				if err != nil {
					continue
				}
				pt.CurveSamples = append(pt.CurveSamples, sampletruth.CurveSample{Segment: si, Points: points})
			}
			sample.Paths = append(sample.Paths, pt)
		}
		file.Samples = append(file.Samples, sample)
	}
	return file
}

func sameTimelinePixels(a, b []*vectorcore.TimeResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Bitmap.Width != b[i].Bitmap.Width || a[i].Bitmap.Height != b[i].Bitmap.Height {
			return false
		}
		if !bytes.Equal(a[i].Bitmap.Pixels, b[i].Bitmap.Pixels) {
			return false
		}
	}
	return true
}

func buildScene(batchID string, rng *rand.Rand, seed int64) *vectorcore.Scene {
	fps := int64(60)
	keyFrames := []int64{0, 6, 12, 18, 24, 30, 36}
	sampleFrames := []int64{0, 3, 6, 9, 12, 15, 18, 21, 24, 27, 30, 33, 36}
	scene := &vectorcore.Scene{
		ID: batchID, SourceVersion: 3, FPS: fps, Rounding: vectorcore.RoundNearest,
	}
	for _, frame := range sampleFrames {
		ticks, _ := vectorcore.FrameToTicks(frame, fps, vectorcore.RoundNearest)
		scene.SampleTimes = append(scene.SampleTimes, ticks)
	}
	cx := int64(70 + rng.Intn(70))
	cy := int64(55 + rng.Intn(50))
	baseSeed := int64((seed + rng.Int63()) % 97)
	type pathSpec struct {
		id      string
		paint   vectorcore.Paint
		dx, dy  int64
		w, h    int64
		variant int64
	}
	specs := []pathSpec{
		{
			id: "pulse-fill",
			paint: vectorcore.Paint{
				Fill: true, Color: vectorcore.RGBA{R: 56, G: 196, B: 255, A: 218},
				Opacity: 9200, Blend: vectorcore.BlendNormal, Join: vectorcore.JoinRound,
			},
			dx: 0, dy: 0, w: 86, h: 58, variant: 0,
		},
		{
			id: "ribbon-stroke",
			paint: vectorcore.Paint{
				Fill: false, StrokeWidth: 7 * vectorcore.CoordScale, Join: vectorcore.JoinRound,
				Color: vectorcore.RGBA{R: 255, G: 178, B: 71, A: 232}, Opacity: 8600, Blend: vectorcore.BlendScreen,
			},
			dx: 30, dy: -8, w: 72, h: 50, variant: 1,
		},
		{
			id: "badge-edge",
			paint: vectorcore.Paint{
				Fill: true, StrokeWidth: 4 * vectorcore.CoordScale, Join: vectorcore.JoinBevel,
				Color: vectorcore.RGBA{R: 255, G: 90, B: 140, A: 225}, Opacity: 7800, Blend: vectorcore.BlendMultiply,
			},
			dx: -26, dy: 15, w: 54, h: 42, variant: 2,
		},
	}
	for _, spec := range specs {
		path := vectorcore.Path{ID: spec.id, Paint: spec.paint}
		for ki, frame := range keyFrames {
			ticks, _ := vectorcore.FrameToTicks(frame, fps, vectorcore.RoundNearest)
			path.Keyframes = append(path.Keyframes, vectorcore.Keyframe{
				Time: ticks, Contour: contourAt(cx+spec.dx, cy+spec.dy, spec.w, spec.h, spec.variant, int64(ki), baseSeed),
			})
		}
		scene.Paths = append(scene.Paths, path)
	}
	return scene
}

var wave = []int64{0, 2, -1, 3, -2, 1, 0}

func contourAt(cx, cy, w, h, variant, k, seed int64) vectorcore.Contour {
	px := func(v int64) int64 { return v * vectorcore.CoordScale }
	phase := wave[k] + ((seed + variant*3 + k) % 5) - 2
	start := vectorcore.Point{X: cx - px(w/2) + px(phase/2), Y: cy + px(phase)}
	topLift := px((h/2)+1) + px(phase)
	topRise := px(phase/2 + 1)
	right := cx + px(w/2) + px(phase/2)
	bottom := cy + px(h/2) + px(-phase/2)
	contour := vectorcore.Contour{Start: start}
	contour.Segments = []vectorcore.Segment{
		{
			Kind: vectorcore.SegmentQuad,
			C1:   vectorcore.Point{X: cx - px(w/2) + px(phase), Y: cy - topLift},
			To:   vectorcore.Point{X: cx + px(phase/2), Y: cy - topLift + topRise},
		},
		{
			Kind: vectorcore.SegmentQuad,
			C1:   vectorcore.Point{X: right, Y: cy - topLift + px(phase)},
			To:   vectorcore.Point{X: right, Y: cy + px(phase/2)},
		},
		{
			Kind: vectorcore.SegmentCubic,
			C1:   vectorcore.Point{X: right + px(phase), Y: bottom},
			C2:   vectorcore.Point{X: cx + px(w/4), Y: bottom + px(phase)},
			To:   vectorcore.Point{X: cx - px(w/4), Y: bottom},
		},
		{
			Kind: vectorcore.SegmentCubic,
			C1:   vectorcore.Point{X: cx - px(w/2) - px(phase/2), Y: bottom},
			C2:   vectorcore.Point{X: cx - px(w/2), Y: cy + px(h/4)},
			To:   start,
		},
	}
	return contour
}
