package repair

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
)

func testSample(t *testing.T) generator.Sample {
	t.Helper()
	s, err := generator.Generate(4, 42, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDiagnosticMatchesTruth(t *testing.T) {
	s := testSample(t)
	diag, e := Diagnose(s.Mesh, s.Truth)
	if e != nil {
		t.Fatalf("diagnose failed: %v", e)
	}
	if diag.Stats.NonManifoldEdges != s.Truth.Defects.NonManifoldEdges ||
		diag.Stats.DuplicateVertices != s.Truth.Defects.DuplicateVertices ||
		diag.Stats.FlippedFaces != s.Truth.Defects.FlippedFaces ||
		diag.Stats.BoundaryLoops != s.Truth.Defects.BoundaryLoops ||
		diag.Stats.SelfIntersections != s.Truth.Defects.SelfIntersections {
		t.Fatalf("diagnostic stats do not match truth: %+v vs %+v", diag.Stats, s.Truth.Defects)
	}
	if s.Truth.Defects.NonManifoldEdges < 1 || s.Truth.Defects.DuplicateVertices < 1 ||
		s.Truth.Defects.FlippedFaces < 1 || s.Truth.Defects.ThinSheetComponents < 1 ||
		s.Truth.Defects.SelfIntersections < 1 {
		t.Fatalf("sample is missing required defect classes: %+v", s.Truth.Defects)
	}
}

func TestGeometryErrorsIdentifyFirstFace(t *testing.T) {
	s := testSample(t)
	bad := s.Mesh.Clone()
	bad.Faces[0] = mesh.Face{A: -1, B: bad.Faces[0].B, C: bad.Faces[0].C}
	_, e := Diagnose(bad, s.Truth)
	if e == nil || e.Code != errs.CodeGeom || e.FaceIndex == nil || *e.FaceIndex != 0 {
		t.Fatalf("invalid index error = %+v", e)
	}

	short := s.Mesh.Clone()
	short.Faces = short.Faces[:len(short.Faces)-1]
	_, e = Diagnose(short, s.Truth)
	if e == nil || e.Code != errs.CodeGeom || e.FaceIndex == nil {
		t.Fatalf("face-count mismatch error = %+v", e)
	}

	degenerate := s.Mesh.Clone()
	degenerate.Faces[0] = mesh.Face{A: 0, B: 0, C: 2}
	_, e = Diagnose(degenerate, s.Truth)
	if e == nil || e.Code != errs.CodeGeom || e.FaceIndex == nil || *e.FaceIndex != 0 {
		t.Fatalf("degenerate face error = %+v", e)
	}
}

func TestExactAndSpatialProduceTruthTopology(t *testing.T) {
	s := testSample(t)
	cfg := DefaultConfig()
	for _, mode := range []Mode{ModeExact, ModeSpatial} {
		cfg.Mode = mode
		res, e := RunAll(s, cfg)
		if e != nil {
			t.Fatalf("%s run failed: %v", mode, e)
		}
		if res.Stats.Vertices != s.Truth.Repaired.Vertices ||
			res.Stats.Faces != s.Truth.Repaired.Faces ||
			res.Stats.EulerCharacteristic != s.Truth.Repaired.Euler {
			t.Fatalf("%s topology=%+v truth=%+v", mode, res.Stats, s.Truth.Repaired)
		}
		if res.Stats.NonManifoldEdges != 0 || res.Stats.BoundaryLoops != 0 ||
			res.Stats.FlippedFaces != 0 || res.Stats.SelfIntersections != 0 {
			t.Fatalf("%s repair defects remain: %+v", mode, res.Stats)
		}
		if res.FillArea <= 0 {
			t.Fatalf("%s fill area should be positive", mode)
		}
	}
}

func TestScaleConsistencyAndDeterminism(t *testing.T) {
	s := testSample(t)
	cfg := DefaultConfig()
	report, e := RunConsistency(s, cfg)
	if e != nil {
		t.Fatalf("consistency failed: %v", e)
	}
	if !report.Passed || report.QualityFingerprint == "" {
		t.Fatalf("bad consistency report: %+v", report)
	}
	report2, e := RunConsistency(s, cfg)
	if e != nil {
		t.Fatalf("second consistency failed: %v", e)
	}
	if report.QualityFingerprint != report2.QualityFingerprint {
		t.Fatalf("quality fingerprint is not deterministic")
	}
	det, e := RunDeterminism(s, cfg)
	if e != nil {
		t.Fatalf("determinism failed: %v", e)
	}
	if !det.Equal || det.SerialHash != det.ParallelHash {
		t.Fatalf("parallel and serial differ: %+v", det)
	}
}

func TestErrorCodes(t *testing.T) {
	s := testSample(t)
	_, _, e := WeldAndCanonicalize(s.Mesh, WeldOptions{Tolerance: 2.0, MinComponentPct: 0.05})
	if e == nil || e.Code != errs.CodeTolo || e.SuggestedTolerance <= 0 {
		t.Fatalf("over-weld error=%+v", e)
	}
	if e := ValidateSmoothOptions(SmoothOptions{Weight: 0, Iterations: 8}); e == nil || e.Code != errs.CodePara {
		t.Fatalf("parameter error=%+v", e)
	}
	badCfg := DefaultConfig()
	badCfg.SmoothWeight = 0
	if _, err := NewPipeline(s, badCfg, filepath.Join(t.TempDir(), "bad-para"), false); err == nil {
		t.Fatal("invalid smoothing weight should fail pipeline construction")
	} else if ce, _ := errs.As(err); ce == nil || ce.Code != errs.CodePara {
		t.Fatalf("pipeline parameter error=%v", err)
	}
	badCfg = DefaultConfig()
	badCfg.WeldTolerance = -1
	if _, err := NewPipeline(s, badCfg, filepath.Join(t.TempDir(), "bad-tol"), false); err == nil {
		t.Fatal("negative weld tolerance should fail pipeline construction")
	} else if ce, _ := errs.As(err); ce == nil || ce.Code != errs.CodeTolo {
		t.Fatalf("pipeline tolerance error=%v", err)
	}
	_, _, oe := UnifyOrientation(mobiusStrip())
	if oe == nil || oe.Code != errs.CodeOrient || len(oe.ConflictComponents) == 0 {
		t.Fatalf("orientation error=%+v", oe)
	}
	_, se := ValidateScaleQuality(
		ScaleSnapshot{Vertices: 10, Faces: 20, Euler: 2, FillArea: 1},
		ScaleSnapshot{Vertices: 10, Faces: 21, Euler: 1, FillArea: 2},
	)
	if se == nil || se.Code != errs.CodeScale || se.Item != "faces" ||
		se.Left == nil || se.Right == nil {
		t.Fatalf("scale error=%+v", se)
	}
}

func TestCheckpointResumeAndInvalidation(t *testing.T) {
	s := testSample(t)
	cfg := DefaultConfig()
	cfg.Mode = ModeExact
	dir := t.TempDir()
	cp := filepath.Join(dir, "checkpoints")
	p1, err := NewPipeline(s, cfg, cp, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p1.RunStage(0); err != nil {
		t.Fatal(err)
	}
	if err := p1.RunStage(1); err != nil {
		t.Fatal(err)
	}

	p2, err := NewPipeline(s, cfg, cp, true)
	if err != nil {
		t.Fatal(err)
	}
	st := p2.State()
	if st.Progress.Committed != 2 {
		t.Fatalf("resume committed=%d, want 2; status=%s reason=%s", st.Progress.Committed, st.ResumeStatus, st.ResumeReason)
	}
	if err := p2.Run(); err != nil {
		t.Fatal(err)
	}
	final, ok := p2.FinalMesh()
	if !ok {
		t.Fatal("final mesh not available")
	}
	whole, e := RunAll(s, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if final.ContentHash() != whole.Mesh.ContentHash() {
		t.Fatalf("resumed mesh differs")
	}
	if e := p2.RunStage(0); e == nil {
		t.Fatal("committed stage should reject rerun")
	} else if ce, _ := errs.As(e); ce == nil || ce.Code != errs.CodeState {
		t.Fatalf("frozen stage error=%v", e)
	}

	changed := cfg
	changed.SmoothWeight = 0.4
	p3, err := NewPipeline(s, changed, cp, true)
	if err != nil {
		t.Fatal(err)
	}
	if p3.State().Progress.Committed != 0 || p3.State().ResumeStatus != "invalidated" {
		t.Fatalf("version mismatch should invalidate all: %+v", p3.State().ResumeStatus)
	}
}

func TestCheckpointHashCorruptionInvalidates(t *testing.T) {
	s := testSample(t)
	cfg := DefaultConfig()
	cfg.Mode = ModeExact
	cp := filepath.Join(t.TempDir(), "checkpoints")
	p, err := NewPipeline(s, cfg, cp, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RunStage(0); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" && entry.Name() != "manifest.json" {
			if err := os.WriteFile(filepath.Join(cp, entry.Name()), []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	p2, err := NewPipeline(s, cfg, cp, true)
	if err != nil {
		t.Fatal(err)
	}
	if p2.State().Progress.Committed != 0 || p2.State().ResumeStatus != "invalidated" {
		t.Fatalf("corrupt snapshot was not invalidated: %+v", p2.State())
	}
}

func mobiusStrip() mesh.Mesh {
	n := 5
	m := mesh.Mesh{}
	for i := 0; i < n; i++ {
		th := 6.283185307179586 * float64(i) / float64(n)
		m.Vertices = append(m.Vertices,
			mesh.Vec3{X: 1 + 0.2*math.Cos(th/2), Y: math.Sin(th), Z: 0.2 * math.Sin(th/2)},
			mesh.Vec3{X: 1 - 0.2*math.Cos(th/2), Y: math.Sin(th), Z: -0.2 * math.Sin(th/2)},
		)
	}
	next := func(i int) int { return (i + 1) % n }
	for i := 0; i < n; i++ {
		a, b := 2*i, 2*i+1
		an, bn := 2*next(i), 2*next(i)+1
		if i == n-1 {
			an, bn = 1, 0
		}
		m.Faces = append(m.Faces,
			mesh.Face{A: a, B: b, C: an},
			mesh.Face{A: b, B: bn, C: an},
		)
	}
	return m
}
