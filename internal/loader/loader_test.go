package loader

import (
	"encoding/json"
	"strings"
	"testing"

	"strokeforge/internal/generator"
	"strokeforge/internal/model"
)

func TestParseRoundTrip(t *testing.T) {
	doc, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(parsed.Glyphs) != 3 {
		t.Fatalf("glyph count = %d", len(parsed.Glyphs))
	}
}

func TestMalformedJSONIsEDecode(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Fatal("expected error")
	} else if ce, ok := err.(*model.CodedError); !ok || ce.Code != model.EDecode {
		t.Fatalf("want E_DECODE, got %v", err)
	}
}

func TestUnclosedContourIsEDecode(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.Glyphs[0].Contours[0].Closed = false
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.EDecode) {
		t.Fatalf("want E_DECODE, got %v", err)
	}
}

func TestBadSegmentTypeIsEDecode(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.Glyphs[0].Contours[0].Segments[0].Typ = "quartic"
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.EDecode) {
		t.Fatalf("want E_DECODE, got %v", err)
	}
}

func TestCoordinateOutOfQuantizationIsEDecode(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.Glyphs[0].Contours[0].Segments[0].P0.X = 9999
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.EDecode) {
		t.Fatalf("want E_DECODE, got %v", err)
	}
}

func TestMissingFieldIsESchema(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.Seed = 0
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.ESchema) {
		t.Fatalf("want E_SCHEMA, got %v", err)
	}
}

func TestWidthOutOfRangeIsEPara(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.StrokeParams.Width = 500
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.EPara) {
		t.Fatalf("want E_PARA, got %v", err)
	}
}

func TestArcCoverageGapIsEDecode(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.Glyphs[0].StrokeSegments[1].StartRatio = 0.3
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.EDecode) {
		t.Fatalf("want E_DECODE, got %v", err)
	}
}

func TestDensityOverflowIsEDens(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.Truth.Glyphs[0].SamplePoints[0].Density = 999
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.EDens) {
		t.Fatalf("want E_DENS, got %v", err)
	}
	if ce, ok := err.(*model.CodedError); ok && ce.Field != "density" {
		t.Fatalf("expected density field, got %s", ce.Field)
	}
}

func TestMaskDimensionMismatchNamesField(t *testing.T) {
	doc, _ := generator.Generate(generator.DefaultSeed)
	doc.Truth.Glyphs[1].MaskWidth += 7
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	if err == nil || !strings.Contains(err.Error(), model.EDens) {
		t.Fatalf("want E_DENS, got %v", err)
	}
	if ce, ok := err.(*model.CodedError); ok && ce.Field != "mask_width" {
		t.Fatalf("expected mask_width field, got %s", ce.Field)
	}
}
