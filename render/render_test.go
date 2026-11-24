package render_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"diffrender/errs"
	"diffrender/render"
	"diffrender/sample"
	"diffrender/scene"
)

func loadTask(t *testing.T, seed int64) (*sample.Task, *scene.Scene, *scene.Scene) {
	t.Helper()
	task, err := sample.GenerateTask(seed)
	if err != nil {
		t.Fatal(err)
	}
	truth, err := scene.Load(task.VersionFiles[scene.Version3], "truth-3.0.json")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := scene.Load(task.InitialVersions[scene.Version1], "initial-1.0.json")
	if err != nil {
		t.Fatal(err)
	}
	return task, truth, initial
}

func TestForwardMatchesTargetExactly(t *testing.T) {
	task, truth, _ := loadTask(t, 201)
	images, err := render.Render(task.Truth, render.Config{MaxWorkers: 1, BlockPixels: 50, QueueCapacity: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != len(task.Targets) {
		t.Fatalf("images=%d targets=%d", len(images), len(task.Targets))
	}
	for i := range images {
		if images[i].CameraID != task.Targets[i].CameraID {
			t.Fatalf("camera %d id mismatch", i)
		}
		for p := range images[i].Pixels {
			if math.Float64bits(images[i].Pixels[p]) != math.Float64bits(task.Targets[i].Pixels[p]) {
				t.Fatalf("camera %d pixel %d differs: %v != %v", i, p, images[i].Pixels[p], task.Targets[i].Pixels[p])
			}
		}
	}
	reloadedImages, err := render.Render(truth, render.Config{MaxWorkers: 2, BlockPixels: 50, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := range reloadedImages {
		for p := range reloadedImages[i].Pixels {
			if math.Abs(reloadedImages[i].Pixels[p]-task.Targets[i].Pixels[p]) > 1e-12 {
				t.Fatalf("v3 round-trip changed image %d pixel %d: %g", i, p, reloadedImages[i].Pixels[p]-task.Targets[i].Pixels[p])
			}
		}
	}
}

func TestAnalyticGradientMatchesCentralDifference(t *testing.T) {
	task, _, initial := loadTask(t, 202)
	specs, err := render.ParameterSpecs(initial)
	if err != nil {
		t.Fatal(err)
	}
	cfg := render.Config{MaxWorkers: 1, BlockPixels: 96, QueueCapacity: 0}
	base, err := render.Compute(initial, task.Targets, cfg)
	if err != nil {
		t.Fatal(err)
	}
	values := render.ParameterValues(initial, specs)
	h := 1e-6
	var adVec, fdVec []float64
	for idx := 0; idx < 7; idx++ {
		plus := append([]float64(nil), values...)
		minus := append([]float64(nil), values...)
		plus[idx] += h
		minus[idx] -= h
		sp := initial.Clone()
		sm := initial.Clone()
		if err := render.ApplyParameterValues(sp, specs, plus); err != nil {
			t.Fatal(err)
		}
		if err := render.ApplyParameterValues(sm, specs, minus); err != nil {
			t.Fatal(err)
		}
		bp, err := render.Compute(sp, task.Targets, cfg)
		if err != nil {
			t.Fatal(err)
		}
		bm, err := render.Compute(sm, task.Targets, cfg)
		if err != nil {
			t.Fatal(err)
		}
		fd := (bp.Loss - bm.Loss) / (2 * h)
		ad := base.Grad[idx]
		if math.Abs(fd) > 1e-9 && math.Signbit(fd) != math.Signbit(ad) {
			t.Fatalf("%s direction differs: AD=%g FD=%g", specs[idx].ID, ad, fd)
		}
		if math.Abs(ad-fd) > 2e-6*math.Max(1, math.Abs(fd)) {
			t.Fatalf("%s AD=%g FD=%g", specs[idx].ID, ad, fd)
		}
		adVec = append(adVec, ad)
		fdVec = append(fdVec, fd)
	}
	var dot, adNorm, fdNorm float64
	for i := range adVec {
		dot += adVec[i] * fdVec[i]
		adNorm += adVec[i] * adVec[i]
		fdNorm += fdVec[i] * fdVec[i]
	}
	cosine := dot / math.Sqrt(adNorm*fdNorm)
	if cosine < 0.999999 {
		t.Fatalf("gradient direction cosine=%g", cosine)
	}
}

func TestSerialAndParallelAreBitIdentical(t *testing.T) {
	task, _, initial := loadTask(t, 203)
	serial := render.Config{MaxWorkers: 1, BlockPixels: 17, QueueCapacity: 0}
	parallel := render.Config{MaxWorkers: 4, BlockPixels: 17, QueueCapacity: 1}
	a, err := render.Compute(initial, task.Targets, serial)
	if err != nil {
		t.Fatal(err)
	}
	b, err := render.Compute(initial, task.Targets, parallel)
	if err != nil {
		t.Fatal(err)
	}
	if math.Float64bits(a.Loss) != math.Float64bits(b.Loss) {
		t.Fatalf("loss differs: %g != %g", a.Loss, b.Loss)
	}
	if len(a.Grad) != len(b.Grad) {
		t.Fatal("gradient length differs")
	}
	for i := range a.Grad {
		if math.Float64bits(a.Grad[i]) != math.Float64bits(b.Grad[i]) {
			t.Fatalf("gradient %d differs: %g != %g", i, a.Grad[i], b.Grad[i])
		}
	}
	for i := range a.Images {
		for p := range a.Images[i].Pixels {
			if math.Float64bits(a.Images[i].Pixels[p]) != math.Float64bits(b.Images[i].Pixels[p]) {
				t.Fatalf("image %d pixel %d differs", i, p)
			}
		}
	}
	if b.Metrics.PerWorkerBufferValues != 17*3 || b.Metrics.BlockPixels != 17 {
		t.Fatalf("unexpected resource metrics: %+v", b.Metrics)
	}
	if b.Metrics.BlockCount != 2*((28*28+16)/17) {
		t.Fatalf("unexpected block count: %+v", b.Metrics)
	}
}

func TestFailedBlockIsIsolatedAndRetryable(t *testing.T) {
	task, _, initial := loadTask(t, 204)
	faultCfg := render.Config{
		MaxWorkers: 2, BlockPixels: 64, QueueCapacity: 0, Retries: 0,
		Fault: func(view, start, end, attempt int) error {
			if view == 0 && start == 0 {
				return errorsNew("injected block failure")
			}
			return nil
		},
	}
	batch, err := render.Compute(initial, task.Targets, faultCfg)
	if err == nil {
		t.Fatal("expected initial block failure")
	}
	if batch == nil || batch.Blocks[0].Err == nil {
		t.Fatalf("expected failed block, got %+v", batch)
	}
	otherPixels := append([]float64(nil), batch.Blocks[1].Pixels...)
	otherGradient := append([]float64(nil), batch.Blocks[1].Gradient...)
	if len(otherPixels) == 0 || len(otherGradient) == 0 {
		t.Fatal("other block was not computed")
	}
	retryCfg := render.Config{MaxWorkers: 3, BlockPixels: 64, QueueCapacity: 0, Retries: 0}
	if err := batch.RetryFailed(retryCfg); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if batch.Blocks[0].Err != nil || batch.Blocks[0].Attempts != 2 {
		t.Fatalf("retried block=%+v", batch.Blocks[0])
	}
	if batch.Blocks[1].Attempts != 1 {
		t.Fatal("completed block was recomputed during retry")
	}
	for i := range otherPixels {
		if math.Float64bits(otherPixels[i]) != math.Float64bits(batch.Blocks[1].Pixels[i]) {
			t.Fatal("retry polluted another block's pixels")
		}
	}
	for i := range otherGradient {
		if math.Float64bits(otherGradient[i]) != math.Float64bits(batch.Blocks[1].Gradient[i]) {
			t.Fatal("retry polluted another block's gradient")
		}
	}
	clean, err := render.Compute(initial, task.Targets, retryCfg)
	if err != nil {
		t.Fatal(err)
	}
	if math.Float64bits(batch.Loss) != math.Float64bits(clean.Loss) {
		t.Fatalf("retried loss=%g clean=%g", batch.Loss, clean.Loss)
	}
	for i := range clean.Grad {
		if math.Float64bits(batch.Grad[i]) != math.Float64bits(clean.Grad[i]) {
			t.Fatalf("retried gradient %d differs", i)
		}
	}
}

func TestPanickingBlockBecomesRetryableFailure(t *testing.T) {
	task, _, initial := loadTask(t, 208)
	cfg := render.Config{
		MaxWorkers: 2, BlockPixels: 64, QueueCapacity: 0, Retries: 0,
		Fault: func(view, start, end, attempt int) error {
			if view == 0 && start == 0 {
				panic("injected renderer panic")
			}
			return nil
		},
	}
	batch, err := render.Compute(initial, task.Targets, cfg)
	if err == nil {
		t.Fatal("expected block failure")
	}
	if batch == nil || batch.Blocks[0].Err == nil {
		t.Fatal("panicking block was not marked failed")
	}
	if err := batch.RetryFailed(render.Config{MaxWorkers: 2, BlockPixels: 64, QueueCapacity: 0}); err != nil {
		t.Fatalf("retry panic block: %v", err)
	}
	if batch.Blocks[0].Err != nil || batch.Metrics.FailedBlockCount != 0 {
		t.Fatal("panicked block did not recover on retry")
	}
}

func TestHardConcurrencyCaps(t *testing.T) {
	task, _, initial := loadTask(t, 209)
	_, err := render.Compute(initial, task.Targets, render.Config{MaxWorkers: render.MaxWorkerLimit + 1, BlockPixels: 16})
	code, _ := errs.CodeOf(err)
	if code != errs.CodeConfig {
		t.Fatalf("worker cap error=%v", err)
	}
	_, err = render.Compute(initial, task.Targets, render.Config{MaxWorkers: 1, BlockPixels: render.MaxBlockPixelLimit + 1})
	code, _ = errs.CodeOf(err)
	if code != errs.CodeConfig {
		t.Fatalf("pixel buffer cap error=%v", err)
	}
}

func TestTargetChecksumAndDecodeErrors(t *testing.T) {
	task, _, _ := loadTask(t, 205)
	data, err := render.EncodeTarget(task.Targets[0])
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-40] ^= 0x80
	_, err = render.DecodeTarget(corrupt, "corrupt.drtgt")
	code, ok := errs.CodeOf(err)
	if !ok || code != errs.CodeDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
	e, _ := errs.As(err)
	if e.File != "corrupt.drtgt" || e.Field != "target.checksum" {
		t.Fatalf("decode error=%+v", e)
	}
	path := filepath.Join(t.TempDir(), "bad.drtgt")
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = render.ReadTargetFile(path)
	code, _ = errs.CodeOf(err)
	if code != errs.CodeDecode {
		t.Fatalf("read corrupt target err=%v", err)
	}
	if bytes.Equal(corrupt, data) {
		t.Fatal("corruption did not change bytes")
	}
}

func TestParameterBoundsReturnEPara(t *testing.T) {
	task, _, initial := loadTask(t, 206)
	bad := initial.Clone()
	bad.Spheres[0].Radius = 0
	_, err := render.Compute(bad, task.Targets, render.Config{MaxWorkers: 1})
	code, ok := errs.CodeOf(err)
	if !ok || code != errs.CodeParam {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	e, _ := errs.As(err)
	if e.Field != "spheres[0].radius" {
		t.Fatalf("parameter field=%q", e.Field)
	}
}

func TestTargetShapeMismatchReturnsEDecode(t *testing.T) {
	task, _, initial := loadTask(t, 207)
	targets := cloneTargets(task.Targets)
	targets[0].Width++
	targets[0].Pixels = append(targets[0].Pixels, 0, 0, 0)
	_, err := render.Compute(initial, targets, render.Config{MaxWorkers: 1})
	code, ok := errs.CodeOf(err)
	if !ok || code != errs.CodeDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}

func cloneTargets(in []render.Image) []render.Image {
	out := make([]render.Image, len(in))
	for i := range in {
		out[i] = in[i].Clone()
	}
	return out
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

func errorsNew(s string) error { return simpleError(s) }
