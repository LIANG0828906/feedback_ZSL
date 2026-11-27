package terrainbake

import (
	"context"
	"testing"
)

func bakeForSeamTests(t *testing.T) (*Project, *BakeResult) {
	t.Helper()
	p := testProject(t)
	e, err := NewEngine(p, DefaultEngineConfig(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p, r
}

func TestSeamsAreAlignedAndMatchTruth(t *testing.T) {
	p, r := bakeForSeamTests(t)
	if len(r.Seams) != len(p.Seams) {
		t.Fatalf("seams = %d want %d", len(r.Seams), len(p.Seams))
	}
	for _, s := range r.Seams {
		if !s.Aligned || s.CountA != s.CountB {
			t.Fatalf("%s not point-aligned: %d/%d", s.ID, s.CountA, s.CountB)
		}
		if !s.TruthAligned {
			t.Fatalf("%s disagrees with truth profile", s.ID)
		}
		if s.Attribution == "E_ATTR" || s.Attribution == "alignment_mismatch" {
			t.Fatalf("%s unexpected attribution %s", s.ID, s.Attribution)
		}
	}
}

func TestSeamAlignmentMismatchAttribution(t *testing.T) {
	p, r := bakeForSeamTests(t)
	seam := p.Seams[0]
	sub := p.Clone()
	sub.Seams = []SeamSpec{seam}
	meshes := cloneMeshes(r.Meshes)
	b := meshes[seam.B]
	// Remove one boundary vertex from one side. Triangle references are not
	// relevant to the boundary sequence analyzer.
	for i, v := range b.Vertices {
		onBoundary := (seam.Axis == AxisVertical && v.X == seam.Coordinate) ||
			(seam.Axis == AxisHorizontal && v.Y == seam.Coordinate)
		if onBoundary {
			b.Vertices = append(b.Vertices[:i], b.Vertices[i+1:]...)
			break
		}
	}
	reports, err := AnalyzeSeams(sub, meshes)
	if err != nil {
		t.Fatalf("alignment mismatch should be attributable, got %v", err)
	}
	if reports[0].Attribution != "alignment_mismatch" || reports[0].Aligned {
		t.Fatalf("report = %+v", reports[0])
	}
}

func TestSeamUndersamplingAttribution(t *testing.T) {
	p, r := bakeForSeamTests(t)
	seam := p.Seams[0]
	sub := p.Clone()
	seam.AngleLimitDeg = 0
	sub.Seams = []SeamSpec{seam}
	meshes := cloneMeshes(r.Meshes)
	meshes[seam.A].BoundaryErrorMaxUM = meshes[seam.A].ToleranceUM + 1000
	meshes[seam.A].Undersampled = true
	reports, err := AnalyzeSeams(sub, meshes)
	if err != nil {
		t.Fatalf("undersampling should be attributable, got %v", err)
	}
	if reports[0].Attribution != "undersampling" {
		t.Fatalf("attribution = %s report=%+v", reports[0].Attribution, reports[0])
	}
	if len(reports[0].Exceeded) == 0 {
		t.Fatal("expected exceeded positions")
	}
}

func TestSeamUnattributableReturnsEAttr(t *testing.T) {
	p, r := bakeForSeamTests(t)
	seam := p.Seams[0]
	sub := p.Clone()
	seam.AngleLimitDeg = 0
	sub.Seams = []SeamSpec{seam}
	meshes := cloneMeshes(r.Meshes)
	for _, id := range []string{seam.A, seam.B} {
		meshes[id].BoundaryErrorMaxUM = 0
		meshes[id].Undersampled = false
	}
	reports, err := AnalyzeSeams(sub, meshes)
	e := mustCode(t, err, CodeAttr)
	if e.Boundary != seam.ID || e.CountA == 0 || e.CountB == 0 {
		t.Fatalf("E_ATTR context = %+v", e)
	}
	if len(reports) != 1 || reports[0].Attribution != "E_ATTR" {
		t.Fatalf("report = %+v", reports)
	}
}

func TestGeneratedFractureTruthCanDriveAttribution(t *testing.T) {
	p, r := bakeForSeamTests(t)
	if len(p.Truth.Fractures) < 2 {
		t.Fatalf("expected several fracture truths, got %d", len(p.Truth.Fractures))
	}
	for _, truth := range p.Truth.Fractures {
		seam, ok := p.SeamByID(truth.SeamID)
		if !ok {
			t.Fatalf("truth references unknown seam %s", truth.SeamID)
		}
		sub := p.Clone()
		spec := *seam
		spec.AngleLimitDeg = truth.AngleLimit
		sub.Seams = []SeamSpec{spec}
		meshes := cloneMeshes(r.Meshes)
		switch truth.Expected {
		case "undersampling":
			meshes[seam.A].BoundaryErrorMaxUM = truth.ToleranceUM + 1000
			meshes[seam.A].Undersampled = true
		case "alignment_mismatch":
			b := meshes[seam.B]
			for i, v := range b.Vertices {
				onBoundary := (spec.Axis == AxisVertical && v.X == spec.Coordinate) ||
					(spec.Axis == AxisHorizontal && v.Y == spec.Coordinate)
				if onBoundary {
					b.Vertices = append(b.Vertices[:i], b.Vertices[i+1:]...)
					break
				}
			}
		}
		reports, err := AnalyzeSeams(sub, meshes)
		if err != nil {
			t.Fatalf("%s truth %s returned error %v", truth.ID, truth.Expected, err)
		}
		if reports[0].Attribution != truth.Expected {
			t.Fatalf("%s truth attribution = %s want %s", truth.ID, reports[0].Attribution, truth.Expected)
		}
	}
}
