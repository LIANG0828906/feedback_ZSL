package ocrflow

import (
	"encoding/json"
	"math"
	"sync"
	"testing"
)

func TestGeneratorTruthIsDeterministicAndValid(t *testing.T) {
	a, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 4 {
		t.Fatalf("expected 4 screenshots, got %d", len(a))
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatal("generated manifest/truth is not deterministic")
	}
	for _, shot := range a {
		if shot.ContentHash == "" || len(shot.ContentHash) < 32 {
			t.Fatalf("bad content hash for %s", shot.ShotID)
		}
		if len(shot.Lines) == 0 {
			t.Fatalf("shot %s has no truth lines", shot.ShotID)
		}
		for _, line := range shot.Lines {
			if line.Text == "" {
				t.Fatalf("%s line %d has empty text", shot.ShotID, line.LineNo)
			}
			if line.CharCount != len([]rune(line.Text)) {
				t.Fatalf("%s line %d char count mismatch", shot.ShotID, line.LineNo)
			}
			if line.AvgStrokeWidthPx <= 0 || line.AvgStrokeWidthMilliPx != line.AvgStrokeWidthPx*1000 {
				t.Fatalf("%s line %d bad stroke width", shot.ShotID, line.LineNo)
			}
			if !ValidQuad(line.Quad) {
				t.Fatalf("%s line %d degenerate quad", shot.ShotID, line.LineNo)
			}
		}
	}
}

