package samplegen_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"glyphbrush/internal/samplegen"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	a, at := samplegen.Generate(samplegen.DefaultSeed)
	b, bt := samplegen.Generate(samplegen.DefaultSeed)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(at, bt) {
		t.Fatal("same seed did not produce identical documents")
	}
	c, _ := samplegen.Generate(samplegen.DefaultSeed + 1)
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seeds produced identical glyph outlines")
	}
}

func TestFilesAreByteStableAndComplete(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	if _, _, err := samplegen.Write(dir1, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := samplegen.Write(dir2, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"glyphs.json", "truth.json"} {
		a, err := os.ReadFile(filepath.Join(dir1, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("%s is not byte-stable", name)
		}
	}
	doc, truth := samplegen.Generate(samplegen.DefaultSeed)
	for _, g := range doc.Glyphs {
		if len(g.Contours) < 2 {
			t.Fatalf("%s does not have multiple contours", g.ID)
		}
		var quad, cubic bool
		for _, c := range g.Contours {
			for _, s := range c.Segments {
				quad = quad || string(s.Type) == "quadratic"
				cubic = cubic || string(s.Type) == "cubic"
			}
		}
		if !quad && !cubic {
			t.Fatalf("%s has no supported curve types", g.ID)
		}
	}
	if len(truth.Glyphs) != len(doc.Glyphs) {
		t.Fatal("truth count differs from glyph count")
	}
	var sampleCount int
	for _, g := range truth.Glyphs {
		sampleCount += len(g.Samples)
		if g.ClosedArea <= 0 {
			t.Fatalf("%s has non-positive area", g.ID)
		}
	}
	if sampleCount == 0 {
		t.Fatal("truth has no known curve samples")
	}
	raw, err := json.Marshal(truth)
	if err != nil || len(raw) == 0 {
		t.Fatalf("truth cannot encode: %v", err)
	}
}
