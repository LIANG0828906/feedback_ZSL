package render_test

import (
	"context"
	"math"
	"reflect"
	"testing"

	"volumelab/pkg/model"
	"volumelab/pkg/pool"
	"volumelab/pkg/render"
	"volumelab/pkg/samplegen"
	"volumelab/pkg/volume"
)

func testVolume(t *testing.T) *volume.Volume {
	t.Helper()
	dir := t.TempDir()
	result, err := samplegen.Generate(samplegen.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	_, v, err := volume.LoadFromManifest(result.ManifestPath, "vol_a")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func baseRequest() render.Request {
	return render.Request{
		Crosshair: model.Vec3{22, 24, 24},
		Window:    model.Window{Width: 1200, Level: 300},
		ThicknessMM: map[string]float64{
			"axial":    3,
			"coronal":  2.4,
			"sagittal": 3,
		},
		InterpolationOrder: 1,
	}
}

func TestRenderTripleSerialParallelIdentical(t *testing.T) {
	v := testVolume(t)
	request := baseRequest()
	serial, appErr := render.RenderTriple(context.Background(), v, request, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	executor, err := pool.New(3, 37)
	if err != nil {
		t.Fatal(err)
	}
	parallel, appErr := render.RenderTriple(context.Background(), v, request, executor)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if len(serial.Images) != 3 || len(parallel.Images) != 3 {
		t.Fatalf("unexpected image count: %d %d", len(serial.Images), len(parallel.Images))
	}
	for i := range serial.Images {
		if serial.Images[i].Plane != parallel.Images[i].Plane {
			t.Fatalf("plane order diverged at %d", i)
		}
		if !reflect.DeepEqual(serial.Images[i].Pixels, parallel.Images[i].Pixels) {
			t.Fatalf("%s pixels differ between serial and parallel", serial.Images[i].Plane)
		}
		if serial.Images[i].PixelHash != parallel.Images[i].PixelHash {
			t.Fatalf("%s hashes differ", serial.Images[i].Plane)
		}
	}
	if executor.Stats().PeakWorkers > 3 {
		t.Fatalf("peak workers exceeded cap: %+v", executor.Stats())
	}
}

func TestRenderRepeatIsByteIdentical(t *testing.T) {
	v := testVolume(t)
	request := baseRequest()
	a, appErr := render.RenderTriple(context.Background(), v, request, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	b, appErr := render.RenderTriple(context.Background(), v, request, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	for i := range a.Images {
		if !reflect.DeepEqual(a.Images[i].Pixels, b.Images[i].Pixels) {
			t.Fatalf("%s is not repeatable", a.Images[i].Plane)
		}
	}
}

func TestThicknessWeightsAreNormalized(t *testing.T) {
	v := testVolume(t)
	indices, weights := render.EquivalentLayers(24, 7.5, v.Spacing[2], v.Shape[2])
	if len(indices) == 0 || len(indices) != len(weights) {
		t.Fatalf("bad weights: %v %v", indices, weights)
	}
	sum := 0.0
	for _, weight := range weights {
		sum += weight
	}
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("weights sum to %f, want 1", sum)
	}
	for i := 1; i < len(indices); i++ {
		if indices[i] != indices[i-1]+1 {
			t.Fatalf("layer indices are not adjacent: %v", indices)
		}
	}
}

func TestWindowAndThicknessValidation(t *testing.T) {
	v := testVolume(t)
	request := baseRequest()
	request.Window.Width = 0
	_, appErr := render.RenderTriple(context.Background(), v, request, nil)
	if appErr == nil || appErr.Code != "E_WIN" || appErr.Field != "width" {
		t.Fatalf("expected E_WIN width, got %#v", appErr)
	}
	request = baseRequest()
	request.Window.Level = 99999
	_, appErr = render.RenderTriple(context.Background(), v, request, nil)
	if appErr == nil || appErr.Code != "E_WIN" || appErr.Field != "level" || appErr.Allowed == nil {
		t.Fatalf("expected E_WIN level with allowed range, got %#v", appErr)
	}
	request = baseRequest()
	request.ThicknessMM["axial"] = 1000
	_, appErr = render.RenderTriple(context.Background(), v, request, nil)
	if appErr == nil || appErr.Code != "E_THICK" || appErr.Allowed == nil {
		t.Fatalf("expected E_THICK with allowed range, got %#v", appErr)
	}
}

func TestCrosshairPixelUsesSharedWorldPoint(t *testing.T) {
	v := testVolume(t)
	request := baseRequest()
	request.Crosshair = model.Vec3{20, 30, 27}
	result, appErr := render.RenderTriple(context.Background(), v, request, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	want := map[model.Plane][2]int{
		model.PlaneAxial:    {20, 25},
		model.PlaneCoronal:  {20, 18},
		model.PlaneSagittal: {25, 18},
	}
	for _, image := range result.Images {
		if image.CrosshairPixel != want[image.Plane] {
			t.Fatalf("%s crosshair %v, want %v", image.Plane, image.CrosshairPixel, want[image.Plane])
		}
	}
}
