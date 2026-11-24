package sample

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"hdrproof/internal/apperr"
	"hdrproof/internal/color"
	"hdrproof/internal/model"
)

type LoadOptions struct {
	DecodeFrames bool
}

func DefaultLoadOptions() LoadOptions { return LoadOptions{DecodeFrames: true} }

func Load(dir string) (*model.Dataset, error) {
	return LoadWithOptions(dir, DefaultLoadOptions())
}

// LoadMetadata validates and loads declarations and truth data without reading
// or decoding frame-sequence payloads. It is used by the independent auditor.
func LoadMetadata(dir string) (*model.Dataset, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	truthPath := filepath.Join(dir, "truth.json")
	var manifest model.Manifest
	if err := readJSON(manifestPath, &manifest); err != nil {
		return nil, apperr.New(apperr.CodeMeta, "", -1, -1, "manifest", "valid JSON manifest", err.Error(), err)
	}
	var truth model.Truth
	if err := readJSON(truthPath, &truth); err != nil {
		return nil, apperr.New(apperr.CodeMeta, "", -1, -1, "truth", "valid JSON truth list", err.Error(), err)
	}
	if err := validateManifestAndTruth(dir, &manifest, &truth, false); err != nil {
		return nil, err
	}
	ds := &model.Dataset{
		Dir: dir, ManifestPath: manifestPath, TruthPath: truthPath,
		Manifest: manifest, Truth: truth, TruthFingerprint: truth.Fingerprint,
	}
	for _, route := range manifest.Routes {
		tr := findTruthRoute(&truth, route.ID)
		if tr == nil {
			continue
		}
		ds.Routes = append(ds.Routes, &model.RouteData{Route: route, Truth: *tr})
	}
	return ds, nil
}

func LoadWithOptions(dir string, opts LoadOptions) (*model.Dataset, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	truthPath := filepath.Join(dir, "truth.json")
	var manifest model.Manifest
	if err := readJSON(manifestPath, &manifest); err != nil {
		return nil, apperr.New(apperr.CodeMeta, "", -1, -1, "manifest", "valid JSON manifest", err.Error(), err)
	}
	var truth model.Truth
	if err := readJSON(truthPath, &truth); err != nil {
		return nil, apperr.New(apperr.CodeMeta, "", -1, -1, "truth", "valid JSON truth list", err.Error(), err)
	}
	if err := validateManifestAndTruth(dir, &manifest, &truth, true); err != nil {
		return nil, err
	}
	ds := &model.Dataset{
		Dir: dir, ManifestPath: manifestPath, TruthPath: truthPath,
		Manifest: manifest, Truth: truth, TruthFingerprint: truth.Fingerprint,
	}
	truthByRoute := make(map[string]*model.RouteTruth, len(truth.Routes))
	for i := range truth.Routes {
		truthByRoute[truth.Routes[i].RouteID] = &truth.Routes[i]
	}
	for _, route := range manifest.Routes {
		routeCopy := route
		frames, err := ReadSequence(filepath.Join(dir, filepath.FromSlash(route.FrameFile)), routeCopy)
		if err != nil {
			return nil, err
		}
		rd := &model.RouteData{
			Route: routeCopy, Truth: *truthByRoute[routeCopy.ID], Frames: frames,
		}
		if opts.DecodeFrames {
			rd.DecodedFrames = make([]model.DecodedFrame, 0, len(frames))
			for _, frame := range frames {
				linear, err := DecodePayload(frame.Payload, routeCopy)
				if err != nil {
					if ae, ok := apperr.As(err); ok {
						ae.Segment = model.SegmentOf(routeCopy, frame.TimestampMS)
						ae.Frame = frame.Index
					}
					return nil, err
				}
				rd.DecodedFrames = append(rd.DecodedFrames, model.DecodedFrame{RawFrame: frame, LinearRGB: linear})
			}
		}
		ds.Routes = append(ds.Routes, rd)
	}
	return ds, nil
}

func readJSON(path string, out interface{}) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func metaErr(route, field, expected, message string) error {
	return apperr.New(apperr.CodeMeta, route, -1, -1, field, expected, message, nil)
}

