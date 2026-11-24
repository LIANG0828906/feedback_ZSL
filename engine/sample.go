package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const SampleSeed int64 = 0x5EED2026

type SampleManifest struct {
	Seed            int64    `json:"seed"`
	SceneID         string   `json:"sceneId"`
	Versions        []string `json:"versions"`
	SceneFiles      []string `json:"sceneFiles"`
	TruthFile       string   `json:"truthFile"`
	SampleTimesMs   []int64  `json:"sampleTimesMs"`
	KeyframeTimesMs []int64  `json:"keyframeTimesMs"`
	Notes           string   `json:"notes"`
}

func q10(v int64) int64 { return v * CoordScale }

func pt(x, y int64) Vec2 { return Vec2{X: q10(x), Y: q10(y)} }

func deform(p Vec2, phase int64) Vec2 {
	cx, cy := q10(380), q10(300)
	dx := divRound((p.Y-cy)*phase, 220, RoundNearest) + q10(phase*7)
	dy := -divRound((p.X-cx)*phase, 340, RoundNearest) - q10(phase*4)
	return Vec2{X: p.X + dx, Y: p.Y + dy}
}

func morphSegments(segments []Segment, phase int64) []Segment {
	out := make([]Segment, len(segments))
	for i, seg := range segments {
		out[i] = Segment{
			Kind:  seg.Kind,
			Start: deform(seg.Start, phase),
			C1:    deform(seg.C1, phase),
			C2:    deform(seg.C2, phase),
			End:   deform(seg.End, phase),
		}
	}
	return out
}

func sampleScene() *Scene {
	p1Segments := []Segment{
		{Kind: Cubic, Start: pt(180, 180), C1: pt(300, 82), C2: pt(470, 82), End: pt(600, 180)},
		{Kind: Quadratic, Start: pt(600, 180), C1: pt(680, 300), End: pt(600, 420)},
		{Kind: Cubic, Start: pt(600, 420), C1: pt(470, 518), C2: pt(300, 518), End: pt(180, 420)},
		{Kind: Quadratic, Start: pt(180, 420), C1: pt(100, 300), End: pt(180, 180)},
	}
	p2Segments := []Segment{
		{Kind: Cubic, Start: pt(360, 220), C1: pt(430, 135), C2: pt(558, 180), End: pt(610, 300)},
		{Kind: Quadratic, Start: pt(610, 300), C1: pt(648, 390), End: pt(558, 470)},
		{Kind: Cubic, Start: pt(558, 470), C1: pt(460, 540), C2: pt(320, 500), End: pt(260, 390)},
		{Kind: Quadratic, Start: pt(260, 390), C1: pt(232, 290), End: pt(360, 220)},
	}
	phaseAt := []int64{0, 1, 0}
	keyTimes := []int64{0, 500, 1000}
	p1Keys := make([]Keyframe, 0, len(phaseAt))
	p2Keys := make([]Keyframe, 0, len(phaseAt))
	for i, ph := range phaseAt {
		p1Keys = append(p1Keys, Keyframe{Time: millisToTicks(keyTimes[i]), Segments: morphSegments(p1Segments, ph)})
		p2Keys = append(p2Keys, Keyframe{Time: millisToTicks(keyTimes[i]), Segments: morphSegments(p2Segments, ph)})
	}
	return &Scene{
		ID:               fmt.Sprintf("deterministic-seed-%x", SampleSeed),
		SourceVersion:    Version3,
		Interpolation:    InterpOptions{Mode: "linear", PrecisionBits: 12},
		Tolerance:        64,
		Subsamples:       4,
		Scale:            CoordScale / 2,
		BlendVersion:     "blend-v1",
		AlgorithmVersion: "algo-v1",
		Samples:          []int64{0, millisToTicks(250), millisToTicks(500), millisToTicks(750), millisToTicks(1000)},
		Paths: []Path{
			{
				ID: "ember", HasFill: true, HasStroke: true,
				Fill: Color{R: 236, G: 92, B: 54, A: 232}, StrokeColor: Color{R: 82, G: 28, B: 20, A: 255},
				StrokeWidth: q10(7), LineJoin: JoinRound, Blend: BlendMultiply, FillRule: "nonzero", Opacity: 245,
				Keyframes: p1Keys,
			},
			{
				ID: "sapphire", HasFill: true, HasStroke: false,
				Fill: Color{R: 35, G: 130, B: 210, A: 210}, StrokeColor: Color{R: 35, G: 130, B: 210, A: 210},
				LineJoin: JoinMiter, Blend: BlendScreen, FillRule: "evenodd", Opacity: 220,
				Keyframes: p2Keys,
			},
		},
	}
}

