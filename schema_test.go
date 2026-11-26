package dialoguecal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestGeneratedProjectLoadsInAllThreeSchemas(t *testing.T) {
	g := mustGenerate(t, 42)
	if err := ValidateProject(g.Project); err != nil {
		t.Fatalf("generated project validation: %v", err)
	}
	want := g.Project.SemanticFingerprint()
	if want == "" {
		t.Fatal("empty semantic fingerprint")
	}
	for _, version := range []SchemaVersion{VersionV1, VersionV2, VersionV3} {
		data := g.Docs[version]
		if len(data) == 0 {
			t.Fatalf("missing generated v%d document", version)
		}
		p, err := LoadProject(data)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		if p.SourceVersion != version {
			t.Fatalf("source version: got %d want %d", p.SourceVersion, version)
		}
		if got := CanonicalSemantic(p); got != want {
			t.Fatalf("v%d semantic fingerprint mismatch: %s != %s", version, got, want)
		}
		if version <= VersionV2 && len(p.Defaults) == 0 {
			t.Fatalf("v%d did not record default fills", version)
		}
		if len(p.Conversions) == 0 {
			t.Fatalf("v%d did not record field conversions", version)
		}
		if len(p.Mappings) == 0 {
			t.Fatalf("v%d did not record source-to-internal field mappings", version)
		}
	}
}

func TestV1RoundTripHasZeroFieldResidual(t *testing.T) {
	g := mustGenerate(t, 99)
	rt, err := RoundTripProject(g.Project, VersionV1)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(rt.Residuals) != 0 {
		t.Fatalf("expected zero residuals, got %+v", rt.Residuals)
	}
	if rt.FingerprintBefore != rt.FingerprintAfter {
		t.Fatalf("fingerprint changed: %s != %s", rt.FingerprintBefore, rt.FingerprintAfter)
	}
	if len(rt.Export.Lossy) == 0 {
		t.Fatal("expected E_LOSSY entries for nested organization/markers")
	}
	foundOrganization := false
	foundMarker := false
	for _, issue := range rt.Export.Lossy {
		if issue.Code != CodeLossy || issue.FieldPath == "" || issue.Disposition == "" {
			t.Fatalf("malformed lossy issue: %+v", issue)
		}
		if strings.Contains(issue.FieldPath, "sceneGroups") {
			foundOrganization = true
		}
		if strings.Contains(issue.FieldPath, "calibration") {
			foundMarker = true
		}
	}
	if !foundOrganization || !foundMarker {
		t.Fatalf("lossy list did not cover organization and markers: %+v", rt.Export.Lossy)
	}
}

func TestV2RoundTripHasZeroFieldResidual(t *testing.T) {
	g := mustGenerate(t, 1001)
	rt, err := RoundTripProject(g.Project, VersionV2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Residuals) != 0 || rt.FingerprintBefore != rt.FingerprintAfter {
		t.Fatalf("v2 round trip not exact: residuals=%+v fingerprints=%s/%s",
			rt.Residuals, rt.FingerprintBefore, rt.FingerprintAfter)
	}
	if len(rt.Export.Lossy) == 0 {
		t.Fatal("v2 export should report scene-group flattening and marker omission")
	}
}

func TestSchemaUnknownVersionAndMissingFieldPaths(t *testing.T) {
	g := mustGenerate(t, 7)
	var root map[string]any
	if err := json.Unmarshal(g.Docs[VersionV2], &root); err != nil {
		t.Fatal(err)
	}
	root["schemaVersion"] = 99
	data, _ := json.Marshal(root)
	_, err := LoadProject(data)
	ke := requireCode(t, err, CodeSchema)
	if ke.Path != "$.schemaVersion" {
		t.Fatalf("unknown version path: %s", ke.Path)
	}

	if err := json.Unmarshal(g.Docs[VersionV2], &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "projectId")
	data, _ = json.Marshal(root)
	_, err = LoadProject(data)
	ke = requireCode(t, err, CodeSchema)
	if ke.Path != "$.projectId" {
		t.Fatalf("missing field path: %s", ke.Path)
	}
}

func TestDecodeOrderAndScaleValidationCodes(t *testing.T) {
	g := mustGenerate(t, 11)

	brokenCount := g.Project.Clone()
	brokenCount.Dialogues[0].PCM.DeclaredSampleCount++
	requireCode(t, ValidateProject(brokenCount), CodeDecode)

	brokenOrder := g.Project.Clone()
	ph := brokenOrder.Dialogues[0].Segments[0].Phonemes
	ph[0].StartSample, ph[1].StartSample = ph[1].StartSample, ph[0].StartSample
	requireCode(t, ValidateProject(brokenOrder), CodeOrder)

	brokenScale := g.Project.Clone()
	brokenScale.TargetLoudness = LoudnessCeiling + 1
	requireCode(t, ValidateProject(brokenScale), CodeCal)

	var root map[string]any
	if err := json.Unmarshal(g.Docs[VersionV3], &root); err != nil {
		t.Fatal(err)
	}
	groups := root["sceneGroups"].([]any)
	dialogues := groups[0].(map[string]any)["dialogues"].([]any)
	segments := dialogues[0].(map[string]any)["segments"].([]any)
	phonemes := segments[0].(map[string]any)["phonemes"].([]any)
	p0 := phonemes[0].(map[string]any)
	p1 := phonemes[1].(map[string]any)
	p0["startSample"], p1["startSample"] = p1["startSample"], p0["startSample"]
	data, _ := json.Marshal(root)
	_, err := LoadProject(data)
	requireCode(t, err, CodeOrder)
}

