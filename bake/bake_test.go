package bake_test

import (
	"encoding/json"
	"math"
	"testing"

	"terrain-bake/bake"
	"terrain-bake/internal/testutil"
	"terrain-bake/project"
)

func loadProject(t *testing.T) (*project.Project, string) {
	t.Helper()
	dir := testutil.CopyProject(t)
	p, e := project.Load(dir)
	if e != nil {
		t.Fatalf("load project: %v", e)
	}
	return p, dir
}

func bakeAll(t *testing.T, p *project.Project) map[string]*bake.Artifact {
	t.Helper()
	params := bake.ParamsFromManifest(p.Manifest)
	out := make(map[string]*bake.Artifact, len(p.BlockOrder))
	for _, id := range p.BlockOrder {
		a, e := bake.BakeBlock(p, id, params)
		if e != nil {
			t.Fatalf("bake %s: %v", id, e)
		}
		out[id] = a
	}
	return out
}

func TestBakeDeterministicAndTruth(t *testing.T) {
	p, _ := loadProject(t)
	params := bake.ParamsFromManifest(p.Manifest)
	first, e := bake.BakeBlock(p, "B_0_0", params)
	if e != nil {
		t.Fatal(e)
	}
	second, e := bake.BakeBlock(p, "B_0_0", params)
	if e != nil {
		t.Fatal(e)
	}
	if string(first.CanonicalBytes()) != string(second.CanonicalBytes()) {
		t.Fatal("same block bake is not byte-identical")
	}

	artifacts := bakeAll(t, p)
	if string(artifacts["B_0_0"].CanonicalBytes()) != string(first.CanonicalBytes()) {
		t.Fatal("bake result changed when invoked after other blocks")
	}
	truth := bake.ValidateTruth(p, artifacts)
	if !truth.OK {
		t.Fatalf("truth mismatches: %+v", truth.Mismatches)
	}
	if truth.Checks != 53 {
		t.Fatalf("expected 53 truth checks, got %d", truth.Checks)
	}
	for id, a := range artifacts {
		if len(a.Positions) != 1457 {
			t.Fatalf("%s vertex count = %d", id, len(a.Positions))
		}
		if len(a.Normals) != len(a.Positions) {
			t.Fatalf("%s normal buffer length mismatch", id)
		}
		if len(a.MaterialWeights) != len(a.Positions)*p.Layers() {
			t.Fatalf("%s material buffer length mismatch", id)
		}
		if a.Seams.ConstraintViolations != 0 {
			t.Fatalf("%s has %d seam violations", id, a.Seams.ConstraintViolations)
		}
		if a.Seams.MaxNormalAngleDeg > params.SeamAngleThresholdDeg {
			t.Fatalf("%s seam angle %.9g exceeds %.9g", id, a.Seams.MaxNormalAngleDeg, params.SeamAngleThresholdDeg)
		}
		if a.Seams.MaxMaterialDiff != 0 {
			t.Fatalf("%s seam material delta = %d", id, a.Seams.MaxMaterialDiff)
		}
		sum := 0.0
		for _, r := range a.MaterialRatios {
			sum += r
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Fatalf("%s material ratios sum to %.12f", id, sum)
		}
		if a.Bounds.Min.X > a.Bounds.Max.X || a.Bounds.Min.Y > a.Bounds.Max.Y || a.Bounds.Min.Z > a.Bounds.Max.Z {
			t.Fatalf("%s bounds are inverted", id)
		}
	}
}

func TestSeamFailureReportsBothBlocksAndAngle(t *testing.T) {
	p, _ := loadProject(t)
	for row := 0; row < p.Blocks["B_0_0"].Def.Grid.Rows; row++ {
		value := 3.0
		if row%2 == 0 {
			value = -3.0
		}
		if e := p.ApplyHeightEdit("B_0_0", p.Blocks["B_0_0"].Def.Grid.Cols-1, row, value); e != nil {
			t.Fatal(e)
		}
	}
	params := bake.ParamsFromManifest(p.Manifest)
	params.SeamMaxIterations = 0
	_, e := bake.BakeBlock(p, "B_0_0", params)
	if e == nil {
		t.Fatal("expected E_SEAM")
	}
	if e.Code != project.CodeSeam {
		t.Fatalf("expected E_SEAM, got %s: %v", e.Code, e)
	}
	if e.Details["a"] != "B_0_0" || e.Details["b"] != "B_1_0" {
		t.Fatalf("seam error does not identify both blocks: %+v", e.Details)
	}
	angle, _ := e.Details["angleDeg"].(float64)
	threshold, _ := e.Details["thresholdDeg"].(float64)
	if angle <= threshold {
		t.Fatalf("seam was reported but angle %.9g <= threshold %.9g", angle, threshold)
	}
}

func TestPerThreadVertexLimit(t *testing.T) {
	p, _ := loadProject(t)
	params := bake.ParamsFromManifest(p.Manifest)
	params.MaxVerticesPerThread = 100
	_, e := bake.BakeBlock(p, "B_0_0", params)
	if e == nil || e.Code != project.CodeResource {
		t.Fatalf("expected E_RESOURCE, got %v", e)
	}
}

func TestMaterialEditKeepsSeamBoundaryContinuous(t *testing.T) {
	p, _ := loadProject(t)
	bd := p.Blocks["B_0_0"]
	if e := p.ApplyMaterialEdit("B_0_0", bd.Def.Grid.Cols-1, 8, 0, 9000); e != nil {
		t.Fatal(e)
	}
	params := bake.ParamsFromManifest(p.Manifest)
	a, e := bake.BakeBlock(p, "B_0_0", params)
	if e != nil {
		t.Fatal(e)
	}
	b, e := bake.BakeBlock(p, "B_1_0", params)
	if e != nil {
		t.Fatal(e)
	}
	for _, stat := range a.Seams.PerSeam {
		if stat.SeamID == "S_01" {
			if stat.MaxMaterialDiff != 0 {
				t.Fatalf("material boundary diff = %d", stat.MaxMaterialDiff)
			}
			if stat.MaxAngleDeg > params.SeamAngleThresholdDeg {
				t.Fatalf("seam angle %.9g exceeds threshold", stat.MaxAngleDeg)
			}
			return
		}
	}
	t.Fatal("seam S_01 not found in artifact")
	_ = b
}

func TestCompareArtifactsLocatesFirstProductType(t *testing.T) {
	p, _ := loadProject(t)
	params := bake.ParamsFromManifest(p.Manifest)
	a, e := bake.BakeBlock(p, "B_0_0", params)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(a)
	var altered bake.Artifact
	if err := json.Unmarshal(data, &altered); err != nil {
		t.Fatal(err)
	}
	altered.Normals[10].X += 0.125
	report, e := bake.CompareArtifacts(
		map[string]*bake.Artifact{"B_0_0": a},
		map[string]*bake.Artifact{"B_0_0": &altered},
		[]string{"B_0_0"},
	)
	if e == nil || e.Code != project.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", e)
	}
	if report.FirstBlock != "B_0_0" || report.ArtifactType != "normals" {
		t.Fatalf("wrong diff location: %+v", report)
	}
	if e.Field != "artifactType:normals" {
		t.Fatalf("diff error field = %s", e.Field)
	}
}
