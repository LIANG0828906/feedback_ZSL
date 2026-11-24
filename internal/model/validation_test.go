package model_test

import (
	"testing"

	"cadlab/internal/model"
	"cadlab/internal/sample"
)

func TestSampleDatasetValidates(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	if d := model.ValidateDataset(ds); d != nil {
		t.Fatalf("generated dataset invalid: %v", d)
	}
	if len(ds.Patches) != 3 || len(ds.Meshes) < 2 || len(ds.Truth.Samples) == 0 {
		t.Fatalf("sample manifest incomplete: patches=%d meshes=%d samples=%d", len(ds.Patches), len(ds.Meshes), len(ds.Truth.Samples))
	}
}

func TestDiagnosticCodesAndLocations(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	base := ds.Patches[0]

	t.Run("schema", func(t *testing.T) {
		p := base
		p.Name = ""
		d := model.ValidatePatch(p)
		assertDiag(t, d, model.CodeSchema, base.ID, "name")
	})
	t.Run("weight", func(t *testing.T) {
		p := base
		p.Control = append([]model.ControlPoint(nil), base.Control...)
		p.Control[2].Weight = 0
		d := model.ValidatePatch(p)
		assertDiag(t, d, model.CodeWeight, base.ID, "weight")
	})
	t.Run("clip", func(t *testing.T) {
		p := base
		p.Trim = append([]model.Vec2(nil), base.Trim...)
		p.Trim[len(p.Trim)-1] = model.Vec2{U: 0.5, V: 0.5}
		d := model.ValidatePatch(p)
		assertDiag(t, d, model.CodeClip, base.ID, "trimBoundary")
	})
	t.Run("knot", func(t *testing.T) {
		p := base
		p.UKnots = append([]float64(nil), base.UKnots...)
		p.UKnots[4] = -1
		d := model.ValidatePatch(p)
		assertDiag(t, d, model.CodeKnot, base.ID, "uKnots")
	})
	t.Run("missingControl", func(t *testing.T) {
		p := base
		p.Control = append([]model.ControlPoint(nil), base.Control[:len(base.Control)-1]...)
		d := model.ValidatePatch(p)
		assertDiag(t, d, model.CodeGeom, base.ID, "controlPoints")
	})
	t.Run("meshIndex", func(t *testing.T) {
		m := ds.Meshes[0]
		m.Indices = append([]int(nil), m.Indices...)
		m.Indices[0] = 99999
		d := model.ValidateSubdivMesh(m)
		if d == nil || d.Code != model.CodeSchema || d.Field == "" {
			t.Fatalf("expected mesh index schema diagnostic, got %v", d)
		}
	})
}

func assertDiag(t *testing.T, d *model.Diagnostic, code string, patchID int, fieldFragment string) {
	t.Helper()
	if d == nil {
		t.Fatalf("expected %s diagnostic, got nil", code)
	}
	if d.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, d.Code, d)
	}
	if d.PatchID != patchID {
		t.Fatalf("expected patch %d, got %d", patchID, d.PatchID)
	}
	if fieldFragment != "" && !contains(d.Field, fieldFragment) {
		t.Fatalf("field %q does not contain %q", d.Field, fieldFragment)
	}
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
