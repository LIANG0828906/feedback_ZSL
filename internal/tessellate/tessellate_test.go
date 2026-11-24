package tessellate_test

import (
	"math"
	"testing"

	"cadpipe/internal/cad"
	"cadpipe/internal/sample"
	"cadpipe/internal/tessellate"
)

func TestTessellationMatchesAnalyticTruth(t *testing.T) {
	bundle, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, truth := range bundle.Truth.SurfacePoints {
		patch := bundle.Scene.Patch(truth.PatchID)
		got := tessellate.Evaluate(patch, truth.U, truth.V)
		want := cad.Vec3{X: truth.X, Y: truth.Y, Z: truth.Z}
		if delta := maxAxisDelta(got, want); delta > 1e-9 {
			t.Fatalf("analytic mismatch %s u=%v v=%v got=%+v want=%+v delta=%v",
				truth.PatchID, truth.U, truth.V, got, want, delta)
		}
	}

	patch := bundle.Scene.Patches[0]
	mesh, err := tessellate.Tessellate(patch, &cad.Subdivision{
		Density: 8, CurvatureSensitivity: 0, MaxDensity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mesh.Vertices) != 81 || len(mesh.Triangles) != 128 {
		t.Fatalf("8x8 mesh = %d vertices %d triangles", len(mesh.Vertices), len(mesh.Triangles))
	}
	for _, v := range mesh.Vertices {
		if math.Abs(v.Normal.X) > 1e-6 && math.Abs(v.Normal.Y) > 1e-6 && math.Abs(v.Normal.Z) > 0.99 {
			continue
		}
		if math.Abs(math.Hypot(v.Normal.X, math.Hypot(v.Normal.Y, v.Normal.Z))-1) > 1e-9 {
			t.Fatalf("normal not normalized: %+v", v.Normal)
		}
	}
}

func TestDensityAndToleranceMonotonic(t *testing.T) {
	bundle, _ := sample.Generate(sample.DefaultSeed)
	patch := bundle.Scene.Patch("surface-beta")

	var faces []int
	for _, density := range []int{1, 2, 4, 8, 16} {
		mesh, err := tessellate.Tessellate(patch, &cad.Subdivision{
			Density: density, CurvatureSensitivity: 0, MaxDensity: 128,
		})
		if err != nil {
			t.Fatal(err)
		}
		faces = append(faces, len(mesh.Triangles))
	}
	for i := 1; i < len(faces); i++ {
		if faces[i] < faces[i-1] {
			t.Fatalf("face counts not monotonic: %v", faces)
		}
	}

	tolerances := []float64{3e-3, 1e-3, 3e-4, 1e-4, 3e-5}
	var errs []float64
	var lastFaces int
	for _, tol := range tolerances {
		mesh, err := tessellate.Tessellate(patch, &cad.Subdivision{
			Density: 2, CurvatureSensitivity: 0, MaxDensity: 128, Tolerance: tol,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(mesh.Triangles) < lastFaces {
			t.Fatalf("tightening tolerance reduced faces: %d < %d", len(mesh.Triangles), lastFaces)
		}
		lastFaces = len(mesh.Triangles)
		errs = append(errs, meshApproxError(patch, mesh))
	}
	for i := 1; i < len(errs); i++ {
		if errs[i] > errs[i-1]+1e-12 {
			t.Fatalf("approximation error increased under tighter tolerance: %v", errs)
		}
	}
}

func TestInvalidParametersAndKnots(t *testing.T) {
	bundle, _ := sample.Generate(sample.DefaultSeed)
	patch := bundle.Scene.Patches[0]
	for _, density := range []int{0, 999} {
		_, err := tessellate.Tessellate(patch, &cad.Subdivision{Density: density, MaxDensity: 1024})
		ce, ok := cad.AsError(err)
		if !ok || ce.Code != cad.CodeParam {
			t.Fatalf("density %d error = %v", density, err)
		}
	}
	bad := *patch
	bad.KnotsU = append([]float64(nil), patch.KnotsU...)
	bad.KnotsU[2], bad.KnotsU[3] = 0.8, 0.2
	_, err := tessellate.Tessellate(&bad, nil)
	ce, ok := cad.AsError(err)
	if !ok || ce.Code != cad.CodeKnot || ce.PatchID == "" {
		t.Fatalf("knot error = %v", err)
	}
}

func TestCurvatureSensitivityAddsSegments(t *testing.T) {
	bundle, _ := sample.Generate(sample.DefaultSeed)
	flat := *bundle.Scene.Patch("surface-alpha")
	for u := range flat.ControlPoints {
		for v := range flat.ControlPoints[u] {
			flat.ControlPoints[u][v].Z = 0
		}
	}
	curved := *bundle.Scene.Patch("surface-alpha")
	curved.ControlPoints = append([][]cad.Point3(nil), bundle.Scene.Patch("surface-alpha").ControlPoints...)
	curved.ControlPoints[1] = append([]cad.Point3(nil), bundle.Scene.Patch("surface-alpha").ControlPoints[1]...)
	curved.ControlPoints[1][1].Z = cad.Millimetre(400)

	params := cad.Subdivision{Density: 1, CurvatureSensitivity: 20, MaxDensity: 128}
	flatMesh, err := tessellate.Tessellate(&flat, &params)
	if err != nil {
		t.Fatal(err)
	}
	curvedMesh, err := tessellate.Tessellate(&curved, &params)
	if err != nil {
		t.Fatal(err)
	}
	if curvedMesh.SegmentsU <= flatMesh.SegmentsU {
		t.Fatalf("curvature did not increase segments: curved=%d flat=%d", curvedMesh.SegmentsU, flatMesh.SegmentsU)
	}
}

func meshApproxError(patch *cad.Patch, mesh *tessellate.Mesh) float64 {
	var maxErr float64
	// Dense vertex truth is already exact; measure triangle chord deviation at
	// cell centres by comparing the analytic point with the planar diagonal.
	for iu := 0; iu < mesh.SegmentsU; iu++ {
		for iv := 0; iv < mesh.SegmentsV; iv++ {
			u := (float64(iu) + 0.5) / float64(mesh.SegmentsU)
			v := (float64(iv) + 0.5) / float64(mesh.SegmentsV)
			analytic := tessellate.Evaluate(patch, u, v)
			// At the centre of a bilinear cell, chord height error is bounded by
			// distance to the average corner position.
			c00 := tessellate.Evaluate(patch, float64(iu)/float64(mesh.SegmentsU), float64(iv)/float64(mesh.SegmentsV))
			c10 := tessellate.Evaluate(patch, float64(iu+1)/float64(mesh.SegmentsU), float64(iv)/float64(mesh.SegmentsV))
			c01 := tessellate.Evaluate(patch, float64(iu)/float64(mesh.SegmentsU), float64(iv+1)/float64(mesh.SegmentsV))
			c11 := tessellate.Evaluate(patch, float64(iu+1)/float64(mesh.SegmentsU), float64(iv+1)/float64(mesh.SegmentsV))
			center := cad.Vec3{
				X: (c00.X + c10.X + c01.X + c11.X) / 4,
				Y: (c00.Y + c10.Y + c01.Y + c11.Y) / 4,
				Z: (c00.Z + c10.Z + c01.Z + c11.Z) / 4,
			}
			maxErr = math.Max(maxErr, maxAxisDelta(analytic, center))
		}
	}
	return maxErr
}

func maxAxisDelta(a, b cad.Vec3) float64 {
	return math.Max(math.Abs(a.X-b.X), math.Max(math.Abs(a.Y-b.Y), math.Abs(a.Z-b.Z)))
}