func validateManifestAndTruth(dir string, m *model.Manifest, t *model.Truth, requireFrameFiles bool) error {
	if m.SchemaVersion != model.SchemaVersion {
		return metaErr("", "schema_version", model.SchemaVersion, fmt.Sprintf("received %q", m.SchemaVersion))
	}
	if t.SchemaVersion != model.SchemaVersion {
		return metaErr("", "truth.schema_version", model.SchemaVersion, fmt.Sprintf("received %q", t.SchemaVersion))
	}
	if m.Seed != t.Seed {
		return metaErr("", "seed", fmt.Sprint(m.Seed), fmt.Sprintf("truth seed is %d", t.Seed))
	}
	if strings.TrimSpace(t.Fingerprint) == "" {
		return metaErr("", "truth.fingerprint", "non-empty SHA-256", "missing truth fingerprint")
	}
	if len(m.Routes) == 0 {
		return metaErr("", "routes", "at least one route", "manifest has no routes")
	}
	if len(t.Routes) != len(m.Routes) {
		return metaErr("", "truth.routes", fmt.Sprintf("%d routes", len(m.Routes)),
			fmt.Sprintf("received %d truth routes", len(t.Routes)))
	}
	seen := make(map[string]bool, len(m.Routes))
	truthSeen := make(map[string]bool, len(t.Routes))
	for i := range t.Routes {
		if truthSeen[t.Routes[i].RouteID] {
			return metaErr(t.Routes[i].RouteID, "truth.route_id", "unique route id", "duplicate truth route")
		}
		truthSeen[t.Routes[i].RouteID] = true
	}
	for i := range m.Routes {
		r := &m.Routes[i]
		if err := validateRoute(dir, r, seen, requireFrameFiles); err != nil {
			return err
		}
		tr := findTruthRoute(t, r.ID)
		if tr == nil {
			return metaErr(r.ID, "truth.route_id", r.ID, "missing truth route")
		}
		if err := validateTruthRoute(r, tr); err != nil {
			return err
		}
	}
	return nil
}

func findTruthRoute(t *model.Truth, id string) *model.RouteTruth {
	for i := range t.Routes {
		if t.Routes[i].RouteID == id {
			return &t.Routes[i]
		}
	}
	return nil
}

func validateRoute(dir string, r *model.Route, seen map[string]bool, requireFrameFile bool) error {
	if strings.TrimSpace(r.ID) == "" {
		return metaErr("", "route.id", "non-empty route id", "missing route id")
	}
	if seen[r.ID] {
		return metaErr(r.ID, "route.id", "unique route id", "duplicate route id")
	}
	seen[r.ID] = true
	if r.Width <= 0 {
		return metaErr(r.ID, "width", "> 0", fmt.Sprintf("received %d", r.Width))
	}
	if r.Height <= 0 {
		return metaErr(r.ID, "height", "> 0", fmt.Sprintf("received %d", r.Height))
	}
	if r.Channels != 3 {
		return metaErr(r.ID, "channels", "3", fmt.Sprintf("received %d", r.Channels))
	}
	if BytesPerChannel(r.Encoding) == 0 {
		return metaErr(r.ID, "encoding", model.EncodingLinearF32+"|"+model.EncodingRGB10LE,
			fmt.Sprintf("received %q", r.Encoding))
	}
	if !model.Finite(r.PeakNits) || r.PeakNits <= 0 {
		return metaErr(r.ID, "peak_nits", "finite > 0", fmt.Sprintf("received %v", r.PeakNits))
	}
	if !model.Finite(r.FPS) || r.FPS <= 0 || r.FPS > 1000 {
		return metaErr(r.ID, "fps", "finite 0..1000", fmt.Sprintf("received %v", r.FPS))
	}
	if r.FrameCount <= 0 || len(r.TimestampsMS) != r.FrameCount {
		return metaErr(r.ID, "frame_count", fmt.Sprintf("%d timestamps", r.FrameCount),
			fmt.Sprintf("received %d timestamps", len(r.TimestampsMS)))
	}
	if r.SegmentDurationMS <= 0 {
		return metaErr(r.ID, "segment_duration_ms", "> 0", fmt.Sprintf("received %d", r.SegmentDurationMS))
	}
	for i, ts := range r.TimestampsMS {
		if ts < 0 || (i > 0 && ts <= r.TimestampsMS[i-1]) {
			return metaErr(r.ID, "timestamps_ms", "strictly increasing non-negative timestamps",
				fmt.Sprintf("frame %d has %d after %d", i, ts, previousTimestamp(r.TimestampsMS, i)))
		}
	}
	expectedInterval := 1000 / r.FPS
	actualInterval := float64(r.TimestampsMS[len(r.TimestampsMS)-1]-r.TimestampsMS[0]) / float64(len(r.TimestampsMS)-1)
	if math.Abs(actualInterval-expectedInterval) > 2 {
		return metaErr(r.ID, "fps", fmt.Sprintf("%.3f (%.3f ms/frame)", r.FPS, expectedInterval),
			fmt.Sprintf("timestamps show %.3f ms/frame", actualInterval))
	}
	if r.ColorSpace.Transfer != "linear" {
		return metaErr(r.ID, "color_space.transfer", "linear", fmt.Sprintf("received %q", r.ColorSpace.Transfer))
	}
	if err := validateColorSpace(r.ID, r.ColorSpace); err != nil {
		return err
	}
	if strings.TrimSpace(r.FrameFile) == "" {
		return metaErr(r.ID, "frame_file", "relative frame sequence path", "missing frame_file")
	}
	clean := filepath.Clean(filepath.FromSlash(r.FrameFile))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return metaErr(r.ID, "frame_file", "path inside sample directory", "path escapes sample directory")
	}
	if requireFrameFile {
		if _, err := os.Stat(filepath.Join(dir, clean)); err != nil {
			return apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_file",
				"existing frame sequence", err.Error(), err)
		}
	}
	return nil
}

