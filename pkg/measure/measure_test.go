package measure_test

import (
	"math"
	"testing"

	"volumelab/pkg/measure"
	"volumelab/pkg/model"
	"volumelab/pkg/samplegen"
	"volumelab/pkg/volume"
)

func testVolume(t *testing.T) *volume.Volume {
	t.Helper()
	result, err := samplegen.Generate(samplegen.Options{OutputDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	_, v, err := volume.LoadFromManifest(result.ManifestPath, "vol_a")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAnnotationDualReadingsAndLengths(t *testing.T) {
	v := testVolume(t)
	annotation, appErr := measure.NewAnnotation(v, model.AnnotationRequest{
		Plane:            model.PlaneAxial,
		A:                model.Vec3{0, 0, 0},
		B:                model.Vec3{3, 4, 5},
		CoordinateSystem: "world",
	}, "ann-0001", 1)
	if appErr != nil {
		t.Fatal(appErr)
	}
	wantWorld := math.Sqrt(9 + 16 + 25)
	wantVoxel := math.Sqrt(9 + math.Pow(4/1.2, 2) + math.Pow(5/1.5, 2))
	if math.Abs(annotation.WorldLengthMM-wantWorld) > 1e-12 {
		t.Fatalf("world length %f, want %f", annotation.WorldLengthMM, wantWorld)
	}
	if math.Abs(annotation.VoxelLength-wantVoxel) > 1e-12 {
		t.Fatalf("voxel length %f, want %f", annotation.VoxelLength, wantVoxel)
	}
	if annotation.EndpointB.Voxel != (model.Vec3{3, 4 / 1.2, 5 / 1.5}) {
		t.Fatalf("unexpected voxel reading: %v", annotation.EndpointB.Voxel)
	}
	if err := measure.ValidateStoredAnnotation(v, *annotation); err != nil {
		t.Fatal(err)
	}
}

func TestAnnotationRangeErrorNamesEndpointAndAxis(t *testing.T) {
	v := testVolume(t)
	_, appErr := measure.NewAnnotation(v, model.AnnotationRequest{
		Plane:            model.PlaneAxial,
		A:                model.Vec3{10, 10, 10},
		B:                model.Vec3{-1, 10, 10},
		CoordinateSystem: "world",
	}, "ann", 1)
	if appErr == nil || appErr.Code != "E_RANGE" || appErr.Endpoint != 2 || appErr.Axis != "X" {
		t.Fatalf("unexpected error: %#v", appErr)
	}
}

func TestAutoSpherePrecisionPasses(t *testing.T) {
	v := testVolume(t)
	annotation, appErr := measure.AutoAnnotation(v, "sphere_a", "sphere_diameter")
	if appErr != nil {
		t.Fatal(appErr)
	}
	annotation.Interpolation = 1
	report, appErr := measure.Compare(v, *annotation, "sphere_a", "sphere_diameter", v.ToleranceMM)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if !report.Pass || math.Abs(report.TruthLengthMM-16) > 1e-12 || report.DeviationMM > 1e-12 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.UpperBoundMM <= 0 || len(report.Inputs) != 3 {
		t.Fatalf("upper bound was not derived: %+v", report)
	}
}

func TestPrecisionFailureReportsDeviationAndBound(t *testing.T) {
	v := testVolume(t)
	annotation, appErr := measure.AutoAnnotation(v, "sphere_a", "sphere_diameter")
	if appErr != nil {
		t.Fatal(appErr)
	}
	annotation.Interpolation = 1
	annotation.EndpointB.World[0] -= 2
	annotation.EndpointB.Voxel[0] -= 2
	annotation.WorldLengthMM -= 2
	report, appErr := measure.Compare(v, *annotation, "sphere_a", "sphere_diameter", 0.5)
	if appErr == nil || appErr.Code != "E_PREC" {
		t.Fatalf("expected E_PREC, got %#v", appErr)
	}
	if report == nil || report.DeviationMM == 0 || report.UpperBoundMM <= 0 {
		t.Fatalf("missing precision diagnostics: %+v", report)
	}
}

func TestUpperBoundDoesNotUseAnalyticTruth(t *testing.T) {
	v := testVolume(t)
	annotation, appErr := measure.AutoAnnotation(v, "sphere_a", "sphere_diameter")
	if appErr != nil {
		t.Fatal(appErr)
	}
	annotation.Interpolation = 1
	boundA, _, _ := measure.UpperBound(v, *annotation)
	changed := *v
	changed.Structures = append([]model.Structure(nil), v.Structures...)
	for i := range changed.Structures {
		if changed.Structures[i].ID == "sphere_a" {
			radius := *changed.Structures[i].Radius * 100
			changed.Structures[i].Radius = &radius
		}
	}
	boundB, _, _ := measure.UpperBound(&changed, *annotation)
	if boundA != boundB {
		t.Fatalf("upper bound changed when truth changed: %f -> %f", boundA, boundB)
	}
}
