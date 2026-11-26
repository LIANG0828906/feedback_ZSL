package strokelab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestGeneratorDeterministicAndMultiResolution(t *testing.T) {
	a, err := GenerateSuite(515151)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSuite(515151)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Samples) != 3 || len(b.Samples) != 3 {
		t.Fatalf("expected three samples")
	}
	wantSizes := []int{64, 96, 128}
	for i := range a.Samples {
		if a.Samples[i].Project.Width != wantSizes[i] {
			t.Fatalf("sample %d width %d", i, a.Samples[i].Project.Width)
		}
		if !bytes.Equal(a.Samples[i].VersionJSON, b.Samples[i].VersionJSON) {
			t.Fatalf("sample %s is not deterministic", a.Samples[i].Name)
		}
		if !masksEqual(a.Samples[i].CandidateMask, b.Samples[i].CandidateMask) {
			t.Fatalf("candidate %s is not deterministic", a.Samples[i].Name)
		}
		diff := 0
		for j := range a.Samples[i].CandidateMask {
			if a.Samples[i].CandidateMask[j] != a.Samples[i].Project.Truth.ReferenceMask[j] {
				diff++
			}
		}
		if diff == 0 {
			t.Fatalf("sample %s candidate is not deliberately perturbed", a.Samples[i].Name)
		}
		if a.Samples[i].Icon == nil || a.Samples[i].Icon.Bounds().Dx() != wantSizes[i] {
			t.Fatalf("sample %s icon missing or wrong size", a.Samples[i].Name)
		}
		hasPartial, hasStrong, hasZero := false, false, false
		for _, v := range a.Samples[i].CandidateMask {
			switch {
			case v == 0:
				hasZero = true
			case v >= 128:
				hasStrong = true
			default:
				hasPartial = true
			}
		}
		if !hasZero || !hasStrong || !hasPartial {
			t.Fatalf("sample %s candidate coverage lacks inset/outset/void mix", a.Samples[i].Name)
		}
		hasSemiTransparent := false
		for y := a.Samples[i].Icon.Bounds().Min.Y; y < a.Samples[i].Icon.Bounds().Max.Y; y++ {
			for x := a.Samples[i].Icon.Bounds().Min.X; x < a.Samples[i].Icon.Bounds().Max.X; x++ {
				if _, _, _, alpha := a.Samples[i].Icon.At(x, y).RGBA(); alpha < 0xffff {
					hasSemiTransparent = true
				}
			}
		}
		if !hasSemiTransparent {
			t.Fatalf("sample %s icon lacks semi-transparent outline pixels", a.Samples[i].Name)
		}
	}
}

func TestVersionLoadersShapeAndRules(t *testing.T) {
	suite, err := GenerateSuite(8102)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range suite.Samples {
		var doc map[string]any
		if err := json.Unmarshal(sample.VersionJSON, &doc); err != nil {
			t.Fatal(err)
		}
		switch sample.Version {
		case Version1:
			ops := doc["operations"].([]any)
			first := ops[0].(map[string]any)
			if first["step_seq"].(float64) != 0 {
				t.Fatalf("v1 step_seq is not zero based")
			}
			if _, ok := first["hardness_permille"]; ok {
				t.Fatalf("v1 unexpectedly contains hardness_permille")
			}
			if _, ok := first["from_px"]; !ok {
				t.Fatalf("v1 does not use absolute from_px")
			}
			if got := first["weight_permille"].(float64); got != 800 {
				t.Fatalf("v1 weight permille=%v", got)
			}
		case Version2:
			ops := doc["brush_ops"].([]any)
			first := ops[0].(map[string]any)
			if first["order_no"].(float64) != 1 {
				t.Fatalf("v2 order_no is not one based")
			}
			if _, ok := first["hardness_permille"]; ok {
				t.Fatalf("v2 unexpectedly contains hardness_permille")
			}
			if _, ok := first["offset"]; !ok {
				t.Fatalf("v2 does not use relative offset")
			}
			if got := first["weight_permille"].(float64); got != 800 {
				t.Fatalf("v2 weight permille=%v", got)
			}
		case Version3:
			groups := doc["construct_groups"].([]any)
			first := groups[0].(map[string]any)["constructs"].([]any)[0].(map[string]any)
			if first["seq"].(float64) != 1 {
				t.Fatalf("v3 seq is not one based")
			}
			if _, ok := first["hardness_permille"]; !ok {
				t.Fatalf("v3 is missing hardness_permille")
			}
			if _, ok := first["from"]; !ok {
				t.Fatalf("v3 is missing absolute from")
			}
			if got := first["weight_pp10000"].(float64); got != 8400 {
				t.Fatalf("v3 weight pp10000=%v", got)
			}
		}
	}
}

