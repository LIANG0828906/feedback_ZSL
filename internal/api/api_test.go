package api_test

import (
	"testing"

	"captionx/internal/api"
	"captionx/internal/loader"
	"captionx/internal/pipeline"
	"captionx/internal/samplegen"
)

func TestQueryInterface(t *testing.T) {
	dirs, err := samplegen.Generate(samplegen.Options{OutDir: t.TempDir(), Count: 1, Seed: 5})
	if err != nil {
		t.Fatal(err)
	}
	p, err := loader.Load(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	res, err := pipeline.RunFull(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := api.NewEngine(res)

	first := res.Captions[0]
	w, ok := e.CaptionWindow(first.CaptionID)
	if !ok || w != first.Window {
		t.Fatalf("CaptionWindow = %+v ok=%v", w, ok)
	}
	if _, ok := e.CaptionWindow(99999); ok {
		t.Fatal("unknown caption must miss")
	}
	h, ok := e.CharHighlight(first.CaptionID, 0)
	if !ok || h.Start != first.CharStarts[0] || h.End != first.CharEnds[0] {
		t.Fatalf("CharHighlight = %+v ok=%v", h, ok)
	}
	if _, ok := e.CharHighlight(first.CaptionID, len(first.CharStarts)); ok {
		t.Fatal("out-of-range char must miss")
	}
	rep := e.ErrorReport()
	if len(rep.Captions) != len(res.Captions) {
		t.Fatal("report captions mismatch")
	}
}
