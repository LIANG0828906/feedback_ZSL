package maskqa

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestThreeVersionLoadAndSchemaErrors(t *testing.T) {
	set, err := GenerateSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range set.Fixtures {
		for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
			data := fx.Exports[version].Data
			p, err := LoadProject(data)
			if err != nil {
				t.Fatalf("%s/%s load: %v", fx.Name, version, err)
			}
			if p.SourceVersion != version {
				t.Fatalf("source version = %q, want %q", p.SourceVersion, version)
			}
			if len(p.Strokes) != 5 {
				t.Fatalf("%s/%s strokes=%d", fx.Name, version, len(p.Strokes))
			}
			if p.ProjectID != fx.Project.ProjectID {
				t.Fatalf("project id mismatch")
			}
		}
	}

	_, err = LoadProject([]byte(`{"schema":"maskqa.icon_project/v9"}`))
	qe, ok := AsError(err)
	if !ok || qe.Code != CodeSchema || qe.Path != "schema" || qe.SourceVersion != "v9" {
		t.Fatalf("unknown version error = %#v", err)
	}

	fx := set.Fixtures[0]
	var doc map[string]any
	if err := json.Unmarshal(fx.Exports[SchemaV1].Data, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "icon_id")
	bad, _ := json.Marshal(doc)
	_, err = LoadProject(bad)
	qe, ok = AsError(err)
	if !ok || qe.Code != CodeSchema || !strings.Contains(qe.Path, "icon_id") || qe.SourceVersion != SchemaV1 {
		t.Fatalf("missing icon_id error = %#v", err)
	}

	doc = map[string]any{}
	_ = json.Unmarshal(fx.Exports[SchemaV3].Data, &doc)
	strokes := doc["strokes"].([]any)
	first := strokes[0].(map[string]any)
	delete(first, "weight_basis_points")
	strokes[0] = first
	doc["strokes"] = strokes
	bad, _ = json.Marshal(doc)
	_, err = LoadProject(bad)
	qe, ok = AsError(err)
	if !ok || qe.Code != CodeSchema || !strings.Contains(qe.Path, "weight_basis_points") || qe.SourceVersion != SchemaV3 {
		t.Fatalf("missing weight error = %#v", err)
	}
}

func TestVersionRoundTripAndIntegerOverflow(t *testing.T) {
	set, err := GenerateSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range set.Fixtures {
		for _, lowVersion := range []string{SchemaV1, SchemaV2} {
			loaded := fx.Loaded[lowVersion]
			byIndex := map[int]Stroke{}
			for _, s := range loaded.Strokes {
				byIndex[s.Index] = s
			}
			for _, original := range fx.Project.Strokes {
				got := byIndex[original.Index]
				if got.Index != original.Index {
					t.Fatalf("stroke index lost: %d", original.Index)
				}
				if got.Start != original.Start || got.End != original.End {
					t.Fatalf("%s/%s stroke %d coords %v->%v not restored", fx.Name, lowVersion, original.Index, original.Start, got.Start)
				}
				diff := original.WeightBPS - got.WeightBPS
				if diff < 0 {
					diff = -diff
				}
				if diff > 9 {
					t.Fatalf("stroke %d weight residual %d BPS exceeds declared 9", original.Index, diff)
				}
			}
		}
	}

	q, tr, err := WeightBPSToPermille(8355, RoundingFloor, "strokes[0].weight_basis_points", 1)
	if err != nil || q != 835 || tr.Remainder != 5 || tr.Position == "" {
		t.Fatalf("weight truncation q=%d tr=%+v err=%v", q, tr, err)
	}

	bad := int(math.MaxInt64/10 + 1)
	_, err = WeightPermilleToBPS(bad, "strokes[9].weight_permille")
	qe, ok := AsError(err)
	if !ok || qe.Code != CodeOVF || qe.Field == "" || len(qe.Operands) != 2 {
		t.Fatalf("weight overflow error = %#v", err)
	}

	enc := CoordinateEncoding{Mode: "stroke_index_offset", IndexStepX: math.MaxInt64, IndexStepY: 0, OffsetQuantum: 1}
	_, _, err = AbsoluteToIndexOffset(Point{X: math.MinInt64, Y: 0}, 2, enc, "strokes[1].startOffset")
	qe, ok = AsError(err)
	if !ok || qe.Code != CodeOVF || len(qe.Operands) != 2 {
		t.Fatalf("coordinate overflow error = %#v", err)
	}
}

func TestIndexOffsetQuantizationResidual(t *testing.T) {
	enc := CoordinateEncoding{Mode: "stroke_index_offset", IndexStepX: 4, IndexStepY: 6, OffsetQuantum: 2}
	p := Point{X: 11, Y: 17}
	offset, truncations, err := AbsoluteToIndexOffset(p, 2, enc, "strokes[1].startOffset")
	if err != nil {
		t.Fatal(err)
	}
	if len(truncations) != 2 {
		t.Fatalf("expected x/y truncation records, got %+v", truncations)
	}
	back, err := IndexOffsetToAbsolute(offset, 2, enc, "strokes[1].startOffset")
	if err != nil {
		t.Fatal(err)
	}
	if absInt(back.X-p.X) > enc.OffsetQuantum-1 || absInt(back.Y-p.Y) > enc.OffsetQuantum-1 {
		t.Fatalf("quantized round trip %v -> %v residual too large", p, back)
	}
	for _, tr := range truncations {
		if tr.Position == "" || tr.Remainder == 0 {
			t.Fatalf("truncation location not recorded: %+v", tr)
		}
	}
}