func TestUnknownVersionAndMissingRequiredField(t *testing.T) {
	if _, err := LoadProjectJSON([]byte(`{"version":"mask-work/v99"}`)); ErrorCode(err) != CodeVersion {
		t.Fatalf("unknown version error = %v", err)
	}
	suite, err := GenerateSuite(19)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(suite.Samples[0].VersionJSON, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "width")
	data, _ := json.Marshal(doc)
	if _, err := LoadProjectJSON(data); ErrorCode(err) != CodeVersion {
		t.Fatalf("missing width error = %v", err)
	}
}

func TestV1DefaultBackfillTraces(t *testing.T) {
	suite, err := GenerateSuite(302)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(suite.Samples[0].VersionJSON, &doc); err != nil {
		t.Fatal(err)
	}
	op := doc["operations"].([]any)[0].(map[string]any)
	delete(op, "step_seq")
	delete(op, "weight_permille")
	data, _ := json.Marshal(doc)
	p, err := LoadProjectJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	s := p.Strokes[0]
	if s.Seq != 1 || s.Weight != 10000 || s.Hardness != 1000 {
		t.Fatalf("defaults not applied: %+v", s)
	}
	if !hasTrace(p.ConversionTrace, "operations[0].step_seq", "<missing>", "1") {
		t.Fatalf("missing seq backfill trace")
	}
	if !hasTrace(p.ConversionTrace, "operations[0].weight_permille", "<missing>", "10000") {
		t.Fatalf("missing weight backfill trace")
	}
	if !hasTrace(p.ConversionTrace, "operations[0].hardness_permille", "<missing>", "1000") {
		t.Fatalf("missing hardness backfill trace")
	}
}

func TestV2DefaultBackfillTraces(t *testing.T) {
	suite, err := GenerateSuite(303)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(suite.Samples[1].VersionJSON, &doc); err != nil {
		t.Fatal(err)
	}
	op := doc["brush_ops"].([]any)[0].(map[string]any)
	delete(op, "weight_permille")
	data, _ := json.Marshal(doc)
	p, err := LoadProjectJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Strokes[0].Weight != 10000 || p.Strokes[0].Hardness != 1000 {
		t.Fatalf("v2 defaults not applied: %+v", p.Strokes[0])
	}
	if !hasTrace(p.ConversionTrace, "brush_ops[0].weight_permille", "<missing>", "10000") {
		t.Fatalf("missing v2 weight backfill trace")
	}
	if !hasTrace(p.ConversionTrace, "brush_ops[0].hardness_permille", "<missing>", "1000") {
		t.Fatalf("missing v2 hardness backfill trace")
	}
}

func hasTrace(traces []ConversionTrace, path, oldValue, newValue string) bool {
	for _, tr := range traces {
		if tr.Path == path && tr.OldValue == oldValue && tr.NewValue == newValue {
			return true
		}
	}
	return false
}

