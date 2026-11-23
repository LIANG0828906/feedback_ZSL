package loader_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/samplegen"
)

func writeSamplePair(t *testing.T) (string, map[string]any, map[string]any) {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	return dir, readMap(t, filepath.Join(dir, "glyphs.json")), readMap(t, filepath.Join(dir, "truth.json"))
}

func readMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeMap(t *testing.T, path string, m any) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func firstContour(doc map[string]any) (map[string]any, map[string]any) {
	glyph := doc["glyphs"].([]any)[0].(map[string]any)
	contour := glyph["contours"].([]any)[0].(map[string]any)
	return glyph, contour
}

func TestSchemaErrorsCarryGlyphAndContour(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(doc, truth map[string]any)
	}{
		{
			name: "unclosed",
			mutate: func(doc, truth map[string]any) {
				_, contour := firstContour(doc)
				contour["closed"] = false
			},
		},
		{
			name: "missing control",
			mutate: func(doc, truth map[string]any) {
				_, contour := firstContour(doc)
				seg := contour["segments"].([]any)[0].(map[string]any)
				delete(seg, "control")
			},
		},
		{
			name: "discontinuous",
			mutate: func(doc, truth map[string]any) {
				_, contour := firstContour(doc)
				seg := contour["segments"].([]any)[1].(map[string]any)
				seg["start"] = map[string]any{"x": 9999, "y": 9999}
			},
		},
		{
			name: "bad type",
			mutate: func(doc, truth map[string]any) {
				_, contour := firstContour(doc)
				seg := contour["segments"].([]any)[0].(map[string]any)
				seg["type"] = "line"
			},
		},
		{
			name: "out of range",
			mutate: func(doc, truth map[string]any) {
				_, contour := firstContour(doc)
				seg := contour["segments"].([]any)[0].(map[string]any)
				ctrl := seg["control"].([]any)[0].(map[string]any)
				ctrl["x"] = 999999
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, doc, truth := writeSamplePair(t)
			tc.mutate(doc, truth)
			writeMap(t, filepath.Join(dir, "glyphs.json"), doc)
			_, err := loader.Load(dir)
			if !errs.IsCode(err, errs.CodeSchema) {
				t.Fatalf("expected E_SCHEMA, got %v", err)
			}
			e, _ := errs.As(err)
			if e.GlyphID == "" || e.ContourIndex == nil {
				t.Fatalf("error lacks glyph/contour: %+v", e)
			}
		})
	}
}

func TestTruthAreaMismatchIsSchemaError(t *testing.T) {
	dir, doc, truth := writeSamplePair(t)
	glyph := doc["glyphs"].([]any)[0].(map[string]any)
	id := glyph["id"].(string)
	tglyphs := truth["glyphs"].([]any)
	for _, raw := range tglyphs {
		tg := raw.(map[string]any)
		if tg["id"] == id {
			tg["closedArea"] = 1.0
		}
	}
	writeMap(t, filepath.Join(dir, "truth.json"), truth)
	_, err := loader.Load(dir)
	if !errs.IsCode(err, errs.CodeSchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	e, _ := errs.As(err)
	if e.GlyphID != id {
		t.Fatalf("wrong glyph in error: %+v", e)
	}
}

func TestTruthSampleMismatchIsSchemaError(t *testing.T) {
	dir, doc, truth := writeSamplePair(t)
	glyph := doc["glyphs"].([]any)[0].(map[string]any)
	id := glyph["id"].(string)
	for _, raw := range truth["glyphs"].([]any) {
		tg := raw.(map[string]any)
		if tg["id"] == id {
			tg["samples"].([]any)[0].(map[string]any)["x"] = 12345.0
		}
	}
	writeMap(t, filepath.Join(dir, "truth.json"), truth)
	_, err := loader.Load(dir)
	if !errs.IsCode(err, errs.CodeSchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
}