func truthForScene(scene *Scene) (*TruthDocument, error) {
	doc := &TruthDocument{
		SceneID: scene.ID, CoordinateUnit: "q10", TimeUnit: "us", FPS: SampleFPS,
	}
	truthQuality := QualityDecision{Mode: "truth", Tolerance: 8, Subsamples: 8, PrecisionBits: 20}
	for _, t := range scene.Samples {
		lines, err := FlattenAt(scene, t)
		if err != nil {
			return nil, err
		}
		truthFrame := TruthFrame{Time: t, TimeMs: ticksToMillis(t, RoundFloor)}
		for pi := range scene.Paths {
			p := &scene.Paths[pi]
			segments, err := EvalPathAt(p, t, scene.Interpolation)
			if err != nil {
				return nil, err
			}
			dense := lines[pi].Points
			tp := TruthPath{
				ID:                p.ID,
				Area:              PolylineArea(dense),
				Bounds:            lines[pi].Bounds,
				CoverageRatio:     float64(PolylineArea(dense)) / float64(max64(lines[pi].Bounds.Width()*lines[pi].Bounds.Height(), 1)),
				CoverageBBoxRatio: float64(PolylineArea(dense)) / float64(max64(lines[pi].Bounds.Width()*lines[pi].Bounds.Height(), 1)),
			}
			for si, seg := range segments {
				for _, tq := range []int64{q30 / 4, q30 / 2, 3 * q30 / 4} {
					tp.Samples = append(tp.Samples, CurveSample{Segment: si, T: tq, Point: EvalCurvePoint(seg, tq)})
					tp.CurvePointCount++
				}
			}
			truthFrame.Paths = append(truthFrame.Paths, tp)
			truthFrame.Bounds = truthFrame.Bounds.Union(lines[pi].Bounds)
		}
		hi, err := RenderFrame(scene, t, 0, truthQuality)
		if err != nil {
			return nil, err
		}
		for i := range truthFrame.Paths {
			if i < len(hi.Paths) {
				truthFrame.Paths[i].CoverageRatio = hi.Paths[i].CoverageRatio
				truthFrame.Paths[i].CoverageBBoxRatio = hi.Paths[i].CoverageBBoxRatio
			}
		}
		payload, _ := json.Marshal(struct {
			Time   int64       `json:"time"`
			Bounds Rect        `json:"bounds"`
			Paths  []TruthPath `json:"paths"`
		}{truthFrame.Time, truthFrame.Bounds, truthFrame.Paths})
		sum := sha256.Sum256(payload)
		truthFrame.Checksum = hex.EncodeToString(sum[:])
		doc.Samples = append(doc.Samples, truthFrame)
	}
	return doc, nil
}

// GenerateSample writes deterministic v1/v2/v3 scene documents and truth.
func GenerateSample(dir string) (*SampleManifest, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	scene := sampleScene()
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	manifest := &SampleManifest{
		Seed: SampleSeed, SceneID: scene.ID, Versions: []string{"1.0", "2.0.0", "3.0"},
		TruthFile: "truth.json", KeyframeTimesMs: []int64{0, 500, 1000},
		Notes: "Fixed-seed generator. Truth curve points/area/bounds are computed from the generated contours; coverage is an 8x AA reference raster.",
	}
	for _, v := range scene.Samples {
		manifest.SampleTimesMs = append(manifest.SampleTimesMs, ticksToMillis(v, RoundFloor))
	}
	for _, target := range []Version{Version1, Version2, Version3} {
		res, err := ExportScene(scene, target, RoundFloor)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("scene_v%c.json", target[0])
		if err := os.WriteFile(filepath.Join(dir, name), res.Data, 0o644); err != nil {
			return nil, err
		}
		manifest.SceneFiles = append(manifest.SceneFiles, name)
	}
	truth, err := truthForScene(scene)
	if err != nil {
		return nil, err
	}
	truthData, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truthData, 0o644); err != nil {
		return nil, err
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestData, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func LoadTruthFile(path string) (*TruthDocument, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc TruthDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}