func TestUpgradeRecordsEveryDefaultFill(t *testing.T) {
	g := mustGenerate(t, 5)
	p, err := LoadProject(g.Docs[VersionV1])
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Defaults) == 0 {
		t.Fatal("v1 load did not record defaults")
	}
	for _, d := range p.Defaults {
		if d.FieldPath == "" || d.Source == "" || d.DefaultValue == "" {
			t.Fatalf("incomplete default trace: %+v", d)
		}
	}
	up, err := UpgradeProject(p, VersionV3)
	if err != nil {
		t.Fatal(err)
	}
	if up.SourceVersion != VersionV3 {
		t.Fatalf("upgrade source version: %d", up.SourceVersion)
	}
	if CanonicalSemantic(up) != CanonicalSemantic(g.Project) {
		t.Fatal("upgrade changed semantic fingerprint")
	}
}

func TestLoudUnreachableAndStageInjection(t *testing.T) {
	g := mustGenerate(t, 13)
	engine := NewEngine(nil)
	loud := g.Project.Clone()
	loud.TargetLoudness = LoudnessCeiling
	_, err := engine.Run(context.Background(), loud, RunOptions{Mode: ModeFull})
	ke := requireCode(t, err, CodeLoud)
	if ke.Target != LoudnessCeiling || ke.ReachableUpper >= ke.Target || ke.SegmentID == "" || !ke.HasFrame {
		t.Fatalf("E_LOUD locators incomplete: %+v", ke)
	}

	intervals, err := BuildIntervals(g.Project)
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Run(context.Background(), g.Project, RunOptions{
		Mode: ModeFull,
		Injection: Injection{
			FailSegmentID: intervals[0].SegmentID, FailFrame: intervals[0].StartFrame, HasFailFrame: true,
		},
	})
	ke = requireCode(t, err, CodeStage)
	if ke.SegmentID == "" || !ke.HasFrame {
		t.Fatalf("E_STAGE locators incomplete: %+v", ke)
	}
}

func TestInjectedInputFailuresUseDecodeOrderCalChains(t *testing.T) {
	g := mustGenerate(t, 17)
	engine := NewEngine(nil)
	_, err := engine.Run(context.Background(), g.Project, RunOptions{Mode: ModeFull, Injection: Injection{CorruptProject: true}})
	ke := requireCode(t, err, CodeDecode)
	if ke.SegmentID == "" || !ke.HasFrame {
		t.Fatalf("E_DECODE locator incomplete: %+v", ke)
	}
	_, err = engine.Run(context.Background(), g.Project, RunOptions{Mode: ModeFull, Injection: Injection{InvertPhoneme: true}})
	ke = requireCode(t, err, CodeOrder)
	if ke.SegmentID == "" || !ke.HasFrame {
		t.Fatalf("E_ORDER locator incomplete: %+v", ke)
	}
	_, err = engine.Run(context.Background(), g.Project, RunOptions{Mode: ModeFull, Injection: Injection{ContradictScale: true}})
	ke = requireCode(t, err, CodeCal)
	if ke.SegmentID == "" || !ke.HasFrame {
		t.Fatalf("E_CAL locator incomplete: %+v", ke)
	}
}

func TestDependencyGraphAndPhonemeFrameQuantization(t *testing.T) {
	g := mustGenerate(t, 23)
	graph, err := BuildDependencyGraph(g.Project)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) == 0 || len(graph.Edges) == 0 {
		t.Fatal("empty dependency graph")
	}
	kinds := make(map[string]int)
	for _, n := range graph.Nodes {
		kinds[n.Kind]++
	}
	for _, kind := range []string{"segment", "frame_feature", "calibration", "calibration_marker"} {
		if kinds[kind] == 0 {
			t.Fatalf("dependency graph missing %s node", kind)
		}
	}
	q, err := QuantizePhonemeBoundaries(g.Project)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) == 0 {
		t.Fatal("no quantized phoneme boundaries")
	}
	for _, b := range q {
		if b.EndFrame <= b.StartFrame {
			t.Fatalf("invalid quantized phoneme range: %+v", b)
		}
	}
}
