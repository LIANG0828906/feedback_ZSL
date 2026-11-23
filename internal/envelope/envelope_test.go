package envelope_test

import (
	"strings"
	"testing"

	"captionx/internal/envelope"
	"captionx/internal/errs"
	"captionx/internal/loader"
	"captionx/internal/model"
	"captionx/internal/samplegen"
)

func loadProject(t *testing.T) *loader.Project {
	t.Helper()
	dirs, err := samplegen.Generate(samplegen.Options{OutDir: t.TempDir(), Count: 1, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	p, err := loader.Load(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetectBoundariesOnHopGrid(t *testing.T) {
	p := loadProject(t)
	fp := p.Manifest.Frame
	det, err := envelope.Detect(p.Audio.Samples, fp, p.Manifest.Segments)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if len(det.Segments) != len(p.Manifest.Segments) {
		t.Fatalf("detected %d segments, want %d", len(det.Segments), len(p.Manifest.Segments))
	}
	for i, ds := range det.Segments {
		for _, b := range []model.BoundaryObservation{ds.Start, ds.End} {
			if b.Quantized%fp.FrameHop != 0 {
				t.Fatalf("seg %d %s: quantized %d not on hop grid", i, b.Kind, b.Quantized)
			}
			if b.QuantError > fp.FrameHop/2 {
				t.Fatalf("seg %d %s: quant error %d > hop/2", i, b.Kind, b.QuantError)
			}
		}
		decl := p.Manifest.Segments[i]
		if d := ds.Start.Quantized - decl.Start; d < -fp.FrameHop || d > fp.FrameHop {
			t.Fatalf("seg %d start deviation %d exceeds hop", i, d)
		}
		if d := ds.End.Quantized - decl.End; d < -fp.FrameHop || d > fp.FrameHop {
			t.Fatalf("seg %d end deviation %d exceeds hop", i, d)
		}
	}
}

func TestDetectRejectsBadFrameParams(t *testing.T) {
	p := loadProject(t)
	fp := p.Manifest.Frame
	fp.FrameHop = 0
	_, err := envelope.Detect(p.Audio.Samples, fp, p.Manifest.Segments)
	if errs.Code(err) != errs.EPara {
		t.Fatalf("code = %s, want E_PARA", errs.Code(err))
	}
	e := err.(*errs.Error)
	if e.Field != "frame.frameHop" {
		t.Fatalf("field = %q", e.Field)
	}
	if !strings.Contains(e.Msg, "[1,") {
		t.Fatalf("E_PARA should carry the legal range, got %q", e.Msg)
	}
}
