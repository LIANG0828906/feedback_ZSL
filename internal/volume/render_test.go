package volume_test

import (
	"context"
	"testing"

	"volumediag/internal/diag"
	"volumediag/internal/scene"
	"volumediag/internal/volume"
)

func TestParameterValidationIncludesSnapshot(t *testing.T) {
	scene := scene.Build(scene.DefaultGenerateOptions())
	p := volume.DefaultParams()
	p.Step = -1
	err := p.Validate(scene)
	if diag.CodeOf(err) != diag.EPara {
		t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
	}
	de := diag.As(err)
	if de == nil || de.Field != "params.step" || len(de.Snapshot) == 0 {
		t.Fatalf("missing parameter snapshot: %#v", de)
	}
}

func TestBlockDeterministicAcrossSchedulingIndependentInputs(t *testing.T) {
	scene := scene.Build(scene.DefaultGenerateOptions())
	p := volume.DefaultParams()
	p.Step = 0.12
	p.Jitter = volume.JitterHalton
	p.JitterSamples = 2
	a, err := volume.RenderBlock(context.Background(), scene, 0, p, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := volume.RenderBlock(context.Background(), scene, 0, p, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("identical block render differs: %s != %s", a.Digest, b.Digest)
	}
}

func TestPreviewErrorBoundUsesObservedParameters(t *testing.T) {
	scene := scene.Build(scene.DefaultGenerateOptions())
	p := volume.PreviewParams()
	q := volume.ErrorBound(scene, &scene.CameraPath[0], p)
	if q.ActualStep <= 0 || q.ActualStep > p.Step || q.JitterSamples != p.JitterSamples || q.LightCount != len(scene.Lights) ||
		q.SigmaTMin <= 0 || q.ErrorBoundPerPixel <= 0 || q.Formula == "" {
		t.Fatalf("incomplete quality report: %+v", q)
	}
}
