package geom_test

import (
	"math"
	"testing"

	"cadlab/internal/geom"
	"cadlab/internal/model"
	"cadlab/internal/sample"
)

func TestSubdivisionMatchesTruth(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	for _, p := range ds.Patches {
		for _, truth := range ds.Truth.Samples {
			if truth.PatchID != p.ID {
				continue
			}
			ev := geom.Evaluate(p, truth.U, truth.V)
			for axis, pair := range [][2]float64{
				{ev.Position.X, truth.Position.X},
				{ev.Position.Y, truth.Position.Y},
				{ev.Position.Z, truth.Position.Z},
			} {
				if math.Abs(pair[0]-pair[1]) > ds.Tolerance {
					t.Fatalf("patch %d axis %d position error %.3g > %.3g", p.ID, axis, math.Abs(pair[0]-pair[1]), ds.Tolerance)
				}
			}
			angle := math.Acos(math.Max(-1, math.Min(1, ev.Normal.Dot(truth.Normal)))) * 180 / math.Pi
			if angle > ds.AngleTol {
				t.Fatalf("patch %d u=%.3f v=%.3f normal error %.4f deg > %.4f eval=%+v truth=%+v", p.ID, truth.U, truth.V, angle, ds.AngleTol, ev.Normal, truth.Normal)
			}
			if math.Abs(ev.Curvature-truth.Curvature) > 1e-7 {
				t.Fatalf("patch %d curvature error %.6g", p.ID, math.Abs(ev.Curvature-truth.Curvature))
			}
		}
		box := model.EmptyBBox()
		for _, tr := range ds.Truth.Samples {
			if tr.PatchID == p.ID {
				box.Add(tr.Position)
			}
		}
		for _, bt := range ds.Truth.Bounds {
			if bt.PatchID == p.ID {
				if box.Min.X < bt.Box.Min.X-ds.Tolerance || box.Max.X > bt.Box.Max.X+ds.Tolerance {
					t.Fatalf("patch %d truth sample outside declared bbox X", p.ID)
				}
				mesh, d := geom.Tessellate(p, geom.TessOptions{Density: 40, CurvatureScale: 1, NormalTolDegrees: 1})
				if d != nil {
					t.Fatal(d)
				}
				meshBox := geom.Bounds(mesh)
				const bboxTol = 0.02
				if meshBox.Min.X < bt.Box.Min.X-bboxTol || meshBox.Max.X > bt.Box.Max.X+bboxTol ||
					meshBox.Min.Y < bt.Box.Min.Y-bboxTol || meshBox.Max.Y > bt.Box.Max.Y+bboxTol ||
					meshBox.Min.Z < bt.Box.Min.Z-bboxTol || meshBox.Max.Z > bt.Box.Max.Z+bboxTol {
					t.Fatalf("patch %d tessellated bbox %+v exceeds truth bbox %+v", p.ID, meshBox, bt.Box)
				}
			}
		}
		for _, tc := range ds.Truth.TrimChecks {
			if tc.PatchID == p.ID {
				got := geom.PointInTrim(tc.Point, p.Trim)
				if got != tc.Inside {
					t.Fatalf("patch %d trim truth mismatch at %+v: got %v want %v", p.ID, tc.Point, got, tc.Inside)
				}
			}
		}
	}
}

func TestDensityCurvatureAndMonotonicity(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	p := ds.Patches[1]
	prevFaces := 0
	for _, density := range []int{4, 8, 16, 32} {
		m, d := geom.Tessellate(p, geom.TessOptions{Density: density, CurvatureScale: 1.5, NormalTolDegrees: 0.75})
		if d != nil {
			t.Fatal(d)
		}
		if m.TriangleCount < prevFaces {
			t.Fatalf("triangle count decreased: density=%d faces=%d prev=%d", density, m.TriangleCount, prevFaces)
		}
		if m.DensityU < density {
			t.Fatalf("curvature-linked density tightened below base: base=%d effective=%d", density, m.DensityU)
		}
		prevFaces = m.TriangleCount
	}
	low, _ := geom.Tessellate(p, geom.TessOptions{Density: 10, CurvatureScale: 0, NormalTolDegrees: 0.75})
	high, _ := geom.Tessellate(p, geom.TessOptions{Density: 10, CurvatureScale: 3, NormalTolDegrees: 0.75})
	if high.TriangleCount < low.TriangleCount || high.DensityU < low.DensityU {
		t.Fatalf("curvature did not increase refinement: low=%d high=%d", low.TriangleCount, high.TriangleCount)
	}
}

func TestTessellationParameterDiagnostics(t *testing.T) {
	p := sample.Generate(sample.DefaultSeed).Patches[0]
	_, d := geom.Tessellate(p, geom.TessOptions{Density: 1, CurvatureScale: 1, NormalTolDegrees: 1})
	if d == nil || d.Code != model.CodePara || d.Field != "density" {
		t.Fatalf("expected E_PARA density, got %v", d)
	}
	_, d = geom.Tessellate(p, geom.TessOptions{Density: 8, CurvatureScale: 99, NormalTolDegrees: 1})
	if d == nil || d.Code != model.CodePara || d.Field != "curvatureScale" {
		t.Fatalf("expected E_PARA curvatureScale, got %v", d)
	}
}
