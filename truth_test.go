package ortho

import (
	"testing"
)

func TestGeometryCheckPass(t *testing.T) {
	params := DefaultStageParams()
	stages := BuildStages(params)
	truth := genTruth(SampleSeed+2, stages, params)
	reports, err := CheckGeometry(stages, truth, params.Tolerance)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 3 {
		t.Fatalf("reports = %d, want 3", len(reports))
	}
	for _, r := range reports {
		if r.Count == 0 || r.Max > params.Tolerance {
			t.Fatalf("stage %s: max=%f count=%d", r.Stage, r.Max, r.Count)
		}
	}
	// 真值须携带畸变中心与半径。
	if truth.DistortionRadius != params.Radial.Radius {
		t.Fatal("truth distortion radius mismatch")
	}
	if truth.DistortionCenter[0] != params.Radial.Cx {
		t.Fatal("truth distortion center mismatch")
	}
}

func TestGeometryCheckTamperedParam(t *testing.T) {
	params := DefaultStageParams()
	truth := genTruth(SampleSeed+2, BuildStages(params), params)
	tampered := *params
	tampered.Radial.K1 += 0.1
	_, err := CheckGeometry(BuildStages(&tampered), truth, params.Tolerance)
	if err == nil || err.Code != EGeom {
		t.Fatalf("expected E_GEOM, got %v", err)
	}
	if err.Stage != "radial" {
		t.Fatalf("E_GEOM stage = %s, want radial", err.Stage)
	}
	if len(err.Outliers) == 0 {
		t.Fatal("E_GEOM must list outlier control point ids")
	}
	for _, o := range err.Outliers {
		if o.Measured <= o.Tolerance {
			t.Fatalf("outlier #%d measured %f <= tolerance %f", o.PointID, o.Measured, o.Tolerance)
		}
	}
}

func TestSampleGenerationDeterministic(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	if _, err := GenerateSamples(dir1); err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateSamples(dir2); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"terrain_gray.png", "terrain_rgb.png", "stages.json", "truth.json"} {
		b1, err := readFile(joinPath(dir1, name))
		if err != nil {
			t.Fatal(err)
		}
		b2, err := readFile(joinPath(dir2, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(b1) != len(b2) {
			t.Fatalf("%s: size differs between runs", name)
		}
		for i := range b1 {
			if b1[i] != b2[i] {
				t.Fatalf("%s: byte %d differs between runs", name, i)
			}
		}
	}
}
