package loader_test

import (
	"testing"

	"glyphbrush/internal/loader"
	"glyphbrush/internal/samplegen"
)

func TestGeneratedSamplesValidateAndMatchTruth(t *testing.T) {
	dir := t.TempDir()
	doc, truth, err := samplegen.Write(dir, samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Glyphs) != len(doc.Glyphs) || len(bundle.Glyphs) != len(truth.Glyphs) {
		t.Fatalf("glyph counts differ: bundle=%d doc=%d truth=%d", len(bundle.Glyphs), len(doc.Glyphs), len(truth.Glyphs))
	}
	for _, g := range bundle.Glyphs {
		if len(g.Glyph.Contours) < 2 {
			t.Fatalf("%s: expected multiple contours", g.Glyph.ID)
		}
		if len(g.Skeletons) != len(g.Glyph.Contours) {
			t.Fatalf("%s: skeleton count mismatch", g.Glyph.ID)
		}
		for _, sk := range g.Skeletons {
			if sk.Length <= 0 {
				t.Fatalf("%s contour %d: non-positive arc length", g.Glyph.ID, sk.ContourIndex)
			}
			if len(sk.Runs) != len(g.Glyph.Contours[sk.ContourIndex].Segments) {
				t.Fatalf("%s contour %d: run count mismatch", g.Glyph.ID, sk.ContourIndex)
			}
		}
	}
}