func previousTimestamp(ts []int64, i int) int64 {
	if i == 0 {
		return -1
	}
	return ts[i-1]
}

func validateColorSpace(route string, cs model.ColorSpace) error {
	var standard model.Primaries
	switch cs.Name {
	case "bt709":
		standard = color.StandardPrimaries("bt709")
	case "display-p3":
		standard = color.StandardPrimaries("display-p3")
	case "bt2020":
		standard = color.StandardPrimaries("bt2020")
	default:
		return metaErr(route, "color_space.name", "bt709|display-p3|bt2020",
			fmt.Sprintf("received %q", cs.Name))
	}
	checks := []struct {
		field    string
		actual   model.Chromaticity
		expected model.Chromaticity
	}{
		{"color_space.primaries.red", cs.Primaries.Red, standard.Red},
		{"color_space.primaries.green", cs.Primaries.Green, standard.Green},
		{"color_space.primaries.blue", cs.Primaries.Blue, standard.Blue},
		{"color_space.primaries.white", cs.Primaries.White, standard.White},
	}
	for _, c := range checks {
		if !close(c.actual.X, c.expected.X) || !close(c.actual.Y, c.expected.Y) {
			return metaErr(route, c.field, fmt.Sprintf("(%.4f, %.4f)", c.expected.X, c.expected.Y),
				fmt.Sprintf("received (%.4f, %.4f)", c.actual.X, c.actual.Y))
		}
	}
	return nil
}

func close(a, b float64) bool { return math.Abs(a-b) <= 1e-6 }

func validateTruthRoute(r *model.Route, tr *model.RouteTruth) error {
	if tr.Fingerprint == "" {
		return metaErr(r.ID, "truth.fingerprint", "non-empty SHA-256", "missing route truth fingerprint")
	}
	if len(tr.Frames) != r.FrameCount {
		return metaErr(r.ID, "truth.frame_count", fmt.Sprint(r.FrameCount),
			fmt.Sprintf("received %d truth frames", len(tr.Frames)))
	}
	target := color.TargetPrimaries()
	for i := range tr.Frames {
		f := &tr.Frames[i]
		if f.FrameIndex != i {
			return metaErr(r.ID, "truth.frame_index", fmt.Sprint(i), fmt.Sprintf("received %d", f.FrameIndex))
		}
		if f.TimestampMS != r.TimestampsMS[i] {
			return metaErr(r.ID, "truth.timestamps_ms", fmt.Sprint(r.TimestampsMS[i]),
				fmt.Sprintf("frame %d received %d", i, f.TimestampMS))
		}
		if f.PeakNits != r.PeakNits {
			return metaErr(r.ID, "truth.peak_nits", fmt.Sprint(r.PeakNits),
				fmt.Sprintf("frame %d received %v", i, f.PeakNits))
		}
		if len(f.GamutBoundary) != 3 {
			return metaErr(r.ID, "truth.gamut_boundary", "3 target gamut vertices",
				fmt.Sprintf("frame %d received %d", i, len(f.GamutBoundary)))
		}
		expectedBoundary := []model.Chromaticity{target.Red, target.Green, target.Blue}
		for j := range expectedBoundary {
			if !close(f.GamutBoundary[j].X, expectedBoundary[j].X) || !close(f.GamutBoundary[j].Y, expectedBoundary[j].Y) {
				return metaErr(r.ID, "truth.gamut_boundary", "BT.709 target boundary",
					fmt.Sprintf("frame %d vertex %d differs", i, j))
			}
		}
		if len(f.Patches) == 0 {
			return metaErr(r.ID, "truth.patches", "at least one patch", fmt.Sprintf("frame %d has none", i))
		}
		names := make(map[string]bool)
		for _, p := range f.Patches {
			if p.Name == "" || names[p.Name] {
				return metaErr(r.ID, "truth.patches.name", "unique non-empty patch names",
					fmt.Sprintf("frame %d has invalid patch %q", i, p.Name))
			}
			names[p.Name] = true
			if p.X < 0 || p.Y < 0 || p.Width <= 0 || p.Height <= 0 ||
				p.X+p.Width > r.Width || p.Y+p.Height > r.Height {
				return metaErr(r.ID, "truth.patches.rect", "rectangle inside frame",
					fmt.Sprintf("frame %d patch %s is outside %dx%d", i, p.Name, r.Width, r.Height))
			}
			for c, v := range p.ExpectedLinearRGB {
				if !model.Finite(v) || v < 0 || v > 1 {
					return metaErr(r.ID, "truth.patches.expected_linear_rgb", "finite values in [0,1]",
						fmt.Sprintf("frame %d patch %s channel %d is %v", i, p.Name, c, v))
				}
			}
		}
	}
	return nil
}
