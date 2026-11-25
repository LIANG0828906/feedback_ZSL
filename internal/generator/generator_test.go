package generator

import (
	"encoding/json"
	"testing"
)

func TestDeterministic(t *testing.T) {
	a, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	if string(ra) != string(rb) {
		t.Fatal("generator output is not deterministic for a fixed seed")
	}
	c, _ := Generate(DefaultSeed + 1)
	rc, _ := json.Marshal(c)
	if string(ra) == string(rc) {
		t.Fatal("different seeds produced identical documents")
	}
}

func TestTruthShape(t *testing.T) {
	doc, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Glyphs) != 3 {
		t.Fatalf("glyphs=%d", len(doc.Glyphs))
	}
	for _, g := range doc.Truth.Glyphs {
		if g.MaskWidth <= 0 || g.MaskHeight <= 0 {
			t.Fatalf("bad mask dims for %s", g.GlyphID)
		}
		if g.ClosedArea <= 0 {
			t.Fatalf("non-positive area for %s: %f", g.GlyphID, g.ClosedArea)
		}
		if len(g.SamplePoints) != 50 {
			t.Fatalf("sample points=%d for %s", len(g.SamplePoints), g.GlyphID)
		}
		for _, sp := range g.SamplePoints {
			if sp.Density < 0 || sp.Density > 1 {
				t.Fatalf("density out of normalized range: %d", sp.Density)
			}
		}
	}
}