func TestThreeRecipeSchemasLoadAndValidate(t *testing.T) {
	canonical := SampleRecipe()
	versions := []struct {
		schema string
		source string
	}{{SchemaV1, "v1"}, {SchemaV2, "v2"}, {SchemaV3, "v3"}}
	for _, v := range versions {
		data, err := EncodeRecipeDocument(canonical, v.schema)
		if err != nil {
			t.Fatalf("%s encode: %v", v.source, err)
		}
		r, err := LoadRecipeDocument(data)
		if err != nil {
			t.Fatalf("%s load: %v", v.source, err)
		}
		if r.SourceVersion != v.source {
			t.Fatalf("source=%s want %s", r.SourceVersion, v.source)
		}
		if len(r.Items) != len(canonical.Items) {
			t.Fatalf("%s items=%d", v.source, len(r.Items))
		}
	}

	badMap, err := RecipeToMap(canonical, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	delete(badMap["detection"].(map[string]any), "merge")
	badJSON, _ := json.Marshal(badMap)
	_, err = LoadRecipeDocument(badJSON)
	oe, ok := asError(err)
	if !ok || oe.Code != CodeSchema {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	if oe.SourceVersion != SchemaV3 || oe.Field != "detection.merge" {
		t.Fatalf("unexpected schema error source=%s field=%s", oe.SourceVersion, oe.Field)
	}

	v1Map, err := RecipeToMap(canonical, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	v1Map["detect"].(map[string]any)["candidate_score_permille"] = 1200
	badV1, _ := json.Marshal(v1Map)
	_, err = LoadRecipeDocument(badV1)
	oe, ok = asError(err)
	if !ok || oe.Code != CodeSchema || oe.Field != "detect.candidate_score_permille" || oe.SourceVersion != SchemaV1 {
		t.Fatalf("expected source-path E_SCHEMA for permille scale, got %+v", err)
	}
}

func TestIntegerConversionRoundTripAndOverflow(t *testing.T) {
	rep, err := VerifyConversionRoundTrip(SampleRecipe())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("round trip outside residual: %+v", rep.Items)
	}
	_, err = PermilleToBPS(math.MaxInt, "detection.merge_overlap_permille")
	oe, ok := asError(err)
	if !ok || oe.Code != CodeOverflow {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	if oe.Field == "" || oe.OperandA == "" || oe.OperandB == "" {
		t.Fatalf("overflow error missing field/operands: %+v", oe)
	}
}

func TestDetectionRecognitionExactDeterministicAndTemplates(t *testing.T) {
	shots, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	recipe := DefaultRecipe()
	lib := DefaultTemplateLibrary()
	for _, shot := range shots {
		run := func() (*DetectionResult, []LineRecognition) {
			prep, err := PreprocessImage(shot.RGBA, recipe.Preprocess)
			if err != nil {
				t.Fatal(err)
			}
			det, err := DetectLines(shot, recipe, prep)
			if err != nil {
				t.Fatal(err)
			}
			recs := make([]LineRecognition, 0, len(det.Lines))
			for _, line := range det.Lines {
				rec, err := RecognizeLine(line, det, det.Mask, lib, recipe.Recognition)
				if err != nil {
					t.Fatal(err)
				}
				recs = append(recs, *rec)
			}
			return det, recs
		}
		det1, rec1 := run()
		det2, _ := run()
		j1, _ := json.Marshal(det1.Lines)
		j2, _ := json.Marshal(det2.Lines)
		if string(j1) != string(j2) {
			t.Fatalf("%s detection is not field-deterministic", shot.ShotID)
		}
		if det1.Mask == nil || det1.Mask.Count() == 0 {
			t.Fatalf("%s missing pixel candidate mask", shot.ShotID)
		}
		if delta := absInt(len(det1.Lines) - len(shot.Lines)); delta > DetectionLineCountTolerance {
			t.Fatalf("%s line count delta %d > tolerance %d", shot.ShotID, delta, DetectionLineCountTolerance)
		}
		report := CompareTruth(shot, rec1)
		if report.CERBPS != 0 {
			t.Fatalf("%s CER=%d distance=%d", shot.ShotID, report.CERBPS, report.TotalDistance)
		}
		for _, rec := range rec1 {
			if rec.ConfidenceBPS < 0 || rec.ConfidenceBPS > 10000 {
				t.Fatalf("confidence out of range: %d", rec.ConfidenceBPS)
			}
			for _, ch := range rec.Chars {
				if ch.TemplateVersion != TemplateVersionV1 {
					t.Fatalf("mixed template version %s != %s", ch.TemplateVersion, TemplateVersionV1)
				}
			}
		}

		var wg sync.WaitGroup
		results := make([]string, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				det, _ := run()
				b, _ := json.Marshal(det.Lines)
				results[i] = string(b)
			}(i)
		}
		wg.Wait()
		for _, got := range results[1:] {
			if got != results[0] {
				t.Fatalf("%s concurrent detection differs", shot.ShotID)
			}
		}
	}

	empty, err := RecognizeLine(DetectedLine{LineNo: 99, LineID: "EMPTY", Box: Rect{MaxX: 10, MaxY: 10}, StrokeWidthPx: 2}, &DetectionResult{}, NewMask(10, 10), lib, recipe.Recognition)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Empty || empty.Text != "" || empty.ConfidenceBPS != 0 {
		t.Fatalf("empty line not explicitly marked: %+v", empty)
	}

	prep, _ := PreprocessImage(shots[0].RGBA, recipe.Preprocess)
	det, _ := DetectLines(shots[0], recipe, prep)
	v2, err := RecognizeLine(det.Lines[0], det, det.Mask, V2TemplateLibrary(), recipe.Recognition)
	if err != nil {
		t.Fatal(err)
	}
	if v2.TemplateVersion != TemplateVersionV2 {
		t.Fatalf("template version not reflected: %s", v2.TemplateVersion)
	}
	for _, ch := range v2.Chars {
		if ch.TemplateVersion != TemplateVersionV2 {
			t.Fatalf("mixed V2 hit: %s", ch.TemplateVersion)
		}
	}
}

func TestRuntimeParameterErrorsHaveAllowedSet(t *testing.T) {
	for _, mutate := range []func(*Recipe){
		func(r *Recipe) { r.Detection.MergeOverlapBPS = 9900 },
		func(r *Recipe) { r.Detection.StrokeConsistencyBPS = 4000 },
	} {
		r := DefaultRecipe()
		mutate(r)
		shot := &Screenshot{ShotID: "x", Width: 10, Height: 10, RGBA: nil}
		_, err := DetectLines(shot, r, &PreparedImage{Mask: NewMask(10, 10)})
		oe, ok := asError(err)
		if !ok || oe.Code != CodeParam || oe.Field == "" || oe.AllowedSet == nil {
			t.Fatalf("expected E_PARA with field and allowed set, got %v", err)
		}
	}
}