func TestLosslessExportsAndExactCrossVersionRoundTrip(t *testing.T) {
	suite, err := GenerateSuite(4004)
	if err != nil {
		t.Fatal(err)
	}
	for _, lowerVersion := range suite.Samples[:2] {
		if _, _, err := ExportProject(lowerVersion.Project, Version3, ExportOptions{VerifyRoundTrip: true}); err != nil {
			t.Fatalf("upgrade generated %s to v3: %v", lowerVersion.Version, err)
		}
	}
	base := suite.Samples[2].Project.Clone()
	for i := range base.Strokes {
		base.Strokes[i].Hardness = 1000
	}
	// v3 -> v2 -> v3.
	v2Bytes, report, err := ExportProject(base, Version2, ExportOptions{VerifyRoundTrip: true})
	if err != nil {
		t.Fatalf("export v2: %v", err)
	}
	if !report.RoundTripOK || len(report.Losses) != 0 {
		t.Fatalf("v2 export report: %+v", report)
	}
	v2Loaded, err := LoadProjectJSON(v2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	v3Bytes, _, err := ExportProject(v2Loaded, Version3, ExportOptions{VerifyRoundTrip: true})
	if err != nil {
		t.Fatalf("v2 -> v3: %v", err)
	}
	v3Loaded, err := LoadProjectJSON(v3Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := equivalentForTarget(base, v3Loaded, Version3); mismatch != "" {
		t.Fatalf("v3->v2->v3 mismatch at %s", mismatch)
	}

	// v3 -> v1 -> v3 with v1-expressible values.
	v1Base := base.Clone()
	v1Base.Groups = nil
	stageGroups := make(map[int]string)
	for i := range v1Base.Strokes {
		stage := v1Base.Strokes[i].Stage
		name, ok := stageGroups[stage]
		if !ok {
			name = fmt.Sprintf("stage-%d", stage)
			stageGroups[stage] = name
			v1Base.Groups = append(v1Base.Groups, Group{Name: name, Stage: stage})
		}
		v1Base.Strokes[i].Group = name
	}
	v1Bytes, v1Report, err := ExportProject(v1Base, Version1, ExportOptions{VerifyRoundTrip: true})
	if err != nil {
		t.Fatalf("export v1: %v\n%+v", err, v1Report)
	}
	v1Loaded, err := LoadProjectJSON(v1Bytes)
	if err != nil {
		t.Fatal(err)
	}
	v3Again, _, err := ExportProject(v1Loaded, Version3, ExportOptions{VerifyRoundTrip: true})
	if err != nil {
		t.Fatalf("v1 -> v3: %v", err)
	}
	final, err := LoadProjectJSON(v3Again)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := equivalentForTarget(v1Base, final, Version3); mismatch != "" {
		t.Fatalf("v3->v1->v3 mismatch at %s", mismatch)
	}
	for i := range v1Base.Strokes {
		if v1Base.Strokes[i].Start != final.Strokes[i].Start ||
			v1Base.Strokes[i].End != final.Strokes[i].End ||
			v1Base.Strokes[i].Weight != final.Strokes[i].Weight {
			t.Fatalf("stroke %s coordinates/weight changed in round trip", v1Base.Strokes[i].ID)
		}
	}
}

func TestLossyExportListsAndE_LOSSY(t *testing.T) {
	suite, err := GenerateSuite(6060)
	if err != nil {
		t.Fatal(err)
	}
	base := suite.Samples[2].Project.Clone()
	if _, report, err := ExportProject(base, Version2, ExportOptions{}); ErrorCode(err) != CodeLossy {
		t.Fatalf("expected E_LOSSY, got %v", err)
	} else if !hasLoss(report.Losses, "hardness_not_expressible") {
		t.Fatalf("missing hardness loss: %+v", report.Losses)
	} else {
		for _, item := range report.Losses {
			if item.Disposition == "" || item.Path == "" {
				t.Fatalf("loss item lacks path/disposition: %+v", item)
			}
		}
	}
	for i := range base.Strokes {
		base.Strokes[i].Hardness = 1000
	}
	base.Strokes[0].Weight = 8401
	if _, report, err := ExportProject(base, Version2, ExportOptions{}); ErrorCode(err) != CodeLossy {
		t.Fatalf("expected weight E_LOSSY, got %v", err)
	} else if !hasLoss(report.Losses, "weight_scale_truncation") {
		t.Fatalf("missing weight loss: %+v", report.Losses)
	}
	base.Strokes[0].Weight = 8400
	base.Strokes[0].Start.X = 11
	if _, report, err := ExportProject(base, Version2, ExportOptions{}); ErrorCode(err) != CodeLossy {
		t.Fatalf("expected coordinate E_LOSSY, got %v", err)
	} else if !hasLoss(report.Losses, "relative_coordinate_quantization") {
		t.Fatalf("missing coordinate loss: %+v", report.Losses)
	}
	v1Base := base.Clone()
	v1Base.Strokes[0].Start.X = 10
	if _, report, err := ExportProject(v1Base, Version1, ExportOptions{}); ErrorCode(err) != CodeLossy {
		t.Fatalf("expected v1 group E_LOSSY, got %v", err)
	} else if !hasLoss(report.Losses, "group_structure_not_expressible") {
		t.Fatalf("missing group loss: %+v", report.Losses)
	}
}

func hasLoss(losses []LossItem, code string) bool {
	for _, l := range losses {
		if l.Code == code {
			return true
		}
	}
	return false
}
