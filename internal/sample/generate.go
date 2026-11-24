package sample

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"hdrproof/internal/color"
	"hdrproof/internal/model"
)

type GenerateOptions struct {
	Dir    string
	Seed   int64
	Routes int
}

type GenerationReport struct {
	Dir              string `json:"dir"`
	ManifestPath     string `json:"manifest_path"`
	TruthPath        string `json:"truth_path"`
	TruthFingerprint string `json:"truth_fingerprint"`
	RouteCount       int    `json:"route_count"`
	FrameCount       int    `json:"frame_count"`
}

func Generate(opts GenerateOptions) (*GenerationReport, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("sample output directory is required")
	}
	if opts.Seed == 0 {
		opts.Seed = 42
	}
	specs := routeSpecs()
	if opts.Routes > 0 && opts.Routes < len(specs) {
		specs = specs[:opts.Routes]
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	manifest := model.Manifest{SchemaVersion: model.SchemaVersion, Seed: opts.Seed}
	truth := model.Truth{SchemaVersion: model.SchemaVersion, Seed: opts.Seed}
	totalFrames := 0
	for routeIndex, route := range specs {
		rng := rand.New(rand.NewSource(opts.Seed*1009 + int64(routeIndex)*7919))
		route.TimestampsMS = make([]int64, route.FrameCount)
		for i := range route.TimestampsMS {
			route.TimestampsMS[i] = int64(math.Round(float64(i) * 1000 / route.FPS))
		}
		route.FrameFile = filepath.ToSlash(filepath.Join("routes", route.ID, "frames.hdrseq"))
		manifest.Routes = append(manifest.Routes, route)

		tr := model.RouteTruth{RouteID: route.ID}
		rawFrames := make([]model.RawFrame, 0, route.FrameCount)
		for fi := 0; fi < route.FrameCount; fi++ {
			frameTruth, linear, err := buildFrame(route, routeIndex, fi, rng)
			if err != nil {
				return nil, fmt.Errorf("route %s frame %d: %w", route.ID, fi, err)
			}
			payload, err := EncodePayload(linear, route.Encoding)
			if err != nil {
				return nil, err
			}
			rawFrames = append(rawFrames, model.RawFrame{
				Index: fi, TimestampMS: route.TimestampsMS[fi], Payload: payload,
			})
			tr.Frames = append(tr.Frames, frameTruth)
		}
		path := filepath.Join(opts.Dir, filepath.FromSlash(route.FrameFile))
		if err := WriteSequence(path, route, rawFrames); err != nil {
			return nil, err
		}
		truth.Routes = append(truth.Routes, tr)
		totalFrames += route.FrameCount
	}
	ComputeFingerprints(&truth)
	if err := writeJSON(filepath.Join(opts.Dir, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(opts.Dir, "truth.json"), truth); err != nil {
		return nil, err
	}
	if err := writeInjections(opts.Dir); err != nil {
		return nil, err
	}
	return &GenerationReport{
		Dir:              opts.Dir,
		ManifestPath:     filepath.Join(opts.Dir, "manifest.json"),
		TruthPath:        filepath.Join(opts.Dir, "truth.json"),
		TruthFingerprint: truth.Fingerprint,
		RouteCount:       len(manifest.Routes),
		FrameCount:       totalFrames,
	}, nil
}

func routeSpecs() []model.Route {
	return []model.Route{
		{
			ID: "arena-linear-bt2020", Width: 32, Height: 24, Channels: 3,
			Encoding: model.EncodingLinearF32, PeakNits: 1000, FPS: 60,
			FrameCount: 24, SegmentDurationMS: 100,
			ColorSpace: model.ColorSpace{Name: "bt2020", Primaries: color.StandardPrimaries("bt2020"), Transfer: "linear"},
		},
		{
			ID: "racing-rgb10-p3", Width: 24, Height: 16, Channels: 3,
			Encoding: model.EncodingRGB10LE, PeakNits: 4000, FPS: 30,
			FrameCount: 20, SegmentDurationMS: 200,
			ColorSpace: model.ColorSpace{Name: "display-p3", Primaries: color.StandardPrimaries("display-p3"), Transfer: "linear"},
		},
		{
			ID: "forge-linear-709", Width: 16, Height: 16, Channels: 3,
			Encoding: model.EncodingLinearF32, PeakNits: 600, FPS: 24,
			FrameCount: 26, SegmentDurationMS: 250,
			ColorSpace: model.ColorSpace{Name: "bt709", Primaries: color.StandardPrimaries("bt709"), Transfer: "linear"},
		},
		{
			ID: "ui-rgb10-bt2020", Width: 20, Height: 12, Channels: 3,
			Encoding: model.EncodingRGB10LE, PeakNits: 1500, FPS: 50,
			FrameCount: 25, SegmentDurationMS: 100,
			ColorSpace: model.ColorSpace{Name: "bt2020", Primaries: color.StandardPrimaries("bt2020"), Transfer: "linear"},
		},
	}
}

func patchRects(route model.Route) []model.PatchTruth {
	w, h := route.Width, route.Height
	pw := max(4, w/4)
	ph := max(4, h/3)
	return []model.PatchTruth{
		{Name: "patch-1", X: max(1, w/16), Y: max(1, h/12), Width: pw, Height: ph},
		{Name: "patch-2", X: max(1, w/2), Y: max(1, h/12), Width: pw, Height: ph},
		{Name: "patch-3", X: max(1, w/16), Y: max(1, h/2), Width: pw, Height: ph},
		{Name: "patch-4", X: max(1, w/2), Y: max(1, h/2), Width: pw, Height: ph},
	}
}

func buildFrame(route model.Route, routeIndex, frameIndex int, rng *rand.Rand) (model.FrameTruth, []float64, error) {
	phase := float64(frameIndex) / math.Max(1, float64(route.FrameCount-1))
	level := 0.50 + 0.08*math.Sin(2*math.Pi*(phase+0.13*float64(routeIndex)))
	bases := [][3]float64{
		{0.98, 0.38, 0.30},
		{0.28, 0.98, 0.46},
		{0.18, 0.30, 0.98},
		{0.88, 0.88, 0.88},
	}
	rects := patchRects(route)
	ft := model.FrameTruth{
		FrameIndex: frameIndex, TimestampMS: route.TimestampsMS[frameIndex], PeakNits: route.PeakNits,
		GamutBoundary: model.GamutBoundary(color.TargetPrimaries()),
	}
	linear := make([]float64, route.Width*route.Height*route.Channels)
	tintR := 0.70 + 0.05*float64(routeIndex)
	tintB := 0.95 + 0.03*float64(routeIndex)
	for y := 0; y < route.Height; y++ {
		for x := 0; x < route.Width; x++ {
			v := 0.018 + 0.075*(0.5+0.5*math.Sin(float64(x)*0.31+float64(y)*0.17+phase*6.283))
			noise := (rng.Float64() - 0.5) * 0.004
			idx := (y*route.Width + x) * route.Channels
			linear[idx] = clamp01(v*tintR + noise)
			linear[idx+1] = clamp01(v + noise*0.5)
			linear[idx+2] = clamp01(v*tintB - noise)
		}
	}
	for pi, rect := range rects {
		desired := [3]float64{
			clamp01(bases[pi][0] * level),
			clamp01(bases[pi][1] * level),
			clamp01(bases[pi][2] * level),
		}
		source, expected, err := sourceForTarget(route.ColorSpace.Primaries, desired, route.Encoding)
		if err != nil {
			return model.FrameTruth{}, nil, err
		}
		for y := rect.Y; y < rect.Y+rect.Height; y++ {
			for x := rect.X; x < rect.X+rect.Width; x++ {
				idx := (y*route.Width + x) * route.Channels
				copy(linear[idx:idx+3], source[:])
			}
		}
		rect.ExpectedLinearRGB = expected
		ft.Patches = append(ft.Patches, rect)
	}
	return ft, linear, nil
}

func sourceForTarget(primaries model.Primaries, desired [3]float64, encoding string) ([3]float64, [3]float64, error) {
	pre := [3]float64{
		color.TransferInverse("aces", desired[0]),
		color.TransferInverse("aces", desired[1]),
		color.TransferInverse("aces", desired[2]),
	}
	pre = color.Desaturate(pre)
	y := 0.2126*pre[0] + 0.7152*pre[1] + 0.0722*pre[2]
	for attempt := 0; attempt < 80; attempt++ {
		src := color.TargetToPrimaries(primaries, pre)
		if inUnit(src) {
			expected := [3]float64{
				color.TransferForward("aces", pre[0]),
				color.TransferForward("aces", pre[1]),
				color.TransferForward("aces", pre[2]),
			}
			if encoding == model.EncodingRGB10LE {
				for i := range src {
					src[i] = math.Round(src[i]*1023) / 1023
				}
				back := color.PrimariesToTarget(primaries)
				_ = back
				expected = [3]float64{
					color.TransferForward("aces", pre[0]),
					color.TransferForward("aces", pre[1]),
					color.TransferForward("aces", pre[2]),
				}
			}
			return src, expected, nil
		}
		pre = [3]float64{
			y + (pre[0]-y)*0.985,
			y + (pre[1]-y)*0.985,
			y + (pre[2]-y)*0.985,
		}
	}
	return [3]float64{}, [3]float64{}, fmt.Errorf("could not construct source value inside declared gamut")
}

func inUnit(v [3]float64) bool {
	for _, x := range v {
		if !model.Finite(x) || x < 0 || x > 1 {
			return false
		}
	}
	return true
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func ComputeFingerprints(t *model.Truth) {
	for i := range t.Routes {
		r := t.Routes[i]
		r.Fingerprint = ""
		t.Routes[i].Fingerprint = model.HashJSON(r)
	}
	clone := *t
	clone.Fingerprint = ""
	clone.Routes = append([]model.RouteTruth(nil), t.Routes...)
	for i := range clone.Routes {
		clone.Routes[i].Fingerprint = ""
	}
	t.Fingerprint = model.HashJSON(clone)
}

func VerifyFingerprints(t *model.Truth) bool {
	expected := *t
	ComputeFingerprints(&expected)
	if expected.Fingerprint != t.Fingerprint {
		return false
	}
	for i := range expected.Routes {
		if expected.Routes[i].Fingerprint != t.Routes[i].Fingerprint {
			return false
		}
	}
	return true
}

func writeInjections(dir string) error {
	base := filepath.Join(dir, "injections")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	cases := map[string]model.Injections{
		"bad_frame.json": {
			BadFrames: []model.BadFrame{{RouteID: "racing-rgb10-p3", Segment: 0, FrameIndex: 0, Field: "frame_payload"}},
		},
		"meta_conflict.json": {
			MetaConflicts: []model.MetaConflict{{RouteID: "arena-linear-bt2020", Segment: 0, Field: "width", Value: 999}},
		},
		"parameter_out_of_range.json": {
			ParameterInjections: []model.ParameterInjection{{RouteID: "racing-rgb10-p3", Segment: 0, Field: "target_peak_nits", Value: 999}},
		},
		"segment_failure.json": {
			SegmentFailures: []model.SegmentFailure{{RouteID: "ui-rgb10-bt2020", Segment: 0, Field: "worker_injected_failure", Message: "controlled segment decoder failure"}},
		},
	}
	for name, injections := range cases {
		if err := writeJSON(filepath.Join(base, name), injections); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadInjections(path string) (model.Injections, error) {
	var injections model.Injections
	if path == "" {
		return injections, nil
	}
	if err := readJSON(path, &injections); err != nil {
		return injections, err
	}
	return injections, nil
}

func TimestampNowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
