package morph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testScene(t *testing.T) (*Scene, *Truth) {
	t.Helper()
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "errors", 3, 2, 5, 5
	opts.Scale = 6
	opts.ReferenceGrid = 24
	s, tr, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, tr
}

func cloneScene(t *testing.T, s *Scene) *Scene {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out Scene
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func requireCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("not structured error: %v", err)
	}
	if e.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, e.Code, err)
	}
	return e
}

func TestErrorDecode(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadScene(bad)
	requireCode(t, err, CodeDecode)
	_, err = LoadTruth(filepath.Join(dir, "missing.json"))
	requireCode(t, err, CodeDecode)
}

func TestErrorPathKeyStyle(t *testing.T) {
	s, _ := testScene(t)
	open := cloneScene(t, s)
	last := len(open.Paths[0].Keyframes[0].Segments) - 1
	open.Paths[0].Keyframes[0].Segments[last].P2.X += 0.1
	e := requireCode(t, ValidateScene(open), CodePath)
	if e.PathID != open.Paths[0].ID || e.Field == "" {
		t.Fatalf("context: %+v", e)
	}

	badKey := cloneScene(t, s)
	badKey.Paths[1].Keyframes[1].Time = -1
	e = requireCode(t, ValidateScene(badKey), CodeKey)
	if e.PathID != badKey.Paths[1].ID || e.Field == "" {
		t.Fatalf("context: %+v", e)
	}

	badStyle := cloneScene(t, s)
	badStyle.Paths[2].Opacity = 1.5
	e = requireCode(t, ValidateScene(badStyle), CodeStyle)
	if e.PathID != badStyle.Paths[2].ID || e.Field != "opacity" {
		t.Fatalf("context: %+v", e)
	}

	badCoord := cloneScene(t, s)
	badCoord.Paths[0].Keyframes[0].Segments[0].P0.X = badCoord.CoordMax + 1
	requireCode(t, ValidateScene(badCoord), CodeStyle)

	dup := cloneScene(t, s)
	dup.Paths[1].ID = dup.Paths[0].ID
	requireCode(t, ValidateScene(dup), CodePath)
}

func TestErrorParams(t *testing.T) {
	s, _ := testScene(t)
	finite := EvalConfig{Interpolation: "linear", Scale: 1, MidTolerance: 0}
	_, err := FlattenPath(&s.Paths[0], s.SampleTimes[0], finite)
	e := requireCode(t, err, CodeParam)
	if e.Field != "mid_tolerance" {
		t.Fatalf("field=%s", e.Field)
	}
	_, err = FlattenPath(&s.Paths[0], s.SampleTimes[0], EvalConfig{Interpolation: "linear", Scale: -1, MidTolerance: .1})
	e = requireCode(t, err, CodeParam)
	if e.Field != "scale" {
		t.Fatalf("field=%s", e.Field)
	}
	_, err = FlattenPath(&s.Paths[0], s.SampleTimes[len(s.SampleTimes)-1]+1, EvalConfig{Interpolation: "linear", Scale: 1, MidTolerance: .1})
	e = requireCode(t, err, CodeParam)
	if e.Field != "sample_time" || e.Time == 0 {
		t.Fatalf("context: %+v", e)
	}
}

func TestErrorScaleAndDiverge(t *testing.T) {
	s, tr := testScene(t)
	cfg := DefaultConfigForScene(s)
	cfg.Checkpoint = t.TempDir()
	cfg.Adaptive.ForceFine = true
	cfg.Runtime.Workers = 1
	e, err := NewEngine(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	fine := e.Results()
	approx := make([]*SampleResult, len(fine))
	for i, r := range fine {
		approx[i] = cloneResult(r)
	}
	approx[0].PathStats[0].CoverageArea *= 2
	approx[0].PathStats[0].CoverageBBox = PixelRect{}
	report, err := CompareQuality(s, tr, ConsistencyTolerance{RelativeArea: 1e-9, BBoxIoU: 1, MeanCoverage: 1}, fine, approx)
	eScale := requireCode(t, err, CodeScale)
	if eScale.Sample != 0 || eScale.PathID == "" || report == nil {
		t.Fatalf("context: %+v", eScale)
	}

	broken := make([]*SampleResult, len(fine))
	for i, r := range fine {
		broken[i] = cloneResult(r)
	}
	if len(broken[0].Gray) > 0 {
		broken[0].Gray[len(broken[0].Gray)/2] ^= 0xff
	}
	_, err = CompareResultFrames(s, []string{"full", "broken"}, [][]*SampleResult{fine, broken})
	eDiv := requireCode(t, err, CodeDiverge)
	if !eDiv.HasPixel || eDiv.Sample != 0 {
		t.Fatalf("context: %+v", eDiv)
	}
}

func cloneResult(r *SampleResult) *SampleResult {
	data, _ := json.Marshal(r)
	var out SampleResult
	_ = json.Unmarshal(data, &out)
	out.Gray = append([]uint8(nil), r.Gray...)
	out.PathStats = append([]PathFrameStats(nil), r.PathStats...)
	out.Flattened = nil
	for _, f := range r.Flattened {
		cp := *f
		cp.Points = append([]Vec2(nil), f.Points...)
		out.Flattened = append(out.Flattened, &cp)
	}
	return &out
}
