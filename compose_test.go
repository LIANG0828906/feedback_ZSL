package vpcompose_test

import (
	"sync"
	"testing"

	"vpcompose"
	"vpcompose/sample"
)

func TestCompositionIsDeterministicAndTruthExact(t *testing.T) {
	generated, err := sample.Generate(11)
	if err != nil {
		t.Fatal(err)
	}
	project, err := vpcompose.LoadProject(generated.Files["project-v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	first, err := vpcompose.ComposeFrame(project, 5)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	var wg sync.WaitGroup
	digests := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			frame, err := vpcompose.ComposeFrame(project, 5)
			if err != nil {
				errs[index] = err
				return
			}
			digests[index] = frame.Digest()
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if digests[i] != first.Digest() {
			t.Fatalf("worker %d digest differs", i)
		}
	}
	if first.Width != project.Width || first.Height != project.Height || first.Channels != project.Channels {
		t.Fatalf("frame geometry not derived from model")
	}
	if first.Timebase != project.Timebase || first.TimeTicks != project.Frames[5].TimeTicks {
		t.Fatalf("frame timebase not derived from model")
	}
	if err := vpcompose.VerifyTruth(project); err != nil {
		t.Fatal(err)
	}
}

func TestPixelTruthMismatchIsEPix(t *testing.T) {
	generated, err := sample.Generate(12)
	if err != nil {
		t.Fatal(err)
	}
	project, err := vpcompose.LoadProject(generated.Files["project-v1.json"])
	if err != nil {
		t.Fatal(err)
	}
	project.Truth.Frames[2].SamplePixels[1].RGBA[0] ^= 1
	err = vpcompose.VerifyTruth(project)
	if !vpcompose.IsCode(err, vpcompose.CodePixel) {
		t.Fatalf("expected E_PIX, got %v", err)
	}
	ee := err.(*vpcompose.EngineError)
	if ee.Path == "" || ee.Operands[0] < 1 {
		t.Fatalf("pixel mismatch details = %+v", ee)
	}
}
